//go:build volga

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
	Reliable            volgaV6ReliableConfig
	Recovery            volgaV6RecoveryConfig
	AckRepeatInterval   time.Duration
	AckRepeatWindow     time.Duration // repeat an unchanged ACK only this long after the last peer frame
	DrainGrace          time.Duration
	CarrierStartTimeout time.Duration
	RepairWorkers       int

	// Quiet (a server) waits silently while no peer frame has arrived for
	// QuietAfter, or none since start: its outstanding DATA then has no
	// receiver. It sends no repairs. Every carrier pings its editor session
	// once a minute, as an open browser tab does; without that ping Yandex
	// stopped delivering to an idle carrier about two minutes after its
	// authorization. The carriers are still renewed, each renewal a fresh
	// authorization of every lane: every QuietRecycleInterval as a safety
	// net, and after a push socket reconnect or a refused ping. A peer frame
	// restores normal recovery; a live session's yamux keepalives arrive every
	// 5 s. A client keeps fast recovery.
	Quiet                bool
	QuietAfter           time.Duration
	QuietRecycleInterval time.Duration
	// ResubscribeSpacing is the least time between recycles caused by a
	// reconnected push socket; see Tick.
	ResubscribeSpacing time.Duration

	// credentialsChanged, when set, reports that a CAPTCHA/login-latched
	// document has fresh cookies in the store. Tick then renews at once
	// instead of waiting for the spaced retry.
	credentialsChanged func() bool
}

// credentialCheckEvery bounds how often a blocked runtime reads the cookie
// store; maxQuietRenewalPause caps the doubled quiet renewal pause.
const (
	credentialCheckEvery = 5 * time.Second
	maxQuietRenewalPause = time.Hour
)

func defaultVolgaV6RuntimeConfig() volgaV6RuntimeConfig {
	return volgaV6RuntimeConfig{
		Reliable:             defaultVolgaV6ReliableConfig(),
		Recovery:             defaultVolgaV6RecoveryConfig(),
		AckRepeatInterval:    100 * time.Millisecond,
		AckRepeatWindow:      10 * time.Second,
		DrainGrace:           2 * time.Second,
		CarrierStartTimeout:  15 * time.Second,
		RepairWorkers:        4,
		QuietAfter:           30 * time.Second,
		QuietRecycleInterval: 20 * time.Minute,
		ResubscribeSpacing:   time.Minute,
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

	AckSent           uint64
	AckSendFailures   uint64
	RepairsSent       uint64
	LastHandoffReason string
	WaitingForPeer    bool
}

// volgaV6Runtime joins the logical sender/receiver, recovery controller and
// physical carrier manager without allowing physical generation state to own
// logical replay. It remains independent of the concrete Yandex wire adapter.
type volgaV6Runtime struct {
	config volgaV6RuntimeConfig

	manager   *volgaV6CarrierManager
	session   *volgaV6ReliableSession
	receiver  *volgaV6ReliableReceiver
	recovery  *volgaV6RecoveryController
	fragments volgaV6Reassembler

	ackSent         atomic.Uint64
	ackSendFailures atomic.Uint64
	repairsSent     atomic.Uint64
	ackDirty        atomic.Bool
	ackMu           sync.Mutex
	peerActivity    atomic.Bool  // a peer frame arrived since the last Tick
	lastInbound     atomic.Int64 // UnixNano of the last peer frame

	mu                sync.Mutex
	lastAckSent       time.Time
	lastHandoffReason string
	retireAt          map[uint64]time.Time
	lastRenewal       time.Time // start, the last handoff or the last renewal attempt
	renewFailures     int       // failed renewal attempts in a row; each doubles the spacing
	lastCredCheck     time.Time
}

func newVolgaV6Runtime(sessionID uint64, factory volgaV6CarrierFactory, cfg volgaV6RuntimeConfig, onData func([][]byte)) *volgaV6Runtime {
	defaults := defaultVolgaV6RuntimeConfig()
	if cfg.AckRepeatInterval <= 0 {
		cfg.AckRepeatInterval = defaults.AckRepeatInterval
	}
	if cfg.AckRepeatWindow <= 0 {
		cfg.AckRepeatWindow = defaults.AckRepeatWindow
	}
	if cfg.DrainGrace <= 0 {
		cfg.DrainGrace = defaults.DrainGrace
	}
	if cfg.CarrierStartTimeout <= 0 {
		cfg.CarrierStartTimeout = defaults.CarrierStartTimeout
	}
	if cfg.RepairWorkers <= 0 {
		cfg.RepairWorkers = defaults.RepairWorkers
	}
	cfg.RepairWorkers = min(cfg.RepairWorkers, 8)
	if cfg.QuietAfter <= 0 {
		cfg.QuietAfter = defaults.QuietAfter
	}
	if cfg.QuietRecycleInterval <= 0 {
		cfg.QuietRecycleInterval = defaults.QuietRecycleInterval
	}
	if cfg.ResubscribeSpacing <= 0 {
		cfg.ResubscribeSpacing = defaults.ResubscribeSpacing
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
	ctx, cancel := context.WithTimeout(ctx, r.config.CarrierStartTimeout)
	defer cancel()
	if err := r.manager.Start(ctx); err != nil {
		return err
	}
	// Establish a progress clock while the stream is idle. Once DATA becomes
	// outstanding, Observe stops refreshing this timestamp until cumulative ACK
	// advances again.
	r.recovery.Observe(now, r.session.Snapshot(now))
	r.mu.Lock()
	r.lastAckSent = now
	r.lastRenewal = now
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
	// Authorization changes the provider's user ID on handoff. A fresh socket
	// can therefore receive our old carrier's DATA as another user's relay.
	// Logical session identity survives handoff and is the correct echo filter.
	// ACKs intentionally address our session and must still be accepted.
	if (frame.Kind == volgaV6FrameData || frame.Kind == volgaV6FrameFragment) && frame.Session == r.session.sessionID {
		return
	}
	// Peer DATA, or an ACK of our own session, proves the peer is present.
	if frame.Kind == volgaV6FrameData || frame.Kind == volgaV6FrameFragment || (frame.Kind == volgaV6FrameAck && frame.Ack.Session == r.session.sessionID) {
		r.lastInbound.Store(time.Now().UnixNano())
		r.peerActivity.Store(true)
	}
	if frame.Kind == volgaV6FrameFragment {
		// Completed/old sequences need no reassembly storage. Repeating the
		// cumulative ACK lets a peer recover when the last ACK was lost.
		snap := r.receiver.Snapshot()
		if frame.Session < snap.PeerSession {
			return
		}
		if frame.Session == snap.PeerSession && frame.Seq <= snap.Base {
			r.ackDirty.Store(true)
			return
		}
		var ok bool
		frame, ok = r.fragments.Accept(frame, time.Now())
		if !ok {
			return
		}
	}
	switch frame.Kind {
	case volgaV6FrameData:
		ack, ok := r.receiver.Accept(frame)
		if !ok && ack.Session == 0 {
			return
		}
		// ACK is state, not a per-DATA synchronous response. Mark it dirty and
		// let the ACK worker coalesce received DATA into one cumulative ACK.
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
	// Tick and the independent ACK worker may call this concurrently. Never
	// queue another ACK behind an in-flight POST.
	if !r.ackMu.TryLock() {
		return nil
	}
	defer r.ackMu.Unlock()
	dirty := r.ackDirty.Swap(false)
	// A repeated ACK only helps a peer that may still be waiting for it. After
	// AckRepeatWindow of silence the peer is gone or idle; its repairs would
	// mark the ACK dirty again.
	if !dirty && now.Sub(time.Unix(0, r.lastInbound.Load())) > r.config.AckRepeatWindow {
		return nil
	}

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
		r.ackDirty.Store(true)
		return err
	}

	r.mu.Lock()
	r.lastAckSent = now
	r.mu.Unlock()
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
	if r.peerActivity.Swap(false) {
		r.recovery.PeerActive()
	}
	result.Retired = r.retireDue(now)
	health := r.manager.snapshotAt(now).ActiveHealth
	if now.Before(health.RelayRetryAt) {
		// An explicit provider cooldown is not evidence of a dead carrier.
		r.recovery.deferProgress(now)
		result.Recovery.Reason = "provider-rate-limit"
		return result
	}
	if r.credentialsChangedAt(now) {
		result.Recovery.Reason, result.Recovery.Recycle = "credentials-changed", true
		if r.renew(ctx, now, &result, result.Recovery.Reason) {
			r.recovery.ResetRetryBudget(now)
		}
		return result
	}
	snap := r.session.Snapshot(now)
	if r.quietAt(now) {
		return r.waitForPeer(ctx, now, snap, health, result)
	}
	result.Recovery = r.recovery.Observe(now, snap)
	renewed := false
	switch {
	case result.Recovery.Recycle:
		renewed = r.handoff(ctx, now, &result, result.Recovery.Reason)
	case r.resubscribeDue(now, health):
		result.Recovery.Reason, result.Recovery.Recycle = renewalReason(health), true
		renewed = r.renew(ctx, now, &result, result.Recovery.Reason)
	}
	if renewed {
		// Permit a bounded immediate repair burst on the newly authorized
		// carrier. Subsequent repairs are rate-refilled normally.
		r.recovery.ResetRetryBudget(now)
	}

	due := r.session.DueRepairs(now)
	allowed := r.recovery.TakeRetryBudget(now, len(due))
	// A serial repair loop is limited to 1/HTTP RTT regardless of the retry
	// budget. Keep bounded concurrency while preserving that existing budget.
	jobs := make(chan uint64, allowed)
	results := make(chan error, allowed)
	for _, seq := range due[:allowed] {
		jobs <- seq
	}
	close(jobs)
	for worker := 0; worker < min(allowed, r.config.RepairWorkers); worker++ {
		go func() {
			for seq := range jobs {
				results <- r.session.replayAt(seq, now)
			}
		}()
	}
	for i := 0; i < allowed; i++ {
		if err := <-results; err != nil {
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

// handoff replaces the physical carrier generation and reports success.
func (r *volgaV6Runtime) handoff(ctx context.Context, now time.Time, result *volgaV6RuntimeTickResult, reason string) bool {
	startCtx, cancel := context.WithTimeout(ctx, r.config.CarrierStartTimeout)
	oldGen, newGen, err := r.manager.Handoff(startCtx)
	cancel()
	if err != nil {
		result.HandoffErr = err
		return false
	}
	result.Handoff = true
	result.OldGeneration = oldGen
	result.NewGeneration = newGen
	r.scheduleRetire(oldGen, now)
	r.mu.Lock()
	r.lastHandoffReason = reason
	r.lastRenewal = now
	r.renewFailures = 0
	r.mu.Unlock()
	return true
}

// renew is a handoff that is not caused by a delivery stall. Its spacing counts
// from the attempt, so a failing authorization (a CAPTCHA, for example) is
// not retried every tick.
func (r *volgaV6Runtime) renew(ctx context.Context, now time.Time, result *volgaV6RuntimeTickResult, reason string) bool {
	r.mu.Lock()
	r.lastRenewal = now
	r.mu.Unlock()
	if r.handoff(ctx, now, result, reason) {
		return true
	}
	r.mu.Lock()
	r.renewFailures++
	r.mu.Unlock()
	return false
}

// renewalAge is the time since start, the last handoff or the last renewal
// attempt, and the number of failed renewals in a row.
func (r *volgaV6Runtime) renewalAge(now time.Time) (time.Duration, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.Sub(r.lastRenewal), r.renewFailures
}

// quietAt reports whether a Quiet runtime has no peer to serve: no peer frame
// since start, or none for QuietAfter.
func (r *volgaV6Runtime) quietAt(now time.Time) bool {
	if !r.config.Quiet {
		return false
	}
	last := r.lastInbound.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= r.config.QuietAfter
}

// resubscribeDue reports that the active carrier's push socket has reconnected
// since the last authorization, or that Yandex refused a session ping. A
// reconnected socket reuses the subscription signed at that authorization;
// once that is stale it connects but stays silent. On AWS it went silent 5-12
// min after each authorization, and a server waiting for a client missed it.
// A refused ping means the editor session itself is gone. Only a fresh
// authorization restores delivery. Spacing doubles after each failed renewal,
// up to 32 times.
func (r *volgaV6Runtime) resubscribeDue(now time.Time, health volgaV6PhysicalHealth) bool {
	age, failures := r.renewalAge(now)
	return (health.WSReconnects > 0 || health.SessionPingRejected > 0) && age >= r.config.ResubscribeSpacing<<min(failures, 5)
}

// credentialsChangedAt checks the latched documents' cookies at most every
// credentialCheckEvery. New cookies arrive by hand or through the Disk inbox
// while the runtime may be deep in a doubled renewal pause.
func (r *volgaV6Runtime) credentialsChangedAt(now time.Time) bool {
	if r.config.credentialsChanged == nil {
		return false
	}
	r.mu.Lock()
	due := now.Sub(r.lastCredCheck) >= credentialCheckEvery
	if due {
		r.lastCredCheck = now
	}
	r.mu.Unlock()
	return due && r.config.credentialsChanged()
}

func renewalReason(health volgaV6PhysicalHealth) string {
	if health.SessionPingRejected > 0 {
		return "session-ping-rejected"
	}
	return "websocket-resubscribe"
}

// waitForPeer keeps a Quiet runtime silent: no repairs or ACK repeats. It
// renews its carriers only to keep hearing a client: after a push socket
// reconnect or a refused session ping, and once QuietRecycleInterval has
// passed since the last renewal. Both pauses double after each failed renewal.
// The progress clock stays fresh so a returning peer is not met with an
// instant stall verdict.
func (r *volgaV6Runtime) waitForPeer(ctx context.Context, now time.Time, snap volgaV6ReliableSnapshot, health volgaV6PhysicalHealth, result volgaV6RuntimeTickResult) volgaV6RuntimeTickResult {
	r.recovery.deferProgress(now)
	result.Recovery = volgaV6RecoveryDecision{AdmissionLimit: r.recovery.AdmissionLimit(snap.ReplayDepth), Reason: "waiting-for-peer"}
	age, failures := r.renewalAge(now)
	switch {
	case r.resubscribeDue(now, health):
		result.Recovery.Reason = renewalReason(health)
	case age >= min(r.config.QuietRecycleInterval<<min(failures, 5), maxQuietRenewalPause):
		result.Recovery.Reason = "quiet-keepalive"
	default:
		return result
	}
	result.Recovery.Recycle = true
	r.renew(ctx, now, &result, result.Recovery.Reason)
	return result
}

func (r *volgaV6Runtime) Snapshot(now time.Time) volgaV6RuntimeSnapshot {
	r.mu.Lock()
	lastReason := r.lastHandoffReason
	r.mu.Unlock()
	return volgaV6RuntimeSnapshot{
		Reliable:          r.session.Snapshot(now),
		Receiver:          r.receiver.Snapshot(),
		Carrier:           r.manager.snapshotAt(now),
		AckSent:           r.ackSent.Load(),
		AckSendFailures:   r.ackSendFailures.Load(),
		RepairsSent:       r.repairsSent.Load(),
		LastHandoffReason: lastReason,
		WaitingForPeer:    r.quietAt(now),
	}
}
