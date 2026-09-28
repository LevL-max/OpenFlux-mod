package tunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"openflux-volga-lab/internal/recordconn"
)

type Endpoint struct {
	Client     bool
	Key        []byte
	Send       recordconn.SendFunc
	Policy     *TargetDialer
	MaxStreams int
	Idle       time.Duration
	Event      func(string)
	Opts       recordconn.Options
	mu         sync.Mutex
	openMu     sync.Mutex
	conn       *recordconn.Conn
	mux        *yamux.Session
	changed    chan struct{}
	retired    map[recordconn.Epoch]bool
}

func NewEndpoint(client bool, key []byte, send recordconn.SendFunc) *Endpoint {
	return &Endpoint{Client: client, Key: append([]byte(nil), key...), Send: send, Idle: 60 * time.Second, changed: make(chan struct{}), retired: make(map[recordconn.Epoch]bool)}
}
func (e *Endpoint) event(s string) {
	if e.Event != nil {
		e.Event(s)
	}
}
func (e *Endpoint) notifyLocked() { close(e.changed); e.changed = make(chan struct{}) }
func (e *Endpoint) Receive(p []byte) {
	e.mu.Lock()
	c := e.conn
	e.mu.Unlock()
	if c == nil {
		return
	}
	peer, ok := c.Identify(p)
	if !ok {
		return
	}
	e.mu.Lock()
	retired := e.retired[peer]
	e.mu.Unlock()
	if !retired {
		c.Receive(p)
	}
}
func (e *Endpoint) Stats() recordconn.Stats {
	e.mu.Lock()
	c := e.conn
	e.mu.Unlock()
	if c == nil {
		return recordconn.Stats{}
	}
	return c.Stats()
}
func (e *Endpoint) Open(ctx context.Context) (*yamux.Stream, error) {
	limit, err := StreamLimit(e.MaxStreams)
	if err != nil {
		return nil, err
	}
	e.openMu.Lock()
	defer e.openMu.Unlock()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e.mu.Lock()
		m, ch := e.mux, e.changed
		e.mu.Unlock()
		if m != nil && !m.IsClosed() {
			if m.NumStreams() >= limit {
				return nil, errors.New("concurrent stream limit reached")
			}
			return m.OpenStream()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// Run creates fresh authenticated record epochs after a lost peer/session. TCP
// connections belonging to a failed session close; subsequent SOCKS requests
// use the new session. It never claims to resume an interrupted TCP connection.
func (e *Endpoint) Run(ctx context.Context) error {
	if _, err := StreamLimit(e.MaxStreams); err != nil {
		return err
	}
	for ctx.Err() == nil {
		c, err := recordconn.NewWithOptions(ctx, e.Key, e.Client, e.Send, e.Opts)
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.conn = c
		e.mu.Unlock()
		handshake, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = c.Handshake(handshake)
		cancel()
		if err == nil {
			var m *yamux.Session
			if e.Client {
				m, err = yamux.Client(c, MuxConfig())
			} else {
				m, err = yamux.Server(c, MuxConfig())
			}
			if err == nil {
				e.mu.Lock()
				e.mux = m
				e.notifyLocked()
				e.mu.Unlock()
				e.event("session_ready")
				if e.Client {
					select {
					case <-ctx.Done():
					case <-m.CloseChan():
					}
				} else {
					ServeRemote(ctx, m, e.Policy, e.Idle, e.MaxStreams)
				}
				m.Close()
				e.mu.Lock()
				e.mux = nil
				e.retired[c.Peer()] = true
				e.notifyLocked()
				e.mu.Unlock()
				e.event("session_closed")
			}
		} else {
			e.event("handshake_failed")
		}
		c.Close()
		<-c.Done()
		e.mu.Lock()
		e.conn = nil
		count := len(e.retired)
		e.mu.Unlock()
		// Bound old-epoch metadata. Frequent restarts are a diagnostic failure,
		// not a reason to retain an unbounded history or reopen old sessions.
		if count >= 64 {
			return errors.New("session restart limit reached")
		}
		t := time.NewTimer(300 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}
