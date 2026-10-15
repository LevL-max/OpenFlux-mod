//go:build volga

package yandex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestVolgaV6RetryAfterForms(t *testing.T) {
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	for _, header := range []string{" 60 ", now.Add(time.Minute).Format(http.TimeFormat)} {
		if got := volgaV6RetryAfter(header, now); !got.Equal(now.Add(time.Minute)) {
			t.Fatalf("%q: %v", header, got)
		}
	}
	for _, header := range []string{"", "-1", "invalid", now.Add(-time.Minute).Format(http.TimeFormat)} {
		if got := volgaV6RetryAfter(header, now); !got.IsZero() {
			t.Fatalf("accepted invalid/past %q", header)
		}
	}
}

func TestVolgaV6RateLimitSharedAcrossGenerations(t *testing.T) {
	for _, factory := range []volgaV6CarrierFactory{
		newVolgaV6SingleDocumentCarrierFactory([]string{"doc"}, defaultVolgaV6YandexConfig()),
		newVolgaV6YandexCarrierFactory([]string{"doc"}, defaultVolgaV6YandexConfig()),
	} {
		a, _ := factory(1, nil)
		b, _ := factory(2, nil)
		ca, cb := a.(*volgaV6YandexCarrier), b.(*volgaV6YandexCarrier)
		now := time.Now()
		ca.config.relayGate.limited("60", now)
		for i := 0; i < 100; i++ {
			ca.config.relayGate.limited("", now.Add(time.Millisecond))
		}
		rate, until := cb.config.relayGate.snapshot()
		if rate != 100 || !until.Equal(now.Add(time.Minute)) {
			t.Fatalf("handoff/burst reset cooldown: rate=%v until=%v", rate, until)
		}
		cb.config.relayGate.limited("", until.Add(time.Second))
		if rate, _ := ca.config.relayGate.snapshot(); rate != 50 {
			t.Fatalf("second episode did not reduce rate: %v", rate)
		}
		ca.Stop()
		cb.Stop()
	}
}

func TestVolgaV6RateLimitBlocksPOSTAndCancels(t *testing.T) {
	c := newOfflineVolgaV6YandexCarrier(t, 1, 8000)
	c.http = &http.Client{Transport: volgaV6LocalRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := c.postRelayBody([]byte("{}")); err == nil {
		t.Fatal("429 was accepted as success")
	}
	done := make(chan error, 1)
	go func() { done <- c.postRelayBody([]byte("{}")) }()
	select {
	case err := <-done:
		t.Fatalf("POST bypassed cooldown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	c.cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cooldown did not cancel")
	}
	if c.Snapshot().Posts != 1 {
		t.Fatal("extra network request during cooldown")
	}
}

func TestVolgaV6ProviderPauseDoesNotRecycle(t *testing.T) {
	wire, _ := localVolgaV6Wire(t, func(volgaV6WireFrame) {})
	r := newVolgaV6Runtime(123, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6StartedWire{wire}, nil
	}, defaultVolgaV6RuntimeConfig(), nil)
	now := time.Now()
	if err := r.startAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if _, err := r.sendAt([][]byte{[]byte("pending")}, now); err != nil {
		t.Fatal(err)
	}
	wire.config.relayGate.limited("60", now)
	result := r.Tick(context.Background(), now.Add(10*time.Second))
	if result.Handoff || result.Repairs != 0 || result.Recovery.Reason != "provider-rate-limit" {
		t.Fatalf("cooldown triggered recovery: %+v", result)
	}
	if r.session.Snapshot(now).ReplayDepth != 1 {
		t.Fatal("cooldown lost pending payload")
	}
}

func TestVolgaV6RelayPacing(t *testing.T) {
	g := &volgaV6RelayGate{rate: 50}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := g.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("requests exceeded configured pacing rate")
	}
}

func TestVolgaV6PacingReservationsDoNotStarve(t *testing.T) {
	g := &volgaV6RelayGate{rate: 100}
	now := time.Now()
	for i := 0; i < 96; i++ {
		at, epoch := g.reserve(now)
		if want := now.Add(time.Duration(i) * 10 * time.Millisecond); !at.Equal(want) || epoch != 0 {
			t.Fatalf("slot %d: at=%v epoch=%d", i, at, epoch)
		}
	}
	g.limited("60", now)
	at, epoch := g.reserve(now)
	if !at.Equal(now.Add(time.Minute)) || epoch != 1 {
		t.Fatalf("old reservations survived 429: %v %d", at, epoch)
	}
}

func TestVolgaV6IdlePeerDoesNotKeepAccumulatedRatePenalty(t *testing.T) {
	g := &volgaV6RelayGate{rate: 480}
	now := time.Unix(1000, 0)
	// Several peer disconnects reduce the shared gate below the rate needed
	// by the protocol's control records. A later client must be able to start.
	for i := 0; i < 10; i++ {
		g.limited("", now)
		now = now.Add(time.Minute)
	}
	if rate, _ := g.snapshot(); rate != 1 {
		t.Fatalf("fixture rate = %v, want 1", rate)
	}
	now = now.Add(20 * time.Minute)
	at, _ := g.reserve(now)
	if rate, _ := g.snapshot(); rate != 480 || !at.Equal(now) {
		t.Fatalf("idle peer kept penalty: rate=%v, next=%v", rate, at)
	}
	second, _ := g.reserve(now)
	if !second.After(at) {
		t.Fatal("recovery removed pacing")
	}
}

func TestVolgaV6IdleRecoveryRespectsProviderPauseAndQueuedWork(t *testing.T) {
	now := time.Unix(1000, 0)
	g := &volgaV6RelayGate{rate: 480}
	g.limited("600", now)
	at, _ := g.reserve(now.Add(2 * time.Minute))
	if rate, _ := g.snapshot(); rate != 240 || at.Before(now.Add(10*time.Minute)) {
		t.Fatalf("idle recovery bypassed Retry-After: rate=%v at=%v", rate, at)
	}
	// Work reserved recently, including reservations during the cooldown,
	// prevents recovery just because the last 429 is old.
	now = now.Add(10 * time.Minute)
	for i := 0; i < 5; i++ {
		g.reserve(now.Add(time.Duration(i) * 30 * time.Second))
	}
	if rate, _ := g.snapshot(); rate != 240 {
		t.Fatalf("active reservations replenished rate: %v", rate)
	}
}
