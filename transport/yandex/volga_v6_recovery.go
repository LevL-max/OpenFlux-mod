package yandex

import (
	"math"
	"sync"
	"time"
)

type volgaV6RecoveryConfig struct {
	ProgressStall    time.Duration
	RecycleCooldown time.Duration

	MaxWindow         int
	HighWatermark     int
	CriticalWatermark int
	MinWindow         int

	RetryRatePerSecond float64
	RetryBurst         int
}

func defaultVolgaV6RecoveryConfig() volgaV6RecoveryConfig {
	return volgaV6RecoveryConfig{
		ProgressStall:       2 * time.Second,
		RecycleCooldown:     5 * time.Second,
		MaxWindow:           512,
		HighWatermark:       256,
		CriticalWatermark:   384,
		MinWindow:           32,
		RetryRatePerSecond:  24,
		RetryBurst:          8,
	}
}

type volgaV6RecoveryDecision struct {
	Recycle        bool
	Reason         string
	AdmissionLimit int
	ProgressAge    time.Duration
}

// volgaV6RecoveryController interprets end-to-end logical progress. It never
// looks at HTTP 200/204 as proof of delivery and it does not own a Yandex
// carrier. Its output can therefore be tested without live network traffic.
type volgaV6RecoveryController struct {
	config volgaV6RecoveryConfig

	mu sync.Mutex

	initialized  bool
	lastAckBase  uint64
	lastProgress time.Time
	lastRecycle  time.Time

	retryTokens float64
	retryRefill time.Time
}

func newVolgaV6RecoveryController(cfg volgaV6RecoveryConfig) *volgaV6RecoveryController {
	defaults := defaultVolgaV6RecoveryConfig()
	if cfg.ProgressStall <= 0 {
		cfg.ProgressStall = defaults.ProgressStall
	}
	if cfg.RecycleCooldown <= 0 {
		cfg.RecycleCooldown = defaults.RecycleCooldown
	}
	if cfg.MaxWindow <= 0 {
		cfg.MaxWindow = defaults.MaxWindow
	}
	if cfg.HighWatermark <= 0 || cfg.HighWatermark > cfg.MaxWindow {
		cfg.HighWatermark = minInt(defaults.HighWatermark, cfg.MaxWindow)
	}
	if cfg.CriticalWatermark <= 0 || cfg.CriticalWatermark > cfg.MaxWindow {
		cfg.CriticalWatermark = minInt(defaults.CriticalWatermark, cfg.MaxWindow)
	}
	if cfg.CriticalWatermark < cfg.HighWatermark {
		cfg.CriticalWatermark = cfg.HighWatermark
	}
	if cfg.MinWindow <= 0 || cfg.MinWindow > cfg.MaxWindow {
		cfg.MinWindow = minInt(defaults.MinWindow, cfg.MaxWindow)
	}
	if cfg.RetryRatePerSecond <= 0 {
		cfg.RetryRatePerSecond = defaults.RetryRatePerSecond
	}
	if cfg.RetryBurst <= 0 {
		cfg.RetryBurst = defaults.RetryBurst
	}
	return &volgaV6RecoveryController{
		config:      cfg,
		retryTokens: float64(cfg.RetryBurst),
	}
}

func (c *volgaV6RecoveryController) admissionLimitLocked(replayDepth int) int {
	if replayDepth < 0 {
		replayDepth = 0
	}
	if replayDepth >= c.config.CriticalWatermark {
		return c.config.MinWindow
	}
	if replayDepth >= c.config.HighWatermark {
		limit := c.config.MaxWindow / 4
		if limit < c.config.MinWindow {
			limit = c.config.MinWindow
		}
		return limit
	}
	if replayDepth >= c.config.HighWatermark/2 {
		limit := c.config.MaxWindow / 2
		if limit < c.config.MinWindow {
			limit = c.config.MinWindow
		}
		return limit
	}
	return c.config.MaxWindow
}

func (c *volgaV6RecoveryController) AdmissionLimit(replayDepth int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admissionLimitLocked(replayDepth)
}

func (c *volgaV6RecoveryController) Observe(now time.Time, snap volgaV6ReliableSnapshot) volgaV6RecoveryDecision {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.initialized {
		c.initialized = true
		c.lastAckBase = snap.AckBase
		c.lastProgress = now
	}
	if snap.AckBase > c.lastAckBase {
		c.lastAckBase = snap.AckBase
		c.lastProgress = now
	}
	if snap.ReplayDepth == 0 {
		// With no outstanding DATA there is no delivery-progress evidence that
		// justifies recycling a healthy physical carrier.
		c.lastProgress = now
	}

	progressAge := time.Duration(0)
	if !c.lastProgress.IsZero() && now.After(c.lastProgress) {
		progressAge = now.Sub(c.lastProgress)
	}
	decision := volgaV6RecoveryDecision{
		AdmissionLimit: c.admissionLimitLocked(snap.ReplayDepth),
		ProgressAge:    progressAge,
	}

	stalled := snap.ReplayDepth > 0 &&
		snap.OldestUnackedAge >= c.config.ProgressStall &&
		progressAge >= c.config.ProgressStall
	if !stalled {
		return decision
	}
	if !c.lastRecycle.IsZero() && now.Sub(c.lastRecycle) < c.config.RecycleCooldown {
		return decision
	}

	c.lastRecycle = now
	decision.Recycle = true
	decision.Reason = "delivery-progress-stall"
	return decision
}

func (c *volgaV6RecoveryController) refillRetryLocked(now time.Time) {
	if c.retryRefill.IsZero() {
		c.retryRefill = now
		return
	}
	if !now.After(c.retryRefill) {
		return
	}
	elapsed := now.Sub(c.retryRefill).Seconds()
	c.retryTokens += elapsed * c.config.RetryRatePerSecond
	if max := float64(c.config.RetryBurst); c.retryTokens > max {
		c.retryTokens = max
	}
	c.retryRefill = now
}

// TakeRetryBudget enforces a transport-wide repair budget. This is separate
// from DueRepairs(), which decides which logical holes are old enough to retry.
// Both gates must pass before a repair POST is emitted.
func (c *volgaV6RecoveryController) TakeRetryBudget(now time.Time, requested int) int {
	if requested <= 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refillRetryLocked(now)
	available := int(math.Floor(c.retryTokens))
	if available <= 0 {
		return 0
	}
	if requested < available {
		available = requested
	}
	c.retryTokens -= float64(available)
	return available
}

func (c *volgaV6RecoveryController) ResetRetryBudget(now time.Time) {
	c.mu.Lock()
	c.retryTokens = float64(c.config.RetryBurst)
	c.retryRefill = now
	c.mu.Unlock()
}
