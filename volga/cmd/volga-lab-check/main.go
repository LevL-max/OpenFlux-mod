// Small, bounded test helper. It only listens on loopback and transfers a
// deterministic payload, with SHA-256 verified at the receiving side.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var output sync.Mutex

func event(kind string, data any) {
	output.Lock()
	defer output.Unlock()
	json.NewEncoder(os.Stdout).Encode(map[string]any{"time": time.Now().UTC(), "event": kind, "data": data})
}
func payload(n int64) io.Reader {
	seed := sha256.Sum256([]byte("stream-model-incompressible-v1"))
	return io.LimitReader(rand.NewChaCha8(seed), n)
}
func digest(n int64) string {
	h := sha256.New()
	io.Copy(h, payload(n))
	return hex.EncodeToString(h.Sum(nil))
}
func write(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, e := w.Write(p)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func origin(ctx context.Context) error {
	ln, e := net.Listen("tcp", "127.0.0.1:18765")
	if e != nil {
		return e
	}
	defer ln.Close()
	half, e := net.Listen("tcp", "127.0.0.1:18766")
	if e != nil {
		return e
	}
	defer half.Close()
	go func() {
		for {
			c, e := half.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(15 * time.Second))
				b, e := io.ReadAll(io.LimitReader(c, 4097))
				if e == nil && len(b) <= 4096 {
					h := sha256.Sum256(b)
					io.WriteString(c, hex.EncodeToString(h[:]))
				}
			}()
		}
	}()
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, "ready")
			return
		}
		n, e := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if e != nil || n < 1 || n > 50<<20 {
			http.Error(w, "invalid size", 400)
			return
		}
		switch r.URL.Path {
		case "/download":
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			io.Copy(w, payload(n))
		case "/upload":
			h := sha256.New()
			got, e := io.Copy(h, io.LimitReader(r.Body, n+1))
			sum := hex.EncodeToString(h.Sum(nil))
			if e != nil || got != n || sum != digest(n) {
				http.Error(w, "integrity", 400)
				return
			}
			fmt.Fprint(w, sum)
		default:
			http.NotFound(w, r)
		}
	})}
	stop := context.AfterFunc(ctx, func() { srv.Close(); half.Close() })
	defer stop()
	event("origin_ready", nil)
	return srv.Serve(ln)
}

type result struct {
	Label     string  `json:"label"`
	Bytes     int64   `json:"bytes"`
	Seconds   float64 `json:"seconds"`
	Mbps      float64 `json:"mbps"`
	SHA256    string  `json:"sha256"`
	Integrity bool    `json:"integrity"`
}

func transfer(ctx context.Context, hc *http.Client, label, direction string, n int64) (result, error) {
	r := result{Label: label, Bytes: n}
	want := digest(n)
	method := "GET"
	var body io.Reader
	if direction == "upload" {
		method = "POST"
		body = payload(n)
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:18765/"+direction+"?bytes="+strconv.FormatInt(n, 10), body)
	if body != nil {
		req.ContentLength = n
	}
	start := time.Now()
	resp, e := hc.Do(req)
	if e != nil {
		return r, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return r, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if direction == "download" {
		h := sha256.New()
		got, e := io.Copy(h, resp.Body)
		if e != nil {
			return r, e
		}
		if got != n {
			return r, errors.New("size mismatch")
		}
		r.SHA256 = hex.EncodeToString(h.Sum(nil))
	} else {
		p, e := io.ReadAll(io.LimitReader(resp.Body, 128))
		if e != nil {
			return r, e
		}
		r.SHA256 = string(p)
	}
	r.Seconds = time.Since(start).Seconds()
	r.Mbps = float64(n) * 8 / r.Seconds / 1e6
	r.Integrity = r.SHA256 == want
	if !r.Integrity {
		return r, errors.New("SHA-256 mismatch")
	}
	event("transfer", r)
	return r, nil
}
func connect(proxy, target string) (net.Conn, error) {
	c, e := net.DialTimeout("tcp", proxy, 5*time.Second)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if e = write(c, []byte{5, 1, 0}); e != nil {
		return nil, e
	}
	var m [2]byte
	if _, e = io.ReadFull(c, m[:]); e != nil || m != [2]byte{5, 0} {
		return nil, errors.New("SOCKS method failed")
	}
	h, p, e := net.SplitHostPort(target)
	if e != nil {
		return nil, e
	}
	port, _ := strconv.Atoi(p)
	b := append([]byte{5, 1, 0, 3, byte(len(h))}, []byte(h)...)
	b = binary.BigEndian.AppendUint16(b, uint16(port))
	if e = write(c, b); e != nil {
		return nil, e
	}
	var reply [10]byte
	if _, e = io.ReadFull(c, reply[:]); e != nil || reply[1] != 0 {
		return nil, errors.New("SOCKS connect failed")
	}
	ok = true
	return c, nil
}
func lifecycle(proxy string, hc *http.Client) error {
	c, e := connect(proxy, "127.0.0.1:18766")
	if e != nil {
		return e
	}
	msg := []byte("half-close reply after EOF")
	write(c, msg)
	c.(*net.TCPConn).CloseWrite()
	p, e := io.ReadAll(c)
	c.Close()
	want := sha256.Sum256(msg)
	if e != nil || string(p) != hex.EncodeToString(want[:]) {
		return errors.New("half-close integrity failed")
	}
	event("half_close_pass", nil)
	c, e = connect(proxy, "127.0.0.1:18765")
	if e != nil {
		return e
	}
	write(c, []byte("GET /download?bytes=52428800 HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"))
	_, e = bufio.NewReader(c).Peek(1)
	c.(*net.TCPConn).SetLinger(0)
	c.Close()
	if e != nil {
		return e
	}
	resp, e := hc.Get("http://127.0.0.1:18765/health")
	if e != nil {
		return e
	}
	p, e = io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || string(p) != "ready" {
		return errors.New("connection after abort failed")
	}
	event("new_connection_after_abort_pass", nil)
	return nil
}
func bench(ctx context.Context, proxy string, short bool) error {
	u, _ := url.Parse("socks5://" + proxy)
	ht := &http.Transport{Proxy: http.ProxyURL(u), DisableCompression: true, DisableKeepAlives: true}
	defer ht.CloseIdleConnections()
	hc := &http.Client{Transport: ht, Timeout: 65 * time.Second}
	if short {
		return lifecycle(proxy, hc)
	}
	var results []result
	for _, d := range []string{"download", "upload"} {
		r, e := transfer(ctx, hc, d, d, 50<<20)
		if e != nil {
			return e
		}
		results = append(results, r)
		if r.Mbps < 10 {
			return fmt.Errorf("acceptance below 10 Mbps: %.3f", r.Mbps)
		}
	}
	type answer struct {
		R result
		E error
	}
	done := make(chan answer, 2)
	start := time.Now()
	for i := 1; i <= 2; i++ {
		go func(i int) {
			r, e := transfer(ctx, hc, fmt.Sprintf("parallel_download_%d", i), "download", 25<<20)
			done <- answer{r, e}
		}(i)
	}
	for i := 0; i < 2; i++ {
		a := <-done
		if a.E != nil {
			return a.E
		}
		results = append(results, a.R)
	}
	seconds := time.Since(start).Seconds()
	mbps := float64(50<<20) * 8 / seconds / 1e6
	event("parallel_aggregate", map[string]any{"bytes": 50 << 20, "seconds": seconds, "mbps": mbps})
	if mbps < 10 {
		return fmt.Errorf("parallel aggregate below 10 Mbps: %.3f", mbps)
	}
	if e := lifecycle(proxy, hc); e != nil {
		return e
	}
	event("bench_pass", results)
	return nil
}

// endurance crosses the historical 200-300 MiB stall range without reopening
// the HTTP/SOCKS connection or restarting either Volga peer. It stops at the
// first speed/integrity failure and never runs more than six 50 MiB transfers.
func endurance(ctx context.Context, proxy string) error {
	u, _ := url.Parse("socks5://" + proxy)
	ht := &http.Transport{Proxy: http.ProxyURL(u), DisableCompression: true, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}
	defer ht.CloseIdleConnections()
	hc := &http.Client{Transport: ht, Timeout: 65 * time.Second}
	var mu sync.Mutex
	connections, reused := 0, 0
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		mu.Lock()
		defer mu.Unlock()
		if info.Reused {
			reused++
		} else {
			connections++
		}
	}}
	ctx = httptrace.WithClientTrace(ctx, trace)
	var results []result
	var total int64
	var seconds float64
	event("endurance_started", map[string]any{"rounds": 6, "bytes_per_round": 50 << 20, "expected_connections": 1})
	for round := 1; round <= 6; round++ {
		direction := "download"
		if round%2 == 0 {
			direction = "upload"
		}
		r, err := transfer(ctx, hc, fmt.Sprintf("round_%d_%s", round, direction), direction, 50<<20)
		if err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		results = append(results, r)
		total += r.Bytes
		seconds += r.Seconds
		mu.Lock()
		opened, reusedCount := connections, reused
		mu.Unlock()
		event("endurance_progress", map[string]any{"round": round, "verified_bytes": total, "connections_opened": opened, "connection_reuses": reusedCount})
		if opened != 1 || reusedCount != round-1 {
			return errors.New("endurance connection was replaced")
		}
		if r.Mbps < 10 {
			return fmt.Errorf("round %d below 10 Mbps: %.3f", round, r.Mbps)
		}
	}
	event("endurance_pass", map[string]any{"results": results, "bytes": total, "seconds": seconds, "mbps": float64(total) * 8 / seconds / 1e6,
		"connections_opened": connections, "connection_reuses": reused})
	return nil
}
func main() {
	mode := flag.String("mode", "bench", "origin, bench, lifecycle, or endurance")
	proxy := flag.String("proxy", "127.0.0.1:1088", "loopback SOCKS endpoint")
	flag.Parse()
	if !strings.HasPrefix(*proxy, "127.0.0.1:") {
		fmt.Fprintln(os.Stderr, "only loopback SOCKS allowed")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 6*time.Minute)
	defer timeout()
	var e error
	if *mode == "origin" {
		e = origin(ctx)
	} else if *mode == "bench" || *mode == "lifecycle" {
		e = bench(ctx, *proxy, *mode == "lifecycle")
	} else if *mode == "endurance" {
		e = endurance(ctx, *proxy)
	} else {
		e = errors.New("invalid mode")
	}
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		event("check_failed", e.Error())
		os.Exit(1)
	}
}
