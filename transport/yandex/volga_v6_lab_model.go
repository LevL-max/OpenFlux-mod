//go:build volga_lab_model

package yandex

// Offline carrier for comparisons of the actual V6 reliability/batching stack.
// Excluded from ordinary builds. It models pacing and variable delay, not the
// provider's quota, authentication, HTTP implementation or available capacity.
import (
	"context"
	"sync"
	"time"

	"universal-bypass-tool/transport"
)

type VolgaLabModelStats struct {
	Posts            uint64 `json:"posts"`
	UniqueBatches    uint64 `json:"unique_batches"`
	UniqueBatchBytes uint64 `json:"unique_batch_bytes"`
	Records          uint64 `json:"records"`
	DataRecords      uint64 `json:"data_records"`
	DataBytes        uint64 `json:"data_bytes"`
	SmallDataRecords uint64 `json:"small_data_records"`
}
type VolgaLabModelCarrier struct {
	ctx    context.Context
	peer   *volgaV6Runtime
	gate   volgaV6RelayGate
	delays []time.Duration
	mu     sync.Mutex
	stats  VolgaLabModelStats
	seen   map[uint64]bool
}

func (c *VolgaLabModelCarrier) Snapshot() VolgaLabModelStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}
func (c *VolgaLabModelCarrier) Generation() uint64          { return 1 }
func (c *VolgaLabModelCarrier) Start(context.Context) error { return nil }
func (c *VolgaLabModelCarrier) Stop() error                 { return nil }
func (c *VolgaLabModelCarrier) SendVolgaV6(f volgaV6WireFrame) error {
	if e := c.gate.wait(c.ctx); e != nil {
		return e
	}
	c.mu.Lock()
	c.stats.Posts++
	n := c.stats.Posts
	if f.Kind == volgaV6FrameData && !c.seen[f.Seq] {
		c.seen[f.Seq] = true
		c.stats.UniqueBatches++
		for _, p := range f.Payload {
			c.stats.Records++
			c.stats.UniqueBatchBytes += uint64(len(p))
			if len(p) >= 74 && string(p[:4]) == "VLS1" && p[5] == 'D' {
				c.stats.DataRecords++
				c.stats.DataBytes += uint64(len(p) - 74)
				if len(p) < 4774 {
					c.stats.SmallDataRecords++
				}
			}
		}
	}
	c.mu.Unlock()
	timer := time.NewTimer(c.delays[n%uint64(len(c.delays))])
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-timer.C:
	}
	c.peer.handleIncoming(f)
	return nil
}
func NewVolgaLabModelPair(ctx context.Context, delays []time.Duration) (a, b *YandexVolgaV6Transport, ca, cb *VolgaLabModelCarrier) {
	if len(delays) == 0 {
		delays = []time.Duration{40 * time.Millisecond, 100 * time.Millisecond, 55 * time.Millisecond, 85 * time.Millisecond}
	}
	ca = &VolgaLabModelCarrier{ctx: ctx, gate: volgaV6RelayGate{rate: 360}, delays: delays, seen: make(map[uint64]bool)}
	cb = &VolgaLabModelCarrier{ctx: ctx, gate: volgaV6RelayGate{rate: 360}, delays: delays, seen: make(map[uint64]bool)}
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.Telemetry = false
	cfg.QueueSize, cfg.SendQueueSize, cfg.SendWorkers = 512, 64, 64
	a = newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) { return ca, nil })
	b = newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) { return cb, nil })
	ca.peer, cb.peer = b.runtime, a.runtime
	return
}
