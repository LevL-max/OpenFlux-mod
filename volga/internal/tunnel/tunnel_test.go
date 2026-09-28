package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openflux-volga-lab/internal/recordconn"
)

type fixture struct {
	ctx                        context.Context
	client, server             *Endpoint
	proxy                      string
	clientEvents, serverEvents chan string
}

func setup(t *testing.T, allowed []string, idle time.Duration) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	key := sha256.Sum256([]byte("local integration key"))
	qa, qb := make(chan []byte, 256), make(chan []byte, 256)
	send := func(q chan []byte) recordconn.SendFunc {
		return func(ctx context.Context, p []byte) error {
			select {
			case q <- p:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	a, b := NewEndpoint(true, key[:], send(qb)), NewEndpoint(false, key[:], send(qa))
	a.Idle = idle
	b.Idle = idle
	b.Allowed = make(map[string]bool)
	for _, s := range allowed {
		b.Allowed[s] = true
	}
	f := &fixture{ctx: ctx, client: a, server: b, clientEvents: make(chan string, 100), serverEvents: make(chan string, 100)}
	a.Event = func(s string) { f.clientEvents <- s }
	b.Event = func(s string) { f.serverEvents <- s }
	var wg sync.WaitGroup
	start := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	start(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-qa:
				a.Receive(p)
			}
		}
	})
	start(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-qb:
				b.Receive(p)
			}
		}
	})
	start(func() { a.Run(ctx) })
	start(func() { b.Run(ctx) })
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		cancel()
		t.Fatal(e)
	}
	f.proxy = ln.Addr().String()
	start(func() { ServeSOCKS(ctx, ln, a.Open, idle) })
	t.Cleanup(func() {
		cancel()
		ln.Close()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fixture did not shut down")
		}
	})
	f.ready(t)
	return f
}
func (f *fixture) ready(t *testing.T) {
	t.Helper()
	for _, ch := range []chan string{f.clientEvents, f.serverEvents} {
		for {
			select {
			case s := <-ch:
				if s == "session_ready" {
					goto next
				}
			case <-f.ctx.Done():
				t.Fatal("session readiness timeout")
			}
		}
	next:
	}
}

func socksConnect(proxy, target string, fragment bool) (net.Conn, error) {
	c, e := net.DialTimeout("tcp", proxy, time.Second)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	write := func(p []byte) error {
		if !fragment {
			return WriteFull(c, p)
		}
		for _, v := range p {
			if e := WriteFull(c, []byte{v}); e != nil {
				return e
			}
		}
		return nil
	}
	if e = write([]byte{5, 2, 2, 0}); e != nil {
		return nil, e
	}
	var ack [2]byte
	if _, e = io.ReadFull(c, ack[:]); e != nil {
		return nil, e
	}
	if ack != [2]byte{5, 0} {
		return nil, fmt.Errorf("SOCKS auth: %v", ack)
	}
	h, port, e := net.SplitHostPort(target)
	if e != nil {
		return nil, e
	}
	n, _ := strconv.Atoi(port)
	request := append([]byte{5, 1, 0, 3, byte(len(h))}, []byte(h)...)
	request = binary.BigEndian.AppendUint16(request, uint16(n))
	if e = write(request); e != nil {
		return nil, e
	}
	var response [10]byte
	if _, e = io.ReadFull(c, response[:]); e != nil {
		return nil, e
	}
	if response[1] != 0 {
		return nil, fmt.Errorf("SOCKS remote failure %d", response[1])
	}
	c.SetDeadline(time.Time{})
	ok = true
	return c, nil
}

func TestParallelHTTPIntegrity(t *testing.T) {
	data := bytes.Repeat([]byte("deterministic local HTTP data/"), 40000)
	want := sha256.Sum256(data)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write(data)
			return
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(r.Body, int64(len(data)+1)))
		if e != nil || n != int64(len(data)) || !bytes.Equal(h.Sum(nil), want[:]) {
			http.Error(w, "integrity", 400)
			return
		}
		fmt.Fprint(w, hex.EncodeToString(h.Sum(nil)))
	}))
	defer origin.Close()
	f := setup(t, []string{origin.Listener.Addr().String()}, time.Second)
	ht := &http.Transport{DisableKeepAlives: true, DialContext: func(_ context.Context, _, target string) (net.Conn, error) {
		return socksConnect(f.proxy, target, true)
	}}
	defer ht.CloseIdleConnections()
	hc := &http.Client{Transport: ht, Timeout: 5 * time.Second}
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(i int) {
			method := "GET"
			var body io.Reader
			if i%2 == 1 {
				method = "POST"
				body = bytes.NewReader(data)
			}
			req, _ := http.NewRequestWithContext(f.ctx, method, origin.URL, body)
			resp, e := hc.Do(req)
			if e != nil {
				done <- e
				return
			}
			defer resp.Body.Close()
			got, e := io.ReadAll(resp.Body)
			if e == nil && resp.StatusCode != 200 {
				e = errors.New("bad HTTP status")
			}
			if e == nil {
				if method == "GET" {
					if sha256.Sum256(got) != want {
						e = errors.New("download hash mismatch")
					}
				} else if string(got) != hex.EncodeToString(want[:]) {
					e = errors.New("upload hash mismatch")
				}
			}
			done <- e
		}(i)
	}
	for i := 0; i < 4; i++ {
		if e := <-done; e != nil {
			t.Error(e)
		}
	}
}

func halfCloseOrigin(t *testing.T) (net.Listener, *atomic.Int32) {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	active := &atomic.Int32{}
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			active.Add(1)
			go func() {
				defer active.Add(-1)
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				h := sha256.New()
				io.Copy(h, io.LimitReader(c, 2<<20))
				io.WriteString(c, hex.EncodeToString(h.Sum(nil)))
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln, active
}

func TestHalfCloseKeepsResponseReadable(t *testing.T) {
	ln, _ := halfCloseOrigin(t)
	f := setup(t, []string{ln.Addr().String()}, time.Second)
	c, e := socksConnect(f.proxy, ln.Addr().String(), true)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	p := bytes.Repeat([]byte("after EOF response"), 30000)
	want := sha256.Sum256(p)
	if e = WriteFull(c, p); e != nil {
		t.Fatal(e)
	}
	if e = c.(*net.TCPConn).CloseWrite(); e != nil {
		t.Fatal(e)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, e := io.ReadAll(c)
	if e != nil || string(got) != hex.EncodeToString(want[:]) {
		t.Fatalf("half-close response failed: bytes=%d error=%v", len(got), e)
	}
}

func TestDeniedTargetAndSOCKSMethod(t *testing.T) {
	f := setup(t, nil, time.Second)
	if c, e := socksConnect(f.proxy, "127.0.0.1:1", false); e == nil {
		c.Close()
		t.Fatal("unlisted target allowed")
	}
	c, e := net.Dial("tcp", f.proxy)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	WriteFull(c, []byte{5, 1, 2})
	var p [2]byte
	if _, e = io.ReadFull(c, p[:]); e != nil || p != [2]byte{5, 255} {
		t.Fatal("unsupported method accepted", p, e)
	}
}

func TestIdleStreamCleanup(t *testing.T) {
	ln, active := halfCloseOrigin(t)
	f := setup(t, []string{ln.Addr().String()}, 100*time.Millisecond)
	c, e := socksConnect(f.proxy, ln.Addr().String(), false)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, c)
	deadline := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 {
		t.Fatal("idle origin connection remained open")
	}
}

func TestFreshConnectionsAfterSessionRestart(t *testing.T) {
	ln, _ := halfCloseOrigin(t)
	f := setup(t, []string{ln.Addr().String()}, time.Second)
	f.client.mu.Lock()
	old := f.client.mux
	f.client.mu.Unlock()
	old.Close()
	f.ready(t)
	c, e := socksConnect(f.proxy, ln.Addr().String(), false)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	WriteFull(c, []byte("reconnected"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	p, e := io.ReadAll(c)
	want := sha256.Sum256([]byte("reconnected"))
	if e != nil || string(p) != hex.EncodeToString(want[:]) {
		t.Fatal("restart integrity failed", e)
	}
}
