//go:build volga

package yandex

// Isolated, single-target live feasibility diagnostic. Both TCP legs are local
// loopback sockets. Only the V6 carrier connects to an external service.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func streamLiveEvent(kind string, data any) {
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "event": kind, "data": data})
	fmt.Printf("V6STREAM %s\n", b)
}

func TestVolgaV6TerminatedTCPLive(t *testing.T) {
	role := os.Getenv("OPENFLUX_STREAM_ROLE")
	if role == "" {
		t.Skip("explicit isolated live diagnostic only")
	}
	if role != "client" && role != "server" {
		t.Fatal("invalid role")
	}
	rounds, err := strconv.Atoi(os.Getenv("OPENFLUX_STREAM_ROUNDS"))
	if err != nil || rounds < 1 || rounds > 3 {
		t.Fatal("1-3 rounds required")
	}
	mib, err := strconv.Atoi(os.Getenv("OPENFLUX_STREAM_MIB"))
	if err != nil || mib < 1 || mib > 50 {
		t.Fatal("1-50 MiB required")
	}
	size := int64(mib) << 20
	h := sha256.New()
	io.Copy(h, streamModelPayload(size))
	want := hex.EncodeToString(h.Sum(nil))
	ctx, cancel := context.WithTimeout(context.Background(), 390*time.Second)
	defer cancel()
	tr := socksLiveTransport(t)
	stream := newStreamModelAdapter(ctx, tr)
	var wg sync.WaitGroup
	goWork := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	var connections []net.Conn
	var origin *httptest.Server
	t.Cleanup(func() {
		cancel()
		for _, c := range connections {
			c.Close()
		}
		tr.Stop()
		wg.Wait()
		if origin != nil {
			origin.Close()
		}
	})
	goWork(stream.credits)
	goWork(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			stream.mu.Lock()
			data := map[string]any{"read_bytes": stream.readOffset, "write_bytes": stream.writeOffset, "peer_read_bytes": stream.peerRead, "buffered_bytes": stream.buffered, "peak_bytes": stream.peak, "window_bytes": streamModelWindow, "error": fmt.Sprint(stream.err)}
			stream.mu.Unlock()
			streamLiveEvent("stream_snapshot", data)
		}
	})
	bridge := func(c net.Conn) {
		goWork(func() {
			_, e := io.Copy(stream, c)
			if ctx.Err() == nil && e != nil {
				streamLiveEvent("bridge_to_stream_error", e.Error())
			}
		})
		goWork(func() {
			_, e := io.Copy(c, stream)
			if ctx.Err() == nil && e != nil {
				streamLiveEvent("bridge_from_stream_error", e.Error())
			}
		})
	}
	if role == "server" {
		done := make(chan struct{}, 1)
		origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			round, _ := strconv.Atoi(r.URL.Query().Get("round"))
			if r.URL.Path != "/done" && r.URL.Path != "/health" && (round < 1 || round > rounds) {
				http.Error(w, "invalid round", 400)
				return
			}
			switch r.URL.Path {
			case "/health":
				fmt.Fprint(w, "ready")
			case "/download":
				start := time.Now()
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("X-SHA256", want)
				n, e := io.Copy(w, streamModelPayload(size))
				streamLiveEvent("origin_download", map[string]any{"round": round, "bytes": n, "seconds": time.Since(start).Seconds(), "error": fmt.Sprint(e)})
			case "/upload":
				start := time.Now()
				hh := sha256.New()
				n, e := io.Copy(hh, io.LimitReader(r.Body, size+1))
				got := hex.EncodeToString(hh.Sum(nil))
				ok := e == nil && n == size && got == want
				streamLiveEvent("origin_upload", map[string]any{"round": round, "bytes": n, "seconds": time.Since(start).Seconds(), "sha256": got, "integrity": ok, "error": fmt.Sprint(e)})
				if !ok {
					http.Error(w, "integrity failed", 400)
					return
				}
				fmt.Fprint(w, got)
			case "/done":
				fmt.Fprint(w, "done")
				select {
				case done <- struct{}{}:
				default:
				}
			default:
				http.NotFound(w, r)
			}
		}))
		remote, e := net.DialTimeout("tcp", origin.Listener.Addr().String(), time.Second)
		if e != nil {
			t.Fatal(e)
		}
		connections = append(connections, remote)
		bridge(remote)
		if e = os.WriteFile("/run/openflux-audit-20260918/server-ready", []byte("ready"), 0600); e != nil {
			t.Fatal(e)
		}
		streamLiveEvent("server_ready", map[string]any{"rounds": rounds, "bytes_per_direction": size, "tcp_leg": "owned loopback origin"})
		select {
		case <-done:
			time.Sleep(3 * time.Second)
			streamLiveEvent("server_complete", true)
		case <-ctx.Done():
			t.Fatal("server deadline")
		}
		return
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	client, e := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	connections = append(connections, client)
	local, e := ln.Accept()
	if e != nil {
		t.Fatal(e)
	}
	connections = append(connections, local)
	ln.Close()
	bridge(local)
	var dialMu sync.Mutex
	used := false
	ht := &http.Transport{DisableCompression: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dialMu.Lock()
		defer dialMu.Unlock()
		if used {
			return nil, errors.New("single-stream diagnostic cannot reconnect")
		}
		used = true
		return client, nil
	}}
	defer ht.CloseIdleConnections()
	hc := &http.Client{Transport: ht, Timeout: 75 * time.Second}
	const url = "http://volga-stream.test"
	resp, e := hc.Get(url + "/health")
	if e != nil {
		t.Fatal(e)
	}
	_, e = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if e != nil || resp.StatusCode != 200 {
		t.Fatal("health", e)
	}
	streamLiveEvent("client_connected", map[string]any{"rounds": rounds, "bytes_per_direction": size, "compression": false, "same_connection": true})
	var total int64
	for round := 1; round <= rounds; round++ {
		for _, direction := range []string{"download", "upload"} {
			streamLiveEvent("transfer_start", map[string]any{"round": round, "direction": direction, "bytes": size, "previous_completed_bytes": total})
			start := time.Now()
			var req *http.Request
			if direction == "download" {
				req, _ = http.NewRequestWithContext(ctx, "GET", url+"/download?round="+strconv.Itoa(round), nil)
			} else {
				req, _ = http.NewRequestWithContext(ctx, "POST", url+"/upload?round="+strconv.Itoa(round), streamModelPayload(size))
				req.ContentLength = size
			}
			resp, e = hc.Do(req)
			if e != nil {
				streamLiveEvent("transfer_failed", map[string]any{"round": round, "direction": direction, "seconds": time.Since(start).Seconds(), "error": e.Error()})
				t.Fatal(e)
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				t.Fatalf("HTTP %d", resp.StatusCode)
			}
			if direction == "download" {
				h.Reset()
				n, err := io.Copy(h, resp.Body)
				resp.Body.Close()
				got := hex.EncodeToString(h.Sum(nil))
				if err != nil || n != size || got != want {
					streamLiveEvent("transfer_failed", map[string]any{"round": round, "direction": direction, "received_bytes": n, "seconds": time.Since(start).Seconds(), "error": fmt.Sprint(err)})
					t.Fatal("download integrity", n, err)
				}
			} else {
				answer, err := io.ReadAll(io.LimitReader(resp.Body, 128))
				resp.Body.Close()
				if err != nil || string(answer) != want {
					t.Fatal("upload integrity", err)
				}
			}
			seconds := time.Since(start).Seconds()
			total += size
			streamLiveEvent("transfer_complete", map[string]any{"round": round, "direction": direction, "bytes": size, "seconds": seconds, "payload_mbps": float64(size) * 8 / seconds / 1e6, "sha256": want, "cumulative_bytes": total})
		}
	}
	resp, e = hc.Post(url+"/done", "text/plain", nil)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	streamLiveEvent("client_complete", map[string]any{"transfers": rounds * 2, "total_bytes": total})
}
