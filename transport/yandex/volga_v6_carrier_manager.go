package yandex

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	errVolgaV6NoActiveCarrier = errors.New("volga v6 no active carrier")
	errVolgaV6ActiveRetire    = errors.New("volga v6 cannot retire active carrier")
)

// volgaV6PhysicalCarrier is one disposable Yandex authorization/request-path/
// websocket generation. It deliberately owns no logical replay or ACK state.
type volgaV6PhysicalCarrier interface {
	Generation() uint64
	Start(ctx context.Context) error
	SendVolgaV6(frame volgaV6WireFrame) error
	Stop() error
}

type volgaV6PhysicalHealth struct {
	Known         bool
	Generation    uint64
	Document      string
	Connected     bool
	Age           time.Duration
	Posts         uint64
	PostFailures  uint64
	PostBytes     uint64
	PostMicros    uint64
	MaxPostMicros uint64
	WSReconnects  uint64
}

type volgaV6PhysicalHealthReporter interface {
	VolgaV6PhysicalHealth(now time.Time) volgaV6PhysicalHealth
}

type volgaV6CarrierFactory func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error)

type volgaV6CarrierManagerSnapshot struct {
	ActiveGeneration uint64
	Draining         []uint64
	Handoffs         uint64
	ActiveHealth     volgaV6PhysicalHealth
}

// volgaV6CarrierManager owns only physical carrier lifetime. ReliableSession
// implements logical lifetime above it. This object may replace Yandex sessions
// without changing logical session ID, sequence numbers or replay contents.
type volgaV6CarrierManager struct {
	factory volgaV6CarrierFactory
	onFrame func(volgaV6WireFrame)

	// lifecycleMu serializes Start/Handoff/Stop. In particular, two concurrent
	// fresh-authorize handoffs must never race their active-pointer swap.
	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	active      volgaV6PhysicalCarrier
	activeSince time.Time
	draining    map[uint64]volgaV6PhysicalCarrier
	nextGen     uint64
	handoffs    uint64
	stopped     bool
}

func newVolgaV6CarrierManager(factory volgaV6CarrierFactory, onFrame func(volgaV6WireFrame)) *volgaV6CarrierManager {
	return &volgaV6CarrierManager{
		factory:  factory,
		onFrame:  onFrame,
		draining: make(map[uint64]volgaV6PhysicalCarrier),
	}
}

func (m *volgaV6CarrierManager) newCarrier(ctx context.Context) (volgaV6PhysicalCarrier, error) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil, context.Canceled
	}
	// Generations are monotonic and intentionally not reused after a failed
	// authorization/start attempt.
	m.nextGen++
	generation := m.nextGen
	factory := m.factory
	onFrame := m.onFrame
	m.mu.Unlock()

	if factory == nil {
		return nil, fmt.Errorf("volga v6 carrier factory is nil")
	}
	carrier, err := factory(generation, onFrame)
	if err != nil {
		return nil, err
	}
	if carrier == nil {
		return nil, fmt.Errorf("volga v6 carrier factory returned nil generation=%d", generation)
	}
	if carrier.Generation() != generation {
		_ = carrier.Stop()
		return nil, fmt.Errorf("volga v6 carrier generation mismatch got=%d want=%d", carrier.Generation(), generation)
	}
	if err := carrier.Start(ctx); err != nil {
		_ = carrier.Stop()
		return nil, err
	}
	return carrier, nil
}

func (m *volgaV6CarrierManager) Start(ctx context.Context) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	carrier, err := m.newCarrier(ctx)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		_ = carrier.Stop()
		return context.Canceled
	}
	if m.active != nil {
		_ = carrier.Stop()
		return fmt.Errorf("volga v6 carrier manager already started")
	}
	m.active = carrier
	m.activeSince = time.Now()
	return nil
}

// Handoff is make-before-break. The replacement carrier must fully Start()
// before the active pointer is changed. The previous active carrier becomes
// receive-capable draining state and is not stopped here.
func (m *volgaV6CarrierManager) Handoff(ctx context.Context) (oldGeneration, newGeneration uint64, err error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	replacement, err := m.newCarrier(ctx)
	if err != nil {
		return 0, 0, err
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		_ = replacement.Stop()
		return 0, 0, context.Canceled
	}
	old := m.active
	if old == nil {
		m.active = replacement
		m.activeSince = time.Now()
		m.mu.Unlock()
		return 0, replacement.Generation(), nil
	}
	m.draining[old.Generation()] = old
	m.active = replacement
	m.activeSince = time.Now()
	m.handoffs++
	oldGeneration = old.Generation()
	newGeneration = replacement.Generation()
	m.mu.Unlock()
	return oldGeneration, newGeneration, nil
}

func (m *volgaV6CarrierManager) SendVolgaV6(frame volgaV6WireFrame) error {
	m.mu.RLock()
	carrier := m.active
	stopped := m.stopped
	m.mu.RUnlock()
	if stopped || carrier == nil {
		return errVolgaV6NoActiveCarrier
	}
	return carrier.SendVolgaV6(frame)
}

func (m *volgaV6CarrierManager) Retire(generation uint64) error {
	m.mu.Lock()
	if m.active != nil && m.active.Generation() == generation {
		m.mu.Unlock()
		return errVolgaV6ActiveRetire
	}
	carrier := m.draining[generation]
	if carrier != nil {
		delete(m.draining, generation)
	}
	m.mu.Unlock()
	if carrier == nil {
		return nil
	}
	return carrier.Stop()
}

func (m *volgaV6CarrierManager) Snapshot() volgaV6CarrierManagerSnapshot {
	return m.snapshotAt(time.Now())
}

func (m *volgaV6CarrierManager) snapshotAt(now time.Time) volgaV6CarrierManagerSnapshot {
	m.mu.RLock()
	activeCarrier := m.active
	activeSince := m.activeSince
	active := uint64(0)
	if activeCarrier != nil {
		active = activeCarrier.Generation()
	}
	draining := make([]uint64, 0, len(m.draining))
	for generation := range m.draining {
		draining = append(draining, generation)
	}
	handoffs := m.handoffs
	m.mu.RUnlock()

	sort.Slice(draining, func(i, j int) bool { return draining[i] < draining[j] })
	health := volgaV6PhysicalHealth{}
	if reporter, ok := activeCarrier.(volgaV6PhysicalHealthReporter); ok {
		health = reporter.VolgaV6PhysicalHealth(now)
	}
	if !activeSince.IsZero() && now.After(activeSince) {
		health.Age = now.Sub(activeSince)
	}
	return volgaV6CarrierManagerSnapshot{
		ActiveGeneration: active,
		Draining:         draining,
		Handoffs:         handoffs,
		ActiveHealth:     health,
	}
}

func (m *volgaV6CarrierManager) Stop() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	active := m.active
	m.active = nil
	m.activeSince = time.Time{}
	draining := make([]volgaV6PhysicalCarrier, 0, len(m.draining))
	for _, carrier := range m.draining {
		draining = append(draining, carrier)
	}
	m.draining = make(map[uint64]volgaV6PhysicalCarrier)
	m.mu.Unlock()

	var firstErr error
	if active != nil {
		if err := active.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, carrier := range draining {
		if err := carrier.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
