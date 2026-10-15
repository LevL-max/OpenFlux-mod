//go:build volga

package yandex

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shared by every physical generation. Reauthorization must not bypass the
// provider's Retry-After or replenish a pacing budget.
type volgaV6RelayGate struct {
	mu        sync.Mutex
	rate      float64
	ceiling   float64 // rate before congestion backoff
	next      time.Time
	relayNext time.Time // last relay reservation; session pings only advance next
	retryAt   time.Time
	backoff   time.Duration
	epoch     uint64
}

func (g *volgaV6RelayGate) snapshot() (float64, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rate, g.retryAt
}

func (g *volgaV6RelayGate) wait(ctx context.Context) error {
	return g.waitRequest(ctx, true)
}

func (g *volgaV6RelayGate) waitSessionPing(ctx context.Context) error {
	return g.waitRequest(ctx, false)
}

func (g *volgaV6RelayGate) waitRequest(ctx context.Context, relay bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		at, epoch := g.reserveRequest(time.Now(), relay)
		timer := time.NewTimer(time.Until(at))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		g.mu.Lock()
		valid := epoch == g.epoch
		g.mu.Unlock()
		if valid {
			return ctx.Err()
		}
	}
}

// Reserve a distinct slot before waiting. Otherwise fast workers can repeatedly
// win the same wakeup race and starve an older DATA sequence indefinitely.
func (g *volgaV6RelayGate) reserve(now time.Time) (time.Time, uint64) {
	return g.reserveRequest(now, true)
}

func (g *volgaV6RelayGate) reserveRequest(now time.Time, relay bool) (time.Time, uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// A disconnected peer can provoke several 429 episodes while reliable
	// delivery drains. Keeping every rate reduction forever eventually makes
	// even handshake/keepalive traffic exceed the remaining budget. After a
	// full minute without reserved relay work, start again at the configured
	// rate. Session pings keep an idle editor alive and must not postpone that
	// recovery. They still share pacing, pending slots and Retry-After.
	if g.ceiling > g.rate && now.Sub(g.relayNext) >= time.Minute && !now.Before(g.next) && !now.Before(g.retryAt) {
		g.rate = g.ceiling
		g.backoff = 0
	}
	at := now
	if g.retryAt.After(at) {
		at = g.retryAt
	}
	if g.next.After(at) {
		at = g.next
	}
	if g.rate > 0 {
		g.next = at.Add(time.Duration(float64(time.Second) / g.rate))
	}
	if relay {
		g.relayNext = g.next
	}
	return at, g.epoch
}

func volgaV6RetryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date
	}
	return time.Time{}
}

func (g *volgaV6RelayGate) limited(value string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	previous := g.retryAt
	// One reduction per episode, not per already in-flight request.
	if !now.Before(g.retryAt) {
		if g.ceiling == 0 {
			g.ceiling = g.rate
			if g.ceiling == 0 {
				g.ceiling = 100
			}
		}
		g.backoff = min(max(time.Second, 2*g.backoff), 30*time.Second)
		if g.rate == 0 {
			g.rate = 100
		} else {
			g.rate = max(1, g.rate/2)
		}
		g.retryAt = now.Add(g.backoff)
	}
	if requested := volgaV6RetryAfter(value, now); requested.After(g.retryAt) {
		g.retryAt = requested
	}
	if !g.retryAt.Equal(previous) {
		g.epoch++
		g.next = g.retryAt
		g.relayNext = g.retryAt
	}
}
