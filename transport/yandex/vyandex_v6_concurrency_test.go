package yandex

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

type blockingVolgaV6Carrier struct {
	generation uint64
	entered    chan struct{}
	release    chan struct{}
	stoppedCh  chan struct{}
	stopOnce   sync.Once

	started   atomic.Bool
	stopped   atomic.Bool
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func newBlockingVolgaV6Carrier(generation uint64) *blockingVolgaV6Carrier {
	return &blockingVolgaV6Carrier{
		generation: generation,
		entered:    make(chan struct{}, 64),
		release:    make(chan struct{}),
		stoppedCh:  make(chan struct{}),
	}
}

func (c *blockingVolgaV6Carrier) Generation() uint64 { return c.generation }

func (c *blockingVolgaV6Carrier) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.started.Store(true)
	return nil
}

func (c *blockingVolgaV6Carrier) SendVolgaV6(frame volgaV6WireFrame) error {
	if !c.started.Load() || c.stopped.Load() {
		return context.Canceled
	}
	current := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		maximum := c.maxFlight.Load()
		if current <= maximum || c.maxFlight.CompareAndSwap(maximum, current) {
			break
		}
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
		return nil
	case <-c.stoppedCh:
		return context.Canceled
	}
}

func (c *blockingVolgaV6Carrier) Stop() error {
	c.stopOnce.Do(func() {
		c.stopped.Store(true)
		close(c.stoppedCh)
	})
	return nil
}

func waitBlockingCarrierEntries(t *testing.T, carrier *blockingVolgaV6Carrier, count int) {
	t.Helper()
	deadline := time.After(time.Second)
	for i := 0; i < count; i++ {
		select {
		case <-carrier.entered:
		case <-deadline:
			t.Fatalf("saw %d/%d physical sends before timeout", i, count)
		}
	}
}

func TestVolgaV6OuterTransportUsesBoundedParallelPhysicalSends(t *testing.T) {
	var carrier *blockingVolgaV6Carrier
	factory := func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		carrier = newBlockingVolgaV6Carrier(generation)
		return carrier, nil
	}
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.BatchPackets = 1
	cfg.BatchTimeout = time.Millisecond
	cfg.SendWorkers = 4
	cfg.SendQueueSize = 16
	cfg.QueueSize = 16
	cfg.TickInterval = time.Second
	cfg.Runtime.Recovery.ProgressStall = 10 * time.Second

	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factory)
	if err := v6.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v6.Stop() }()

	for i := 0; i < 4; i++ {
		if err := v6.Send([]byte{byte(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	waitBlockingCarrierEntries(t, carrier, 4)
	if got := carrier.maxFlight.Load(); got != 4 {
		t.Fatalf("max concurrent physical sends=%d want 4", got)
	}

	close(carrier.release)
	deadline := time.Now().Add(time.Second)
	for v6.Stats().PacketsSent != 4 {
		if time.Now().After(deadline) {
			t.Fatalf("packets sent=%d want 4", v6.Stats().PacketsSent)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestVolgaV6OuterTransportStopCancelsBlockedPhysicalSend(t *testing.T) {
	var carrier *blockingVolgaV6Carrier
	factory := func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		carrier = newBlockingVolgaV6Carrier(generation)
		return carrier, nil
	}
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.BatchPackets = 1
	cfg.BatchTimeout = time.Millisecond
	cfg.SendWorkers = 1
	cfg.SendQueueSize = 4
	cfg.QueueSize = 4
	cfg.TickInterval = time.Second

	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factory)
	if err := v6.Start(); err != nil {
		t.Fatal(err)
	}
	if err := v6.Send([]byte("blocked")); err != nil {
		t.Fatal(err)
	}
	waitBlockingCarrierEntries(t, carrier, 1)

	done := make(chan error, 1)
	go func() { done <- v6.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop waited for blocked physical send instead of cancelling carrier")
	}
	if !carrier.stopped.Load() {
		t.Fatal("physical carrier was not stopped")
	}
}
