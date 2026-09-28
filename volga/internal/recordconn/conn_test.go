package recordconn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func pair(t *testing.T, reorder bool) (*Conn, *Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	qa, qb := make(chan []byte, 256), make(chan []byte, 256)
	send := func(q chan []byte) SendFunc {
		return func(ctx context.Context, p []byte) error {
			select {
			case q <- p:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	key := sha256.Sum256([]byte("local test only"))
	a, e := New(ctx, key[:], true, send(qb))
	if e != nil {
		t.Fatal(e)
	}
	b, e := New(ctx, key[:], false, send(qa))
	if e != nil {
		t.Fatal(e)
	}
	forward := func(q chan []byte, c *Conn) {
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-q:
				batch := [][]byte{p}
				if reorder {
					for i := 0; i < 7; i++ {
						select {
						case p = <-q:
							batch = append(batch, p)
						default:
							i = 7
						}
					}
				}
				for i := len(batch) - 1; i >= 0; i-- {
					c.Receive(batch[i])
					if reorder {
						c.Receive(batch[i])
					}
				}
			}
		}
	}
	go forward(qa, a)
	go forward(qb, b)
	t.Cleanup(func() { cancel(); a.Close(); b.Close(); <-a.Done(); <-b.Done() })
	if e = a.Handshake(ctx); e != nil {
		t.Fatal(e)
	}
	if e = b.Handshake(ctx); e != nil {
		t.Fatal(e)
	}
	return a, b
}

func TestReorderedDuplicatedDuplexIntegrity(t *testing.T) {
	a, b := pair(t, true)
	x := bytes.Repeat([]byte("incompressibility-is-not-needed-for-an-ordering-test/"), 60000)
	done := make(chan error, 4)
	for _, c := range []*Conn{a, b} {
		go func(c *Conn) { _, e := c.Write(x); done <- e }(c)
		go func(c *Conn) {
			got := make([]byte, len(x))
			_, e := io.ReadFull(c, got)
			if e == nil && !bytes.Equal(got, x) {
				e = errors.New("integrity mismatch")
			}
			done <- e
		}(c)
	}
	for i := 0; i < 4; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	for _, c := range []*Conn{a, b} {
		s := c.Stats()
		if s.PeakBytes > Window || s.BufferedBytes != 0 {
			t.Fatalf("bounds: %+v", s)
		}
	}
}

func TestBlockedReadDeadlineUpdateAndClose(t *testing.T) {
	a, _ := pair(t, false)
	done := make(chan error, 1)
	go func() { _, e := a.Read(make([]byte, 1)); done <- e }()
	a.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	if e := <-done; !errors.Is(e, os.ErrDeadlineExceeded) {
		t.Fatal(e)
	}
	a.SetReadDeadline(time.Time{})
	go func() { _, e := a.Read(make([]byte, 1)); done <- e }()
	a.Close()
	if e := <-done; !errors.Is(e, net.ErrClosed) {
		t.Fatal(e)
	}
}

func TestUnreadPeerAppliesBoundedBackpressure(t *testing.T) {
	a, b := pair(t, false)
	a.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	n, e := a.Write(make([]byte, 2*Window))
	if !errors.Is(e, os.ErrDeadlineExceeded) || n > Window || n == 0 {
		t.Fatalf("n=%d error=%v", n, e)
	}
	if s := b.Stats(); s.BufferedBytes > Window {
		t.Fatalf("unbounded receiver: %+v", s)
	}
}

func TestAuthenticationAndBounds(t *testing.T) {
	a, b := pair(t, false)
	p, _ := a.seal('D', b.local, 0, []byte("secret"))
	p[len(p)-1] ^= 1
	b.Receive(p)
	if s := b.Stats(); s.BufferedBytes != 0 {
		t.Fatal("unauthenticated data accepted")
	}
	p, _ = a.seal('D', b.local, MaxWindow, []byte{1})
	b.Receive(p)
	if _, e := b.Read(make([]byte, 1)); !errors.Is(e, ErrProtocol) {
		t.Fatal(e)
	}
}

func TestCloseCancelsBlockedCarrierEnqueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := sha256.Sum256([]byte("blocked carrier test"))
	// Disable coalescing so a single small Write reaches the carrier send and
	// exercises the blocked-enqueue cancellation path directly.
	c, e := NewWithOptions(ctx, key[:], true, func(ctx context.Context, _ []byte) error { <-ctx.Done(); return ctx.Err() }, Options{FlushDelay: -1})
	if e != nil {
		t.Fatal(e)
	}
	c.mu.Lock()
	c.ready = true
	c.peer[0] = 1
	c.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, e := c.Write([]byte("hello")); done <- e }()
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked enqueue leaked")
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("control worker leaked")
	}
}

func TestDeadlineCancelsBlockedCarrierEnqueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := sha256.Sum256([]byte("blocked carrier deadline"))
	// Disable coalescing so the small Write blocks in the carrier send instead of
	// being staged, so the write deadline must cancel the in-flight enqueue.
	c, e := NewWithOptions(ctx, key[:], true, func(ctx context.Context, _ []byte) error { <-ctx.Done(); return ctx.Err() }, Options{FlushDelay: -1})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.mu.Lock()
	c.ready = true
	c.peer[0] = 1
	c.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, e := c.Write([]byte("hello")); done <- e }()
	c.SetWriteDeadline(time.Now().Add(25 * time.Millisecond))
	select {
	case e := <-done:
		if !errors.Is(e, os.ErrDeadlineExceeded) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("write deadline ignored")
	}
}

func TestWrongKeyCannotHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	key := sha256.Sum256([]byte("wrong key"))
	c, e := New(ctx, key[:], true, func(context.Context, []byte) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Handshake(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}
