package tunnel

import (
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleServerKeepsEpochAndStaysQuiet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	key := sha256.Sum256([]byte("local integration key"))
	var serverSent atomic.Int64
	toClient := make(chan []byte, 256)
	server := NewEndpoint(false, key[:], func(ctx context.Context, p []byte) error {
		serverSent.Add(1)
		select {
		case toClient <- p:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	server.HandshakeTimeout = 300 * time.Millisecond
	server.Opts.Announce = 100 * time.Millisecond
	// A server without an egress policy closes each session at once. It then
	// retires the client's epoch, possibly before its own ACK reached the
	// client, which stays unready on that epoch until its own handshake times
	// out: an artifact of the test, not of a configured server.
	policy, err := NewTargetDialer("public", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Policy = policy
	serverEvents := make(chan string, 100)
	server.Event = func(s string) { serverEvents <- s }
	wg.Add(1)
	go func() { defer wg.Done(); server.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	server.mu.Lock()
	epoch := server.conn
	server.mu.Unlock()
	announced := serverSent.Load()
	time.Sleep(time.Second)
	server.mu.Lock()
	same := epoch != nil && server.conn == epoch
	server.mu.Unlock()
	if !same {
		t.Fatal("idle server replaced its epoch")
	}
	if n := serverSent.Load(); n != announced {
		t.Fatalf("idle server kept sending: %d then %d records", announced, n)
	}
	select {
	case s := <-serverEvents:
		t.Fatalf("idle server reported %q", s)
	default:
	}

	// A client arriving later completes the handshake with the waiting epoch.
	client := NewEndpoint(true, key[:], func(_ context.Context, p []byte) error { server.Receive(p); return nil })
	clientEvents := make(chan string, 100)
	client.Event = func(s string) { clientEvents <- s }
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-toClient:
				client.Receive(p)
			}
		}
	}()
	go func() { defer wg.Done(); client.Run(ctx) }()
	for _, ch := range []chan string{clientEvents, serverEvents} {
		select {
		case s := <-ch:
			if s != "session_ready" {
				t.Fatalf("first event %q, want session_ready", s)
			}
		case <-ctx.Done():
			t.Fatal("late client did not get a session")
		}
	}
}

func TestStoppingAWaitingServerIsNotAFailedHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	key := sha256.Sum256([]byte("local integration key"))
	server := NewEndpoint(false, key[:], func(context.Context, []byte) error { return nil })
	events := make(chan string, 10)
	server.Event = func(s string) { events <- s }
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	close(events)
	for s := range events {
		t.Fatalf("stopping a waiting server reported %q", s)
	}
}
