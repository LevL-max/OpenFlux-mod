//go:build volga

package yandex

// Linux-only application-path diagnostic. The production SOCKS, gVisor and raw
// exit implementations are reused unchanged. A Unix packet socket separates the
// raw exit's network namespace from the Volga carrier's ordinary HTTPS network.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/tunnel"
)

const socksBridgePath = "/run/openflux-audit-20260918/exit.sock"
const socksOrigin = "192.0.2.2:18080"

type socksPacketBridge struct {
	*transport.BaseTransport
	conn *net.UnixConn
}

func (b *socksPacketBridge) Send(p []byte) error { _, err := b.conn.Write(p); return err }
func (b *socksPacketBridge) Stop() error         { return b.conn.Close() }
func (b *socksPacketBridge) pump() error {
	buf := make([]byte, 65535)
	for {
		n, err := b.conn.Read(buf)
		if err != nil {
			return err
		}
		b.CallReceive(append([]byte(nil), buf[:n]...))
	}
}

func socksEvent(kind string, data any) {
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "event": kind, "data": data})
	fmt.Printf("V6SOCKS %s\n", b)
}

func socksPayload(size int64) io.Reader {
	seed := sha256.Sum256([]byte("openflux-socks-20260922-incompressible"))
	return io.LimitReader(rand.NewChaCha8(seed), size)
}

func TestVolgaV6SOCKSApplication(t *testing.T) {
	mode := os.Getenv("OPENFLUX_SOCKS_MODE")
	if mode == "" {
		t.Skip("explicit isolated application diagnostic only")
	}
	mib, err := strconv.Atoi(os.Getenv("OPENFLUX_SOCKS_MIB"))
	if err != nil || mib < 1 || mib > 50 {
		t.Fatal("size must be 1-50 MiB")
	}
	size := int64(mib) << 20
	h := sha256.New()
	_, _ = io.Copy(h, socksPayload(size))
	want := hex.EncodeToString(h.Sum(nil))
	if mode == "origin" {
		done := make(chan struct{}, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ready") })
		mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("X-SHA256", want)
			n, err := io.Copy(w, socksPayload(size))
			socksEvent("origin_download", map[string]any{"bytes": n, "error": fmt.Sprint(err)})
		})
		mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(r.Body, size+1))
			got := hex.EncodeToString(h.Sum(nil))
			if err != nil || n != size || got != want {
				http.Error(w, "integrity failed", 400)
				return
			}
			socksEvent("origin_upload", map[string]any{"bytes": n, "sha256": got})
			fmt.Fprint(w, got)
		})
		mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "done")
			select {
			case done <- struct{}{}:
			default:
			}
		})
		srv := &http.Server{Addr: socksOrigin, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		listener, err := net.Listen("tcp", socksOrigin)
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		go srv.Serve(listener)
		socksEvent("origin_ready", socksOrigin)
		select {
		case <-done:
			time.Sleep(time.Second)
		case <-time.After(4 * time.Minute):
			t.Fatal("origin deadline")
		}
		return
	}
	if mode == "exit" {
		conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socksBridgePath, Net: "unixpacket"})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		bridge := &socksPacketBridge{BaseTransport: transport.NewBaseTransport(transport.DefaultConfig()), conn: conn}
		tunnel.NewTCPTunnel(bridge, true, 262144, 1048576)
		socksEvent("exit_ready", "production raw socket exit in isolated network namespace")
		if err := bridge.pump(); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		return
	}
	if mode != "server" && mode != "client" && mode != "local" {
		t.Fatal("invalid mode")
	}
	var tr transport.Transport
	if mode == "local" || mode == "server" {
		listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socksBridgePath, Net: "unixpacket"})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		_ = listener.SetDeadline(time.Now().Add(40 * time.Second))
		socksEvent("bridge_ready", mode)
		conn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		bridge := &socksPacketBridge{BaseTransport: transport.NewBaseTransport(transport.DefaultConfig()), conn: conn}
		if mode == "local" {
			tr = bridge
			go bridge.pump()
		} else {
			v6 := socksLiveTransport(t)
			defer v6.Stop()
			v6.Receive(func(p []byte) {
				if err := bridge.Send(p); err != nil {
					socksEvent("bridge_write_error", err.Error())
				}
			})
			bridge.Receive(func(p []byte) {
				deadline := time.Now().Add(5 * time.Second)
				for v6.Send(p) != nil {
					if time.Now().After(deadline) {
						socksEvent("bridge_backpressure_timeout", len(p))
						return
					}
					time.Sleep(time.Millisecond)
				}
			})
			socksEvent("server_ready", "four authorizations")
			if err := os.WriteFile("/run/openflux-audit-20260918/server-ready", []byte("ready"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := bridge.pump(); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			return
		}
	} else {
		tr = socksLiveTransport(t)
		defer tr.Stop()
	}
	tun := tunnel.NewTCPTunnel(tr, false, 262144, 1048576)
	addr := os.Getenv("OPENFLUX_SOCKS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:19080"
	}
	go func() {
		if err := socks5.NewSOCKS5Server(addr, tun).Start(); err != nil {
			socksEvent("socks_listener_error", err.Error())
		}
	}()
	httpTransport := &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		var c net.Conn
		var err error
		for i := 0; i < 30; i++ {
			c, err = (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			return nil, err
		}
		ok := false
		defer func() {
			if !ok {
				c.Close()
			}
		}()
		_ = c.SetDeadline(time.Now().Add(25 * time.Second))
		if _, err = c.Write([]byte{5, 1, 0}); err != nil {
			return nil, err
		}
		var auth [2]byte
		if _, err = io.ReadFull(c, auth[:]); err != nil {
			return nil, err
		}
		if auth != [2]byte{5, 0} {
			return nil, fmt.Errorf("SOCKS auth %v", auth)
		}
		// Fixed target exists only inside the owned server-side namespace.
		if _, err = c.Write([]byte{5, 1, 0, 1, 192, 0, 2, 2, 70, 160}); err != nil {
			return nil, err
		}
		var reply [10]byte
		if _, err = io.ReadFull(c, reply[:]); err != nil {
			return nil, err
		}
		if reply[1] != 0 {
			return nil, fmt.Errorf("SOCKS CONNECT reply %v", reply)
		}
		_ = c.SetDeadline(time.Time{})
		ok = true
		return c, nil
	}}
	defer httpTransport.CloseIdleConnections()
	hc := &http.Client{Transport: httpTransport, Timeout: 90 * time.Second}
	resp, err := hc.Get("http://" + socksOrigin + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("health failed", err)
	}
	socksEvent("socks_connected", map[string]any{"bytes_per_direction": size, "compression": false})
	start := time.Now()
	resp, err = hc.Get("http://" + socksOrigin + "/download")
	if err != nil {
		t.Fatal(err)
	}
	h.Reset()
	n, err := io.Copy(h, resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start).Seconds()
	got := hex.EncodeToString(h.Sum(nil))
	if err != nil || resp.StatusCode != 200 || n != size || got != want {
		t.Fatalf("download failed bytes=%d hash=%s err=%v", n, got, err)
	}
	socksEvent("download_complete", map[string]any{"bytes": n, "seconds": elapsed, "payload_mbps": float64(n) * 8 / elapsed / 1e6, "sha256": got})
	start = time.Now()
	req, _ := http.NewRequest("POST", "http://"+socksOrigin+"/upload", socksPayload(size))
	req.ContentLength = size
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	elapsed = time.Since(start).Seconds()
	if err != nil || resp.StatusCode != 200 || string(answer) != want {
		t.Fatal("upload failed", resp.StatusCode, string(answer), err)
	}
	socksEvent("upload_complete", map[string]any{"bytes": size, "seconds": elapsed, "payload_mbps": float64(size) * 8 / elapsed / 1e6, "sha256": want})
	resp, err = hc.Post("http://"+socksOrigin+"/done", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func socksLiveTransport(t *testing.T) *YandexVolgaV6Transport {
	docs := strings.Split(os.Getenv("OPENFLUX_V6_POOL"), ",")
	if len(docs) != 4 {
		t.Fatal("four explicit pool entries required")
	}
	cfg := DefaultVolgaV6TransportConfig(docs)
	cfg.Telemetry = false
	cfg.QueueSize = 512
	cfg.SendQueueSize = 64
	cfg.SendWorkers = 64
	cfg.Yandex.HTTPProtocol = "http1"
	cfg.Yandex.RelayEnvelope = "minimal"
	cfg.Yandex.RelayPostsPerSecond = 360
	cfg.Yandex.authorize = volgaV6DiagnosticAuth(t, docs).authorize
	cfg.Runtime.CarrierStartTimeout = 30 * time.Second
	tr := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, livePoolFactory(docs, cfg.Yandex))
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tr.ctx.Done():
				return
			case now := <-ticker.C:
				s := tr.Snapshot(now)
				s.Carrier.ActiveHealth.Document = ""
				socksEvent("transport_snapshot", s)
			}
		}
	}()
	return tr
}
