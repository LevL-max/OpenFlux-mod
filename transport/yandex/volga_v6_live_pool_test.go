//go:build volga

package yandex

// Diagnostic-only pool: both peers listen to every document throughout the run.
// The existing V6 runtime retains one logical sequence and replay ledger.
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type liveCarrierPool struct {
	generation uint64
	lanes      []*volgaV6YandexCarrier
	next       atomic.Uint64
}

func livePoolFactory(documents []string, cfg volgaV6YandexConfig) volgaV6CarrierFactory {
	// Aggregate pacing and Retry-After apply to all lanes and generations.
	cfg.relayGate = &volgaV6RelayGate{rate: cfg.RelayPostsPerSecond}
	return func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		if len(documents) < 1 || len(documents) > 4 {
			return nil, fmt.Errorf("diagnostic pool requires 1-4 lanes")
		}
		pool := &liveCarrierPool{generation: generation}
		for _, doc := range documents {
			pool.lanes = append(pool.lanes, newVolgaV6YandexCarrier(generation, doc, cfg, onFrame))
		}
		return pool, nil
	}
}

func (p *liveCarrierPool) Generation() uint64 { return p.generation }
func (p *liveCarrierPool) Start(ctx context.Context) error {
	for lane, c := range p.lanes {
		if err := c.Start(ctx); err != nil {
			_ = p.Stop()
			return err
		}
		auth, client := c.authSnapshot()
		emitLiveLane("identity", map[string]any{"generation": p.generation, "lane": lane,
			"resource_hash":     fmt.Sprintf("%x", sha256.Sum256([]byte(auth.ResourceURL))),
			"request_path_hash": fmt.Sprintf("%x", sha256.Sum256([]byte(auth.RequestPath))),
			"push_user_hash":    fmt.Sprintf("%x", sha256.Sum256([]byte(auth.UserIDStr)))})
		client.Transport = &liveHTTPObserver{base: client.Transport, lane: lane, generation: p.generation}
	}
	return nil
}

type liveHTTPObserver struct {
	base       http.RoundTripper
	lane       int
	generation uint64
	reported   atomic.Uint64
}

func (o *liveHTTPObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.base.RoundTrip(req)
	if err == nil && resp.StatusCode >= 400 && o.reported.Add(1) <= 10 {
		after := resp.Header.Get("Retry-After")
		deadline := volgaV6RetryAfter(after, time.Now())
		emitLiveLane("http_error", map[string]any{"lane": o.lane, "generation": o.generation, "status": resp.StatusCode,
			"retry_after_present": after != "", "retry_deadline": deadline, "response_length": resp.ContentLength})
	}
	return resp, err
}
func (o *liveHTTPObserver) CloseIdleConnections() {
	if c, ok := o.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
func emitLiveLane(kind string, data any) {
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "event": kind, "data": data})
	fmt.Printf("V6LANE %s\n", b)
}

func liveLaneSnapshots(tr *YandexVolgaV6Transport) {
	tr.runtime.manager.mu.RLock()
	active := tr.runtime.manager.active
	tr.runtime.manager.mu.RUnlock()
	if p, ok := active.(*liveCarrierPool); ok {
		for lane, c := range p.lanes {
			emitLiveLane("snapshot", map[string]any{"lane": lane, "snapshot": c.Snapshot()})
		}
	}
}
func (p *liveCarrierPool) Stop() error {
	var first error
	for _, c := range p.lanes {
		if err := c.Stop(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
func (p *liveCarrierPool) SendVolgaV6(f volgaV6WireFrame) error {
	i := (p.next.Add(1) - 1) % uint64(len(p.lanes))
	return p.lanes[i].SendVolgaV6(f)
}
func (p *liveCarrierPool) VolgaV6PhysicalHealth(now time.Time) volgaV6PhysicalHealth {
	out := volgaV6PhysicalHealth{Known: true, Generation: p.generation, Connected: true, HTTPStatuses: make(map[int]uint64)}
	for _, c := range p.lanes {
		s := c.VolgaV6PhysicalHealth(now)
		out.Connected = out.Connected && s.Connected
		out.Posts += s.Posts
		out.PostFailures += s.PostFailures
		out.PostBytes += s.PostBytes
		out.PostMicros += s.PostMicros
		out.MaxPostMicros = max(out.MaxPostMicros, s.MaxPostMicros)
		out.WSReconnects += s.WSReconnects
		out.HTTP1Posts += s.HTTP1Posts
		out.HTTP2Posts += s.HTTP2Posts
		out.WSDataFrames += s.WSDataFrames
		out.WSAckFrames += s.WSAckFrames
		out.WSDecodeErrors += s.WSDecodeErrors
		out.WSRawMessages += s.WSRawMessages
		out.WSIgnoredMessages += s.WSIgnoredMessages
		out.WSJSONErrors += s.WSJSONErrors
		out.HTTPTransportErrors += s.HTTPTransportErrors
		out.RelayPostsPerSecond = s.RelayPostsPerSecond
		if s.RelayRetryAt.After(out.RelayRetryAt) {
			out.RelayRetryAt = s.RelayRetryAt
		}
		for code, n := range s.HTTPStatuses {
			out.HTTPStatuses[code] += n
		}
	}
	return out
}
