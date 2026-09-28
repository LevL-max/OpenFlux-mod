//go:build volga

package yandex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVolgaV6LivePoolSharesBudgetAndPreservesFrames(t *testing.T) {
	cfg := defaultVolgaV6YandexConfig()
	cfg.RelayPostsPerSecond = 400
	cfg.RelayEnvelope = "minimal"
	var mu sync.Mutex
	seen := make(map[uint64]bool)
	physical, err := livePoolFactory([]string{"a", "b", "a", "b"}, cfg)(1, func(f volgaV6WireFrame) {
		mu.Lock()
		defer mu.Unlock()
		if f.Session != 77 || seen[f.Seq] || len(f.Payload) != 1 || string(f.Payload[0]) != "payload" {
			t.Errorf("invalid delivery: %+v", f)
		}
		seen[f.Seq] = true
	})
	if err != nil {
		t.Fatal(err)
	}
	p := physical.(*liveCarrierPool)
	defer p.Stop()
	counts := make([]int, len(p.lanes))
	for lane, c := range p.lanes {
		if c.config.relayGate != p.lanes[0].config.relayGate {
			t.Fatal("lanes bypass shared budget")
		}
		c.auth = &volgaAuth{UserID: lane + 1}
		c.started.Store(true)
		ws := newVolgaV6YandexWS(c, c.auth)
		c.http = &http.Client{Transport: volgaV6LocalRoundTripper(func(req *http.Request) (*http.Response, error) {
			var body struct {
				Message json.RawMessage `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			counts[lane]++
			mu.Unlock()
			ws.handleRelay(body.Message, true)
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})}
	}
	var wg sync.WaitGroup
	for seq := uint64(1); seq <= 40; seq++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.SendVolgaV6(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 77, Seq: seq, Floor: 1, Payload: [][]byte{[]byte("payload")}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(seen) != 40 {
		t.Fatalf("received %d/40", len(seen))
	}
	for lane, n := range counts {
		if n != 10 {
			t.Fatalf("lane %d received %d/10", lane, n)
		}
	}
	p.lanes[1].config.relayGate.limited("1", time.Now())
	for _, c := range p.lanes {
		rate, until := c.config.relayGate.snapshot()
		if rate != 200 || until.Before(time.Now()) {
			t.Fatal("429 not shared by pool")
		}
	}
}

func TestVolgaV6LivePoolCanceledStartStopsAllLanes(t *testing.T) {
	physical, err := livePoolFactory([]string{"a", "b"}, defaultVolgaV6YandexConfig())(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := physical.Start(ctx); err == nil {
		t.Fatal("canceled startup accepted")
	}
	for _, c := range physical.(*liveCarrierPool).lanes {
		if c.ctx.Err() == nil {
			t.Fatal("partially started lane left alive")
		}
	}
}
