//go:build volga

package yandex

// Single-stream local feasibility probe. No live provider, authentication,
// multiplexing, remote dialing protocol, or production integration is added.
import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

const streamModelWindow = 512 << 10
const streamModelChunk = 5600 // mirrors recordconn.Chunk in the lab module

type streamModelAdapter struct {
	ctx                               context.Context
	tr                                *YandexVolgaV6Transport
	mu                                sync.Mutex
	writeMu                           sync.Mutex
	changed                           chan struct{}
	pending                           map[uint64][]byte
	readOffset, writeOffset, peerRead uint64
	buffered, peak                    int
	err                               error
}

func newStreamModelAdapter(ctx context.Context, tr *YandexVolgaV6Transport) *streamModelAdapter {
	s := &streamModelAdapter{ctx: ctx, tr: tr, changed: make(chan struct{}), pending: make(map[uint64][]byte)}
	tr.Receive(s.receive)
	return s
}
func (s *streamModelAdapter) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }
func (s *streamModelAdapter) receive(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) < 16 || string(p[:4]) != "SV61" {
		s.err = errors.New("invalid model record")
		s.notifyLocked()
		return
	}
	off := binary.BigEndian.Uint64(p[8:16])
	switch p[4] {
	case 'A':
		if off > s.writeOffset {
			s.err = errors.New("invalid credit")
		} else if off > s.peerRead {
			s.peerRead = off
		}
	case 'D':
		if len(p) == 16 || len(p) > 16+streamModelChunk || off+uint64(len(p)-16) > s.readOffset+streamModelWindow {
			s.err = errors.New("receive window exceeded")
			break
		}
		if off < s.readOffset {
			s.err = errors.New("unexpected stream duplicate")
			break
		}
		if _, ok := s.pending[off]; ok {
			break
		}
		s.pending[off] = append([]byte(nil), p[16:]...)
		s.buffered += len(p) - 16
		s.peak = max(s.peak, s.buffered)
	default:
		s.err = errors.New("invalid model kind")
	}
	s.notifyLocked()
}
func (s *streamModelAdapter) send(kind byte, off uint64, p []byte) error {
	record := make([]byte, 16+len(p))
	copy(record, "SV61")
	record[4] = kind
	binary.BigEndian.PutUint64(record[8:16], off)
	copy(record[16:], p)
	for {
		if err := s.tr.Send(record); err == nil {
			return nil
		} else if !errors.Is(err, errVolgaV6TransportQueueFull) {
			return err
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}
func (s *streamModelAdapter) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), streamModelChunk)
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return written, err
		}
		if s.writeOffset+uint64(n) > s.peerRead+streamModelWindow {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return written, s.ctx.Err()
			case <-changed:
			}
			continue
		}
		off := s.writeOffset
		s.writeOffset += uint64(n)
		s.mu.Unlock()
		if err := s.send('D', off, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
func (s *streamModelAdapter) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return 0, err
		}
		if data, ok := s.pending[s.readOffset]; ok {
			delete(s.pending, s.readOffset)
			n := copy(p, data)
			s.readOffset += uint64(n)
			s.buffered -= n
			if n < len(data) {
				s.pending[s.readOffset] = data[n:]
			}
			s.mu.Unlock()
			return n, nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		case <-changed:
		}
	}
}
func (s *streamModelAdapter) credits() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var previous uint64
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		n := s.readOffset
		s.mu.Unlock()
		if n != previous {
			if s.send('A', n, nil) != nil {
				return
			}
			previous = n
		}
	}
}
func streamModelPayload(size int64) io.Reader {
	seed := sha256.Sum256([]byte("stream-model-incompressible-v1"))
	return io.LimitReader(rand.NewChaCha8(seed), size)
}

func TestVolgaV6StreamModelBounds(t *testing.T) {
	carrier := newOfflineVolgaV6YandexCarrier(t, 1, 8000)
	defer carrier.Stop()
	carrier.config.RelayEnvelope = "minimal"
	payload := [][]byte{make([]byte, streamModelChunk+16)}
	// Worst permitted batch accompanying one full stream record with credits.
	for len(payload[0])+(len(payload)-1)*16+16 <= 5000 {
		payload = append(payload, make([]byte, 16))
	}
	records, ok := encodeVolgaV6Records(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 1, Seq: 1, Floor: 1, Payload: payload})
	if !ok {
		t.Fatal("invalid frame")
	}
	body, err := carrier.buildRelayBody(records)
	if err != nil || len(body) > 8000 {
		t.Fatal("stream batch would fragment", len(body), err)
	}
	t.Logf("maximum configured stream batch: HTTP body=%d, limit=8000", len(body))

	ctx, cancel := context.WithCancel(context.Background())
	s := &streamModelAdapter{ctx: ctx, changed: make(chan struct{}), pending: make(map[uint64][]byte)}
	readDone := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 1)); readDone <- err }()
	cancel()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left a blocked reader")
	}
	bad := make([]byte, 17)
	copy(bad, "SV61")
	bad[4] = 'D'
	binary.BigEndian.PutUint64(bad[8:16], streamModelWindow)
	s.receive(bad)
	if s.err == nil || s.buffered != 0 {
		t.Fatal("out-of-window payload retained")
	}
}

func TestVolgaV6TerminatedTCPStreamModel(t *testing.T) {
	if os.Getenv("OPENFLUX_OFFLINE_STREAM_MODEL") != "1" {
		t.Skip("explicit local feasibility diagnostic only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	ca := &tcpModelCarrier{ctx: ctx, gate: volgaV6RelayGate{rate: 360}}
	cb := &tcpModelCarrier{ctx: ctx, gate: volgaV6RelayGate{rate: 360}}
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.Telemetry = false
	cfg.QueueSize, cfg.SendQueueSize, cfg.SendWorkers = 512, 64, 64
	a := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) { return ca, nil })
	b := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) { return cb, nil })
	ca.peer, cb.peer = b.runtime, a.runtime
	sa, sb := newStreamModelAdapter(ctx, a), newStreamModelAdapter(ctx, b)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	goWork := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	goWork(sa.credits)
	goWork(sb.credits)
	var connections []net.Conn
	var origin *httptest.Server
	t.Cleanup(func() {
		cancel()
		for _, c := range connections {
			c.Close()
		}
		a.Stop()
		b.Stop()
		wg.Wait()
		if origin != nil {
			origin.Close()
		}
	})
	const size int64 = 8 << 20
	h := sha256.New()
	io.Copy(h, streamModelPayload(size))
	want := hex.EncodeToString(h.Sum(nil))
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			io.Copy(w, streamModelPayload(size))
		case "/upload":
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(r.Body, size+1))
			got := hex.EncodeToString(h.Sum(nil))
			if err != nil || n != size || got != want {
				http.Error(w, "integrity failure", 400)
				return
			}
			fmt.Fprint(w, got)
		default:
			http.NotFound(w, r)
		}
	}))
	// Two real loopback TCP legs; only stream records traverse the delayed
	// carrier. The client leg is preconnected, so no SOCKS/DIAL protocol is
	// being claimed by this single-target feasibility probe.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	connections = append(connections, client)
	local, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	connections = append(connections, local)
	ln.Close()
	remote, err := net.Dial("tcp", origin.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	connections = append(connections, remote)
	goWork(func() { io.Copy(sa, local) })
	goWork(func() { io.Copy(local, sa) })
	goWork(func() { io.Copy(sb, remote) })
	goWork(func() { io.Copy(remote, sb) })
	var dialMu sync.Mutex
	used := false
	ht := &http.Transport{DisableCompression: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dialMu.Lock()
		defer dialMu.Unlock()
		if used {
			return nil, errors.New("only one stream allowed")
		}
		used = true
		return client, nil
	}}
	defer ht.CloseIdleConnections()
	hc := &http.Client{Transport: ht, Timeout: 12 * time.Second}
	for _, direction := range []string{"download", "upload"} {
		start := time.Now()
		var req *http.Request
		if direction == "download" {
			req, _ = http.NewRequestWithContext(ctx, "GET", origin.URL+"/download", nil)
		} else {
			req, _ = http.NewRequestWithContext(ctx, "POST", origin.URL+"/upload", streamModelPayload(size))
			req.ContentLength = size
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("HTTP %d", resp.StatusCode)
		}
		if direction == "download" {
			h.Reset()
			n, err := io.Copy(h, resp.Body)
			resp.Body.Close()
			if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != want {
				t.Fatal("download integrity", n, err)
			}
		} else {
			body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
			resp.Body.Close()
			if err != nil || string(body) != want {
				t.Fatal("upload integrity", err)
			}
		}
		seconds := time.Since(start).Seconds()
		rate := float64(size) * 8 / seconds / 1e6
		fmt.Printf("V6STREAMMODEL direction=%s bytes=%d seconds=%.3f payload_mbps=%.3f sha256=%s\n", direction, size, seconds, rate, want)
		if rate < 10 {
			t.Errorf("local feasibility gate failed: %.3f Mbps < 10", rate)
		}
	}
	for _, s := range []*streamModelAdapter{sa, sb} {
		s.mu.Lock()
		peak := s.peak
		err := s.err
		s.mu.Unlock()
		fmt.Printf("V6STREAMMODEL receive_peak_bytes=%d window_bytes=%d\n", peak, streamModelWindow)
		if err != nil || peak > streamModelWindow {
			t.Fatal("bounded stream failed", peak, err)
		}
	}
}
