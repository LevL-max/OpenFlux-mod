package tunnel

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

type pausedConn struct {
	net.Conn
	pause  *atomic.Pointer[chan struct{}]
	closed chan struct{}
	once   sync.Once
}

func (c *pausedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *pausedConn) Write(p []byte) (int, error) {
	if gate := c.pause.Load(); gate != nil {
		select {
		case <-*gate:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

// A provider Retry-After pause must not destroy every established stream.
// Use real timers: yamux's package-global timer pool can reuse timers created
// outside a testing/synctest bubble by other tests.
func TestMuxSurvivesProviderPause(t *testing.T) {
	a, b := net.Pipe()
	var pause atomic.Pointer[chan struct{}]
	client, err := yamux.Client(&pausedConn{Conn: a, pause: &pause, closed: make(chan struct{})}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := yamux.Server(&pausedConn{Conn: b, pause: &pause, closed: make(chan struct{})}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	pause.Store(&gate)
	time.Sleep(35 * time.Second)
	pause.Store(nil)
	close(gate)
	if client.IsClosed() || server.IsClosed() {
		t.Fatal("provider pause destroyed mux session")
	}
	written := make(chan error, 1)
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	go func() { _, err := stream.Write([]byte("resumed")); written <- err }()
	buf := make([]byte, 7)
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil || string(buf) != "resumed" {
		t.Fatal("existing stream did not recover", err)
	}
}
