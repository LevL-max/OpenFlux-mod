package yandex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var errVolgaV6RecoveryBackpressure = errors.New("volga v6 recovery backpressure")

type volgaV6RuntimeConfig struct {
	Reliable          volgaV6ReliableConfig
	Recovery          volgaV6RecoveryConfig
	AckRepeatInterval time.Duration
	DrainGrace        time.Duration
}

func defaultVolgaV6RuntimeConfig() volgaV6RuntimeConfig {
	return volgaV6RuntimeConfig{
		Reliable:          defaultVolgaV6ReliableConfig(),
		Recovery:          defaultVolgaV6RecoveryConfig(),
		AckRepeatInterval: 100 * time.Millisecond,
		DrainGrace:        2 * time.Second,
	}
}

type volgaV6RuntimeTickResult struct {
	Recovery      volgaV6RecoveryDecision
	Handoff       bool
	OldGeneration uint64
	NewGeneration uint64
	Repairs       int
	Retired       []uint64
	HandoffErr    error
	RepairErr     error
	AckErr        error
}

type volgaV6RuntimeSnapshot struct {
	Reliable volgaV6ReliableSnapshot
	Receiver volgaV6ReceiverSnapshot
	Carrier  volgaV6CarrierManagerSnapshot

	AckSent         uint64
	AckSendFailures uint64
	RepairsSent     uint64
}

// volgaV6Runtime joins the logical sender/receiver, recovery controller and
// physical carrier manager without allowing physical generation state to own
// logical replay. It remains independent of the concrete Yandex wire adapter.
type volgaV6Runtime struct {
	config volgaV6RuntimeConfig

	manager  *volgaV6CarrierManager
	session  *volgaV6ReliableSession
	receiver *volgaV6ReliableReceiver
	recovery *volgaV6RecoveryController

	ackSent         atomic.Uint64
	ackSendFailures atomic.Uint64
	repairsSent     atomic.Uint64
	ackDirty        atomic.Bool

	mu          sync.Mutex
	lastAckSent time.Time
	retireAt    map[uint64]time.Time
}

func newVolgaV6Runtime(sessionID uint64, factory volgaV6CarrierFactory, cfg volgaV6RuntimeConfig, onData func([][]byte)) *volgaV6Runtime {
	defaults := defaultVolgaV6RuntimeConfig()
	if cfg.AckRepeatInterval <= 0 {
		cfg.AckRepeatInterval = defaults.AckRepeatInterval
	}
	if cfg.DrainGrace <= 0 {
		cfg.DrainGrace = defaults.DrainGrace
	}

	runtime := &volgaV6Runtime{
		config:   cfg,
		recovery: newVolgaV6RecoveryController(cfg.Recovery),
		retireAt: make(map[uint64]time.Time),
	}
	runtime.receiver = newVolgaV6ReliableReceiver(onData)
	runtime.manager = newVolgaV6CarrierManager(factory, runtime.handleIncoming)
	runtime.session = newVolgaV6ReliableSession(sessionID, runtime.manager, cfg.Reliable)
	return runtime
}

func (r *volgaV6Runtime) Start(ctx context.Context) error {
	return r.startAt(ctx, time.Now())
}

func (r *volgaV6Runtime) startAt(ctx context.Context, now time.Time) error {
	if err := r.manager.Start(ctx); err != nil {
		return err
	}
	// Establish a progress clock while the stream is idle. Once DATA becomes
	// outstanding, Observe stops refreshing this timestamp until cumulative ACK
	// advances again.
	r.recovery.Observe(now, r.session.Snapshot(now))
	r.mu.Lock()
	r.lastAckSent = now
	r.mu.Unlock()
	return nil
}

func (r *volgaV6Runtime) Stop() error {
	return r.manager.Stop()
}

func (r *volgaV6Runtime) Send(payload [][]byte) (uint64, error) {
	return r.sendAt(payload, time.Now())
}

func (r *volgaV6Runtime) sendAt(payload [][]byte, now time.Time) (uint64, error) {
	snap := r.session.Snapshot(now)
	limit := r.recovery.AdmissionLimit(snap.ReplayDepth)
	if snap.ReplayDepth >= limit {
		return 0, errVolgaV6RecoveryBackpressure
	}
	return r.session.sendAt(payload, now)
}

func (r *volgaV6Runtime) handleIncoming(frame volgaV6WireFrame) {
	switch frame.Kind {
	case volgaV6FrameData:
		ack, ok := r.receiver.Accept(frame)
		if !ok && ack.Session == 0 {
			return
		}
		// ACK is state, not a per-DATA synchronous response. Mark it dirty and
		// let Tick coalesce many received DATA frames into one cumulative ACK.
		// This is important for Volga because sending an HTTP ACK while running
		// inside the websocket reader would block further websocket delivery.
		r.ackDirty.Store(true)
	case volgaV6FrameAck:
		r.session.HandleAck(frame.Ack)
	}
}

func (r *volgaV6Runtime) sendAck(ack volgaV6Ack) error {
	if ack.Session == 0 {
		return nil
	}
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameAck,
		Session: ack.Session,
		Ack:     ack,
	}
	if err := r.manager.SendVolgaV6(frame); err != nil {
		r.ackSendFailures.Add(1)
		return err
	}
	r.ackSent.Add(1)
	return nil
}

func (r *volgaV6Runtime) repeatAckIfDue(now time.Time) error {
	dirty := r.ackDirty.Load()

	r.mu.Lock()
	last := r.lastAckSent
	if !dirty && !last.IsZero() && now.Sub(last) < r.config.AckRepeatInterval {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	ack, ok := r.receiver.AckSnapshot()
	if !ok {
		return nil
	}
	if err := r.sendAck(ack); err != nil {
		// Keep dirty state on failure so the next Tick tries again.
		return err
	}

	r.mu.Lock()
	r.lastAckSent = now
	r.mu.Unlock()
	r.ackDirty.Store(false)
	return nil
}

func (r *volgaV6Runtime) retireDue(now time.Time) []uint64 {
	r.mu.Lock()
	var due []uint64
	for generation, deadline := range r.retireAt {
		if !now.Before(deadline) {
			due = append(due, generation)
			delete(r.retireAt, generation)
		}
	}
	r.mu.Unlock()

	retired := make([]uint64, 0, len(due))
	for _, generation := range due {
		if err := r.manager.Retire(generation); err == nil {
			retired = append(retired, generation)
		}
	}
	return retired
}

func (r *volgaV6Runtime) scheduleRetire(generation uint64, now time.Time) {
	if generation == 0 {
		return
	}
	r.mu.Lock()
	r.retireAt[generation] = now.Add(r.config.DrainGrace)
	r.mu.Unlock()
}

// Tick is the only place that turns a progress-stall decision into a physical
// carrier recycle. Repairs are selected and rate-budgeted after the handoff, so
// outstanding logical DATA can immediately be replayed through the fresh
// physical generation without changing logical sequence numbers.
func (r *volgaV6Runtime) Tick(ctx context.Context, now time.Time) volgaV6RuntimeTickResult {
	result := volgaV6RuntimeTickResult{}
	result.Retired = r.retireDue(now)

	snap := r.session.Snapshot(now)
	result.Recovery = r.recovery.Observe(now, snap)
	if result.Recovery.Recycle {
		oldGen, newGen, err := r.manager.Handoff(ctx)
		if err != nil {
			result.HandoffErr = err
		} else {
			result.Handoff = true
			result.OldGeneration = oldGen
			result.NewGeneration = newGen
			r.scheduleRetire(oldGen, now)
			// Permit a bounded immediate repair burst on the newly authorized
			// carrier. Subsequent repairs are rate-refilled normally.
			r.recovery.ResetRetryBudget(now)
		}
	}

	due := r.session.DueRepairs(now)
	allowed := r.recovery.TakeRetryBudget(now, len(due))
	for _, seq := range due[:allowed] {
		if err := r.session.replayAt(seq, now); err != nil {
			if result.RepairErr == nil {
				result.RepairErr = err
			}
			continue
		}
		result.Repairs++
		r.repairsSent.Add(1)
	}

	if err := r.repeatAckIfDue(now); err != nil {
		result.AckErr = err
	}
	return result
}

func (r *volgaV6Runtime) Snapshot(now time.Time) volgaV6RuntimeSnapshot {
	return volgaV6RuntimeSnapshot{
		Reliable:        r.session.Snapshot(now),
		Receiver:        r.receiver.Snapshot(),
		Carrier:         r.manager.Snapshot(),
		AckSent:         r.ackSent.Load(),
		AckSendFailures: r.ackSendFailures.Load(),
		RepairsSent:     r.repairsSent.Load(),
	}
}
