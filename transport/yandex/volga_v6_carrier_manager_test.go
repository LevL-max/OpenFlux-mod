package yandex

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeVolgaV6PhysicalCarrier struct {
	generation uint64
	onFrame    func(volgaV6WireFrame)
	startErr   error
	started    bool
	stopped    bool
	autoEmit   bool
	sent       []volgaV6WireFrame
}

func (c *fakeVolgaV6PhysicalCarrier) Generation() uint64 { return c.generation }

func (c *fakeVolgaV6PhysicalCarrier) Start(ctx context.Context) error {
	if c.startErr != nil {
		return c.startErr
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.started = true
	return nil
}

func (c *fakeVolgaV6PhysicalCarrier) SendVolgaV6(frame volgaV6WireFrame) error {
	if !c.started || c.stopped {
		return errors.New("fake carrier not active")
	}
	copyFrame := frame
	copyFrame.Payload = cloneVolgaV6Payload(frame.Payload)
	c.sent = append(c.sent, copyFrame)
	if c.autoEmit && c.onFrame != nil {
		c.onFrame(copyFrame)
	}
	return nil
}

func (c *fakeVolgaV6PhysicalCarrier) Stop() error {
	c.stopped = true
	return nil
}

func (c *fakeVolgaV6PhysicalCarrier) emit(frame volgaV6WireFrame) {
	if c.onFrame != nil {
		c.onFrame(frame)
	}
}

type fakeVolgaV6CarrierFactory struct {
	carriers map[uint64]*fakeVolgaV6PhysicalCarrier
	startErr map[uint64]error
	autoEmit map[uint64]bool
}

func newFakeVolgaV6CarrierFactory() *fakeVolgaV6CarrierFactory {
	return &fakeVolgaV6CarrierFactory{
		carriers: make(map[uint64]*fakeVolgaV6PhysicalCarrier),
		startErr: make(map[uint64]error),
		autoEmit: make(map[uint64]bool),
	}
}

func (f *fakeVolgaV6CarrierFactory) create(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
	carrier := &fakeVolgaV6PhysicalCarrier{
		generation: generation,
		onFrame:    onFrame,
		startErr:   f.startErr[generation],
		autoEmit:   f.autoEmit[generation],
	}
	f.carriers[generation] = carrier
	return carrier, nil
}

func TestVolgaV6CarrierManagerMakeBeforeBreakAndReplayOnNewGeneration(t *testing.T) {
	factory := newFakeVolgaV6CarrierFactory()
	factory.autoEmit[2] = true

	var delivered []string
	receiver := newVolgaV6ReliableReceiver(func(payload [][]byte) {
		delivered = append(delivered, string(payload[0]))
	})
	manager := newVolgaV6CarrierManager(factory.create, func(frame volgaV6WireFrame) {
		if frame.Kind == volgaV6FrameData {
			receiver.Accept(frame)
		}
	})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	session := newVolgaV6ReliableSession(1111, manager, defaultVolgaV6ReliableConfig())
	seq, err := session.sendAt([][]byte{[]byte("one")}, testTime(0))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 || len(factory.carriers[1].sent) != 1 {
		t.Fatalf("initial send seq=%d gen1=%d", seq, len(factory.carriers[1].sent))
	}

	oldGen, newGen, err := manager.Handoff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oldGen != 1 || newGen != 2 {
		t.Fatalf("handoff old=%d new=%d", oldGen, newGen)
	}
	snap := manager.Snapshot()
	if snap.ActiveGeneration != 2 || !reflect.DeepEqual(snap.Draining, []uint64{1}) || snap.Handoffs != 1 {
		t.Fatalf("manager snapshot=%+v", snap)
	}
	if factory.carriers[1].stopped {
		t.Fatal("old carrier was broken before drain")
	}

	if err := session.replayAt(1, testTime(1)); err != nil {
		t.Fatal(err)
	}
	if len(factory.carriers[2].sent) != 1 {
		t.Fatalf("replacement sends=%d want 1", len(factory.carriers[2].sent))
	}
	first := factory.carriers[1].sent[0]
	second := factory.carriers[2].sent[0]
	if first.Session != second.Session || first.Seq != second.Seq {
		t.Fatalf("logical identity changed across physical generation: first=%+v second=%+v", first, second)
	}
	if !reflect.DeepEqual(delivered, []string{"one"}) {
		t.Fatalf("delivery=%v", delivered)
	}
}

func TestVolgaV6CarrierManagerFailedReplacementKeepsOldActive(t *testing.T) {
	factory := newFakeVolgaV6CarrierFactory()
	manager := newVolgaV6CarrierManager(factory.create, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory.startErr[2] = errors.New("fresh auth failed")

	if _, _, err := manager.Handoff(context.Background()); err == nil {
		t.Fatal("handoff unexpectedly succeeded")
	}
	snap := manager.Snapshot()
	if snap.ActiveGeneration != 1 || len(snap.Draining) != 0 || snap.Handoffs != 0 {
		t.Fatalf("failed handoff changed active state: %+v", snap)
	}
	if !factory.carriers[2].stopped {
		t.Fatal("failed replacement was not stopped")
	}
}

func TestVolgaV6CarrierManagerDrainingCarrierMayDeliverLateDuplicate(t *testing.T) {
	factory := newFakeVolgaV6CarrierFactory()
	factory.autoEmit[2] = true

	deliveries := 0
	receiver := newVolgaV6ReliableReceiver(func(payload [][]byte) { deliveries++ })
	manager := newVolgaV6CarrierManager(factory.create, func(frame volgaV6WireFrame) {
		if frame.Kind == volgaV6FrameData {
			receiver.Accept(frame)
		}
	})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	session := newVolgaV6ReliableSession(2222, manager, defaultVolgaV6ReliableConfig())
	seq, err := session.sendAt([][]byte{[]byte("late")}, testTime(0))
	if err != nil {
		t.Fatal(err)
	}
	original := factory.carriers[1].sent[0]

	if _, _, err := manager.Handoff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.replayAt(seq, testTime(1)); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("new generation delivery count=%d", deliveries)
	}

	factory.carriers[1].emit(original)
	if deliveries != 1 {
		t.Fatalf("late draining duplicate delivered twice: %d", deliveries)
	}
	if snap := receiver.Snapshot(); snap.Duplicates != 1 {
		t.Fatalf("receiver duplicate snapshot=%+v", snap)
	}
}

func TestVolgaV6CarrierManagerRetireOnlyDrainingGeneration(t *testing.T) {
	factory := newFakeVolgaV6CarrierFactory()
	manager := newVolgaV6CarrierManager(factory.create, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Handoff(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.Retire(2); !errors.Is(err, errVolgaV6ActiveRetire) {
		t.Fatalf("active retire error=%v", err)
	}
	if err := manager.Retire(1); err != nil {
		t.Fatal(err)
	}
	if !factory.carriers[1].stopped {
		t.Fatal("draining carrier was not stopped")
	}
	if snap := manager.Snapshot(); snap.ActiveGeneration != 2 || len(snap.Draining) != 0 {
		t.Fatalf("snapshot after retire=%+v", snap)
	}
}

func TestVolgaV6CarrierManagerStopClosesActiveAndDraining(t *testing.T) {
	factory := newFakeVolgaV6CarrierFactory()
	manager := newVolgaV6CarrierManager(factory.create, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Handoff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	if !factory.carriers[1].stopped || !factory.carriers[2].stopped {
		t.Fatalf("stop states gen1=%t gen2=%t", factory.carriers[1].stopped, factory.carriers[2].stopped)
	}
	if err := manager.SendVolgaV6(volgaV6WireFrame{Kind: volgaV6FrameAck}); !errors.Is(err, errVolgaV6NoActiveCarrier) {
		t.Fatalf("send after stop error=%v", err)
	}
}

func testTime(seconds int64) time.Time {
	return time.Unix(seconds, 0)
}
