package yandex

import (
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	volgaV6AckMaxRanges = 32
)

var (
	errVolgaV6WindowFull = errors.New("volga v6 reliable window full")
	errVolgaV6UnknownSeq = errors.New("volga v6 unknown replay sequence")
)

type volgaV6FrameKind uint8

const (
	volgaV6FrameData volgaV6FrameKind = iota + 1
	volgaV6FrameAck
)

type volgaV6AckRange struct {
	Start uint64
	End   uint64
}

type volgaV6Ack struct {
	Session uint64
	Base    uint64
	Ranges  []volgaV6AckRange
}

type volgaV6WireFrame struct {
	Kind    volgaV6FrameKind
	Session uint64
	Seq     uint64
	Floor   uint64
	Payload [][]byte
	Ack     volgaV6Ack
}

// volgaV6WireSender is deliberately unaware of logical replay ownership.
// CarrierManager will implement this interface and choose the current physical
// Yandex generation at the instant a frame is sent.
type volgaV6WireSender interface {
	SendVolgaV6(frame volgaV6WireFrame) error
}

type volgaV6ReliableConfig struct {
	MaxInFlight int
	RetryBurst  int
	BaseRTO     time.Duration
	MaxBackoff  time.Duration
}

func defaultVolgaV6ReliableConfig() volgaV6ReliableConfig {
	return volgaV6ReliableConfig{
		MaxInFlight: 512,
		RetryBurst:  8,
		BaseRTO:     350 * time.Millisecond,
		MaxBackoff:  4 * time.Second,
	}
}

type volgaV6ReplayEntry struct {
	payload   [][]byte
	firstSent time.Time
	lastSent  time.Time
	retries   int
}

type volgaV6ReliableSnapshot struct {
	Session          uint64
	NextSeq          uint64
	AckBase          uint64
	ReplayDepth      int
	OldestUnackedAge time.Duration
	Retries          uint64
}

// volgaV6ReliableSession owns one logical sender lifetime. Physical Yandex
// carrier replacement must never replace this object.
type volgaV6ReliableSession struct {
	sessionID uint64
	sender    volgaV6WireSender
	config    volgaV6ReliableConfig

	mu       sync.Mutex
	nextSeq  uint64
	ackBase  uint64
	replay   map[uint64]*volgaV6ReplayEntry
	retries  uint64
}

func newVolgaV6ReliableSession(sessionID uint64, sender volgaV6WireSender, cfg volgaV6ReliableConfig) *volgaV6ReliableSession {
	if sessionID == 0 {
		sessionID = uint64(time.Now().UnixNano())
		if sessionID == 0 {
			sessionID = 1
		}
	}
	defaults := defaultVolgaV6ReliableConfig()
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = defaults.MaxInFlight
	}
	if cfg.RetryBurst <= 0 {
		cfg.RetryBurst = defaults.RetryBurst
	}
	if cfg.BaseRTO <= 0 {
		cfg.BaseRTO = defaults.BaseRTO
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaults.MaxBackoff
	}
	return &volgaV6ReliableSession{
		sessionID: sessionID,
		sender:    sender,
		config:    cfg,
		replay:    make(map[uint64]*volgaV6ReplayEntry),
	}
}

func cloneVolgaV6Payload(payload [][]byte) [][]byte {
	out := make([][]byte, len(payload))
	for i := range payload {
		out[i] = append([]byte(nil), payload[i]...)
	}
	return out
}

func (s *volgaV6ReliableSession) replayFloorLocked(fallback uint64) uint64 {
	floor := uint64(0)
	for seq := range s.replay {
		if floor == 0 || seq < floor {
			floor = seq
		}
	}
	if floor == 0 {
		floor = fallback
	}
	return floor
}

func (s *volgaV6ReliableSession) Send(payload [][]byte) (uint64, error) {
	return s.sendAt(payload, time.Now())
}

func (s *volgaV6ReliableSession) sendAt(payload [][]byte, now time.Time) (uint64, error) {
	s.mu.Lock()
	if len(s.replay) >= s.config.MaxInFlight {
		s.mu.Unlock()
		return 0, errVolgaV6WindowFull
	}
	s.nextSeq++
	seq := s.nextSeq
	s.replay[seq] = &volgaV6ReplayEntry{
		payload:   cloneVolgaV6Payload(payload),
		firstSent: now,
		lastSent:  now,
	}
	floor := s.replayFloorLocked(seq)
	s.mu.Unlock()

	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: s.sessionID,
		Seq:     seq,
		Floor:   floor,
		Payload: cloneVolgaV6Payload(payload),
	}
	if s.sender == nil {
		return seq, nil
	}
	return seq, s.sender.SendVolgaV6(frame)
}

func volgaV6AckContains(ack volgaV6Ack, seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq <= ack.Base {
		return true
	}
	for _, rg := range ack.Ranges {
		if seq >= rg.Start && seq <= rg.End {
			return true
		}
	}
	return false
}

func (s *volgaV6ReliableSession) HandleAck(ack volgaV6Ack) int {
	if ack.Session != s.sessionID {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if ack.Base > s.ackBase {
		s.ackBase = ack.Base
	}
	released := 0
	for seq := range s.replay {
		if volgaV6AckContains(ack, seq) {
			delete(s.replay, seq)
			released++
		}
	}
	return released
}

func (s *volgaV6ReliableSession) retryDelayLocked(entry *volgaV6ReplayEntry) time.Duration {
	delay := s.config.BaseRTO
	shift := entry.retries
	if shift > 4 {
		shift = 4
	}
	delay *= time.Duration(1 << shift)
	if delay > s.config.MaxBackoff {
		delay = s.config.MaxBackoff
	}
	return delay
}

// DueRepairs is a deterministic selector. It does not start goroutines and it
// does not send. A future recovery loop can call it on a timer and apply a
// separate rate budget without coupling retry policy to a Yandex carrier.
func (s *volgaV6ReliableSession) DueRepairs(now time.Time) []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	seqs := make([]uint64, 0, len(s.replay))
	for seq, entry := range s.replay {
		if entry.lastSent.IsZero() {
			continue
		}
		if now.Sub(entry.lastSent) >= s.retryDelayLocked(entry) {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if len(seqs) > s.config.RetryBurst {
		seqs = seqs[:s.config.RetryBurst]
	}
	return seqs
}

func (s *volgaV6ReliableSession) Replay(seq uint64) error {
	return s.replayAt(seq, time.Now())
}

func (s *volgaV6ReliableSession) replayAt(seq uint64, now time.Time) error {
	s.mu.Lock()
	entry := s.replay[seq]
	if entry == nil {
		s.mu.Unlock()
		return errVolgaV6UnknownSeq
	}
	entry.lastSent = now
	entry.retries++
	s.retries++
	floor := s.replayFloorLocked(seq)
	payload := cloneVolgaV6Payload(entry.payload)
	s.mu.Unlock()

	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: s.sessionID,
		Seq:     seq,
		Floor:   floor,
		Payload: payload,
	}
	if s.sender == nil {
		return nil
	}
	return s.sender.SendVolgaV6(frame)
}

func (s *volgaV6ReliableSession) Snapshot(now time.Time) volgaV6ReliableSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldest := time.Time{}
	for _, entry := range s.replay {
		if oldest.IsZero() || entry.firstSent.Before(oldest) {
			oldest = entry.firstSent
		}
	}
	age := time.Duration(0)
	if !oldest.IsZero() && now.After(oldest) {
		age = now.Sub(oldest)
	}
	return volgaV6ReliableSnapshot{
		Session:          s.sessionID,
		NextSeq:          s.nextSeq,
		AckBase:          s.ackBase,
		ReplayDepth:      len(s.replay),
		OldestUnackedAge: age,
		Retries:          s.retries,
	}
}

type volgaV6ReceiverSnapshot struct {
	PeerSession uint64
	Base        uint64
	Pending     int
	Duplicates uint64
	Resets      uint64
	Stale       uint64
}

// volgaV6ReliableReceiver owns receive reliability independently from any
// websocket. Every active or draining carrier feeds the same instance.
type volgaV6ReliableReceiver struct {
	mu sync.Mutex

	peerSession uint64
	base        uint64
	seen        map[uint64]struct{}

	duplicates uint64
	resets     uint64
	stale      uint64

	onData func([][]byte)
}

func newVolgaV6ReliableReceiver(onData func([][]byte)) *volgaV6ReliableReceiver {
	return &volgaV6ReliableReceiver{
		seen:   make(map[uint64]struct{}),
		onData: onData,
	}
}

func (r *volgaV6ReliableReceiver) Accept(frame volgaV6WireFrame) (volgaV6Ack, bool) {
	if frame.Kind != volgaV6FrameData || frame.Session == 0 || frame.Seq == 0 || frame.Floor == 0 || frame.Floor > frame.Seq {
		return volgaV6Ack{}, false
	}

	deliver := false
	r.mu.Lock()
	if r.peerSession == 0 || frame.Session > r.peerSession {
		r.peerSession = frame.Session
		r.base = frame.Floor - 1
		r.seen = make(map[uint64]struct{})
		r.resets++
	} else if frame.Session < r.peerSession {
		r.stale++
		ack := r.ackSnapshotLocked()
		r.mu.Unlock()
		return ack, false
	}

	if frame.Seq <= r.base {
		r.duplicates++
	} else if _, exists := r.seen[frame.Seq]; exists {
		r.duplicates++
	} else {
		r.seen[frame.Seq] = struct{}{}
		deliver = true
		for {
			next := r.base + 1
			if _, ok := r.seen[next]; !ok {
				break
			}
			delete(r.seen, next)
			r.base = next
		}
	}
	ack := r.ackSnapshotLocked()
	r.mu.Unlock()

	if deliver && r.onData != nil {
		r.onData(cloneVolgaV6Payload(frame.Payload))
	}
	return ack, deliver
}

func (r *volgaV6ReliableReceiver) ackSnapshotLocked() volgaV6Ack {
	if r.peerSession == 0 {
		return volgaV6Ack{}
	}
	keys := make([]uint64, 0, len(r.seen))
	for seq := range r.seen {
		if seq > r.base {
			keys = append(keys, seq)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	ranges := make([]volgaV6AckRange, 0, volgaV6AckMaxRanges)
	for _, seq := range keys {
		if len(ranges) == 0 || seq > ranges[len(ranges)-1].End+1 {
			if len(ranges) >= volgaV6AckMaxRanges {
				break
			}
			ranges = append(ranges, volgaV6AckRange{Start: seq, End: seq})
			continue
		}
		ranges[len(ranges)-1].End = seq
	}
	return volgaV6Ack{
		Session: r.peerSession,
		Base:    r.base,
		Ranges:  ranges,
	}
}

// AckSnapshot is intentionally repeatable. V6 ACK is state, not a one-shot
// event, so the same logical ACK may be sent again if a previous control frame
// was accepted by Yandex but not observed by the peer.
func (r *volgaV6ReliableReceiver) AckSnapshot() (volgaV6Ack, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ack := r.ackSnapshotLocked()
	return ack, ack.Session != 0
}

func (r *volgaV6ReliableReceiver) Snapshot() volgaV6ReceiverSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return volgaV6ReceiverSnapshot{
		PeerSession: r.peerSession,
		Base:        r.base,
		Pending:     len(r.seen),
		Duplicates: r.duplicates,
		Resets:      r.resets,
		Stale:       r.stale,
	}
}
