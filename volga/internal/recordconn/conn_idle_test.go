package recordconn

import (
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestLostHandshakeConfirmationDoesNotFloodCarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		key := sha256.Sum256([]byte("lost handshake confirmation"))
		var confirmations atomic.Int64
		server, err := New(ctx, key[:], false, func(_ context.Context, p []byte) error {
			if p[5] == 'K' {
				confirmations.Add(1)
			}
			return nil // The client cannot hear the server yet.
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { server.Close(); <-server.Done() }()
		client, err := New(ctx, key[:], true, func(_ context.Context, p []byte) error {
			server.Receive(p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { client.Close(); <-client.Done() }()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if got := confirmations.Load(); got < 5 || got > 11 {
			t.Fatalf("lost confirmation generated %d K records in 3 seconds; want bounded retries", got)
		}
		// The next client confirmation still completes the original handshake.
		hello, _ := server.seal('H', Epoch{}, 0, nil)
		client.Receive(hello)
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		if !server.Stats().Ready {
			t.Fatal("handshake did not recover when the return path became available")
		}
	})
}

func TestServerAnnouncesBrieflyThenWaitsForClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := sha256.Sum256([]byte("local test only"))
	var sent atomic.Int64
	server, e := NewWithOptions(ctx, key[:], false, func(context.Context, []byte) error { sent.Add(1); return nil }, Options{Announce: 200 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { server.Close(); <-server.Done() }()
	time.Sleep(400 * time.Millisecond)
	announced := sent.Load()
	if announced == 0 {
		t.Fatal("new server epoch did not announce itself")
	}
	time.Sleep(700 * time.Millisecond)
	if n := sent.Load(); n != announced {
		t.Fatalf("idle server kept sending: %d then %d records", announced, n)
	}
	client, e := New(ctx, key[:], true, func(_ context.Context, p []byte) error { server.Receive(p); return nil })
	if e != nil {
		t.Fatal(e)
	}
	defer func() { client.Close(); <-client.Done() }()
	deadline := time.Now().Add(time.Second)
	for sent.Load() == announced {
		if time.Now().After(deadline) {
			t.Fatal("server did not answer a client hello")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
