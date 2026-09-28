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
	mu      sync.Mutex
	rate    float64
	next    time.Time
	retryAt time.Time
	backoff time.Duration
	epoch   uint64
}

func (g *volgaV6RelayGate) snapshot() (float64, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rate, g.retryAt
}

func (g *volgaV6RelayGate) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		at, epoch := g.reserve(time.Now())
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
	g.mu.Lock()
	defer g.mu.Unlock()
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
	}
}
