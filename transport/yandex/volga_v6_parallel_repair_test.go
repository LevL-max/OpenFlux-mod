//go:build volga

package yandex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVolgaV6RepairConcurrencyRespectsBudget(t *testing.T) {
	carrier := newOfflineVolgaV6YandexCarrier(t, 1, 8000)
	carrier.started.Store(true)
	var block atomic.Bool
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	carrier.http = &http.Client{Transport: volgaV6LocalRoundTripper(func(*http.Request) (*http.Response, error) {
		if block.Load() {
			started <- struct{}{}
			<-release
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Recovery.RetryBurst = 4
	cfg.RepairWorkers = 4
	cfg.Recovery.ProgressStall = time.Minute
	r := newVolgaV6Runtime(123, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6StartedWire{carrier}, nil
	}, cfg, nil)
	now := time.Now()
	if err := r.startAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	for i := 0; i < 8; i++ {
		if _, err := r.sendAt([][]byte{{byte(i)}}, now); err != nil {
			t.Fatal(err)
		}
	}
	block.Store(true)
	done := make(chan volgaV6RuntimeTickResult, 1)
	go func() { done <- r.Tick(context.Background(), now.Add(time.Second)) }()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("only %d concurrent repairs; budget was 4", i)
		}
	}
	select {
	case <-started:
		t.Fatal("retry budget exceeded")
	case <-time.After(20 * time.Millisecond):
	}
	// The HTTP work is still outstanding, so Tick must not report completion.
	select {
	case <-done:
		t.Fatal("Tick returned while repair work was outstanding")
	default:
	}
	unblock()
	select {
	case result := <-done:
		if result.Repairs != 4 || result.RepairErr != nil {
			t.Fatalf("repair outcome: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("repairs did not finish")
	}
}
