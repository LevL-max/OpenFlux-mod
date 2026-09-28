package recordconn

import (
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"time"
)

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
