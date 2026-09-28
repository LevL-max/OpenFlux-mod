package recordconn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"
)

// optPair builds a connected client/server pair with explicit Options and an
// in-memory carrier that copies each record to the peer.
func optPair(t *testing.T, o Options) (*Conn, *Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	qa, qb := make(chan []byte, 8192), make(chan []byte, 8192)
	send := func(q chan []byte) SendFunc {
		return func(ctx context.Context, p []byte) error {
			b := append([]byte(nil), p...)
			select {
			case q <- b:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	key := sha256.Sum256([]byte("options test only"))
	a, e := NewWithOptions(ctx, key[:], true, send(qb), o)
	if e != nil {
		t.Fatal(e)
	}
	b, e := NewWithOptions(ctx, key[:], false, send(qa), o)
	if e != nil {
		t.Fatal(e)
	}
	forward := func(q chan []byte, c *Conn) {
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-q:
				c.Receive(p)
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

// TestCoalescingReducesRecords drives the Yamux-like "12-byte header then body"
// write pattern and shows coalescing sends strictly fewer records than the
// one-record-per-Write behaviour, with identical delivered bytes.
func TestCoalescingReducesRecords(t *testing.T) {
	writes := make([][]byte, 0, 200)
	for i := 0; i < 100; i++ {
		writes = append(writes, bytes.Repeat([]byte{byte(i)}, 12))
		writes = append(writes, bytes.Repeat([]byte{byte(i * 7)}, 4000))
	}
	total := 0
	h := sha256.New()
	for _, w := range writes {
		total += len(w)
		h.Write(w)
	}
	want := hex.EncodeToString(h.Sum(nil))

	run := func(o Options) uint64 {
		a, b := optPair(t, o)
		got := make(chan []byte, 1)
		go func() {
			buf := make([]byte, total)
			if _, e := io.ReadFull(b, buf); e != nil {
				got <- nil
				return
			}
			got <- buf
		}()
		for _, w := range writes {
			if _, e := a.Write(w); e != nil {
				t.Fatal(e)
			}
		}
		select {
		case received := <-got:
			sum := sha256.Sum256(received)
			if hex.EncodeToString(sum[:]) != want {
				t.Fatalf("integrity mismatch (coalescing flushDelay=%v)", o.FlushDelay)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("read timed out")
		}
		return a.Stats().Records
	}

	on := run(Options{})                  // coalescing at the default 1 ms
	off := run(Options{FlushDelay: -1})   // one record per Write
	if on >= off {
		t.Fatalf("coalescing did not reduce records: on=%d off=%d", on, off)
	}
	if off != uint64(len(writes)) {
		t.Fatalf("disabled coalescing should be one record per write: got=%d want=%d", off, len(writes))
	}
}

func TestCustomChunkWindowIntegrity(t *testing.T) {
	a, b := optPair(t, Options{Chunk: 1024, Window: 8 * 1024, FlushDelay: -1})
	x := bytes.Repeat([]byte("custom-chunk-window/"), 5000)
	done := make(chan error, 2)
	go func() { _, e := a.Write(x); done <- e }()
	go func() {
		buf := make([]byte, len(x))
		_, e := io.ReadFull(b, buf)
		if e == nil && !bytes.Equal(buf, x) {
			e = errors.New("integrity mismatch")
		}
		done <- e
	}()
	for i := 0; i < 2; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	if s := a.Stats(); s.Chunk != 1024 || s.Window != 8*1024 {
		t.Fatalf("stats did not reflect options: %+v", s)
	}
}

func TestRejectsInvalidOptions(t *testing.T) {
	ctx := context.Background()
	key := sha256.Sum256([]byte("reject"))
	send := func(context.Context, []byte) error { return nil }
	if _, e := NewWithOptions(ctx, key[:], true, send, Options{Chunk: 100}); e == nil {
		t.Fatal("expected rejection of undersized chunk")
	}
	if _, e := NewWithOptions(ctx, key[:], true, send, Options{Chunk: 1024, Window: 2048}); e == nil {
		t.Fatal("expected rejection of window smaller than 4 chunks")
	}
	if _, e := NewWithOptions(ctx, key[:], true, send, Options{Chunk: MaxChunk + 1}); e == nil {
		t.Fatal("expected rejection of oversized chunk")
	}
}
