package tunnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"
)

func TestStreamLimitValidation(t *testing.T) {
	for _, n := range []int{0, 1, 16, 32, 64} {
		got, err := StreamLimit(n)
		if err != nil || got < 1 || got > 64 {
			t.Fatal(n, got, err)
		}
	}
	for _, n := range []int{-1, 65, 1000000} {
		if _, err := StreamLimit(n); err == nil {
			t.Fatal("unbounded limit accepted", n)
		}
	}
	if MuxConfig().MaxStreamWindowSize != 2<<20 {
		t.Fatal("per-stream window changed")
	}
}

func Test64ConcurrentSOCKSStreamsIntegrityAndAdmission(t *testing.T) {
	ln, active := halfCloseOrigin(t)
	f := setupWithLimits(t, []string{ln.Addr().String()}, 10*time.Second, 64, 64)
	connections := make([]net.Conn, 0, 64)
	defer func() {
		for _, c := range connections {
			c.Close()
		}
	}()
	for i := 0; i < 64; i++ {
		c, err := socksConnect(f.proxy, ln.Addr().String(), false)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		connections = append(connections, c)
	}
	if got := active.Load(); got != 64 {
		t.Fatalf("wanted 64 simultaneous origins, got %d", got)
	}
	if c, err := socksConnect(f.proxy, ln.Addr().String(), false); err == nil {
		c.Close()
		t.Fatal("65th SOCKS connection admitted")
	}
	if s, err := f.client.Open(f.ctx); err == nil {
		s.Close()
		t.Fatal("Endpoint.Open bypassed limit")
	}
	payload := bytes.Repeat([]byte("64-stream integrity"), 1024)
	want := sha256.Sum256(payload)
	done := make(chan error, len(connections))
	for _, c := range connections {
		go func(c net.Conn) {
			c.SetDeadline(time.Now().Add(8 * time.Second))
			err := WriteFull(c, payload)
			if err == nil {
				err = c.(*net.TCPConn).CloseWrite()
			}
			var got []byte
			if err == nil {
				got, err = io.ReadAll(c)
			}
			if err == nil && string(got) != hex.EncodeToString(want[:]) {
				err = io.ErrUnexpectedEOF
			}
			c.Close()
			done <- err
		}(c)
	}
	for range connections {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.client.mu.Lock()
		n := f.client.mux.NumStreams()
		f.client.mu.Unlock()
		if n == 0 && active.Load() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 {
		t.Fatal("origin slots leaked")
	}
	c, err := socksConnect(f.proxy, ln.Addr().String(), false)
	if err != nil {
		t.Fatal("slot not reusable", err)
	}
	c.Close()
}

func TestServerAdmissionLimitIndependentOfClient(t *testing.T) {
	ln, active := halfCloseOrigin(t)
	f := setupWithLimits(t, []string{ln.Addr().String()}, 3*time.Second, 64, 2)
	var cs []net.Conn
	defer func() {
		for _, c := range cs {
			c.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		c, err := socksConnect(f.proxy, ln.Addr().String(), false)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if c, err := socksConnect(f.proxy, ln.Addr().String(), false); err == nil {
		c.Close()
		t.Fatal("server admitted over limit")
	}
	if active.Load() != 2 {
		t.Fatal("server dialed over limit", active.Load())
	}
	// Rejection must not disrupt an already admitted connection.
	cs[0].(*net.TCPConn).CloseWrite()
	cs[0].SetReadDeadline(time.Now().Add(time.Second))
	p, err := io.ReadAll(cs[0])
	want := sha256.Sum256(nil)
	if err != nil || string(p) != hex.EncodeToString(want[:]) {
		t.Fatal("admitted connection disrupted", err)
	}
}
