package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"

	"openflux-volga-lab/internal/recordconn"
)

// scriptedHandshake plays one scripted outcome per Handshake call, so the
// waiting windows are tested without racing real timers.
type scriptedHandshake struct {
	peer  recordconn.Epoch
	steps []func(context.Context, *scriptedHandshake) error
	calls int
}

func (s *scriptedHandshake) Handshake(ctx context.Context) error {
	s.calls++
	return s.steps[s.calls-1](ctx, s)
}

func (s *scriptedHandshake) Peer() recordconn.Epoch { return s.peer }

// window waits out one attempt; answered marks a client that arrived during it
// without completing the handshake before the attempt ended.
func window(answered bool) func(context.Context, *scriptedHandshake) error {
	return func(ctx context.Context, s *scriptedHandshake) error {
		<-ctx.Done()
		if answered {
			s.peer = recordconn.Epoch{1}
		}
		return ctx.Err()
	}
}

func completes(context.Context, *scriptedHandshake) error { return nil }

func TestServerHandshakeGivesALateClientAFullWindow(t *testing.T) {
	e := &Endpoint{HandshakeTimeout: 5 * time.Millisecond}
	// Two idle windows; a client answers at the end of the third and completes in the fourth.
	s := &scriptedHandshake{steps: []func(context.Context, *scriptedHandshake) error{window(false), window(false), window(true), completes}}
	if err := e.handshake(context.Background(), s); err != nil || s.calls != 4 {
		t.Fatalf("err=%v after %d windows, want success in the window after the client answered", err, s.calls)
	}
}

func TestServerHandshakeStillFailsAClientThatWentQuiet(t *testing.T) {
	e := &Endpoint{HandshakeTimeout: 5 * time.Millisecond}
	// A client answered and then went quiet: one more full window, then failure.
	s := &scriptedHandshake{steps: []func(context.Context, *scriptedHandshake) error{window(true), window(false), completes}}
	if err := e.handshake(context.Background(), s); !errors.Is(err, context.DeadlineExceeded) || s.calls != 2 {
		t.Fatalf("err=%v after %d windows, want a timeout after one extra window", err, s.calls)
	}
}

func TestClientHandshakeFailsAfterOneWindow(t *testing.T) {
	e := &Endpoint{Client: true, HandshakeTimeout: 5 * time.Millisecond}
	s := &scriptedHandshake{steps: []func(context.Context, *scriptedHandshake) error{window(false), completes}}
	if err := e.handshake(context.Background(), s); !errors.Is(err, context.DeadlineExceeded) || s.calls != 1 {
		t.Fatalf("err=%v after %d windows, want the client to give up after one", err, s.calls)
	}
}
