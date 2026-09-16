package yandex

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type linkedVolgaV6Carrier struct {
	generation uint64
	factory    *linkedVolgaV6Factory
	started    bool
	stopped    bool
}

func (c *linkedVolgaV6Carrier) Generation() uint64 { return c.generation }

func (c *linkedVolgaV6Carrier) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.started = true
	return nil
}

func (c *linkedVolgaV6Carrier) SendVolgaV6(frame volgaV6WireFrame) error {
	if !c.started || c.stopped {
		return errors.New("linked carrier not active")
	}
	return c.factory.send(c.generation, frame)
}

func (c *linkedVolgaV6Carrier) Stop() error {
	c.stopped = true
	return nil
}

type linkedVolgaV6Sent struct {
	generation uint64
	frame      volgaV6WireFrame
}

type linkedVolgaV6Factory struct {
	mu sync.Mutex

	peer *volgaV6Runtime

	carriers map[uint64]*linkedVolgaV6Carrier
	sent     []linkedVolgaV6Sent

	dropData int
	dropAck  int
}

func newLinkedVolgaV6Factory() *linkedVolgaV6Factory {
	return &linkedVolgaV6Factory{carriers: make(map[uint64]*linkedVolgaV6Carrier)}
}

func (f *linkedVolgaV6Factory) create(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
	carrier := &linkedVolgaV6Carrier{generation: generation, factory: f}
	f.mu.Lock()
	f.carriers[generation] = carrier
	f.mu.Unlock()
	return carrier, nil
}

func (f *linkedVolgaV6Factory) send(generation uint64, frame volgaV6WireFrame) error {
	copyFrame := frame
	copyFrame.Payload = cloneVolgaV6Payload(frame.Payload)

	f.mu.Lock()
	f.sent = append(f.sent, linkedVolgaV6Sent{generation: generation, frame: copyFrame})
	drop := false
	switch frame.Kind {
	case volgaV6FrameData:
		if f.dropData > 0 {
			f.dropData--
			drop = true
		}
	case volgaV6FrameAck:
		if f.dropAck > 0 {
			f.dropAck--
			drop = true
		}
	}
	peer := f.peer
	f.mu.Unlock()

	// Simulate Volga's dangerous semantic: apparent send success does not imply
	// peer delivery. A dropped frame still returns nil to the sender.
	if drop || peer == nil {
		return nil
	}
	peer.handleIncoming(copyFrame)
	return nil
}

func (f *linkedVolgaV6Factory) setDropData(n int) {
	f.mu.Lock()
	f.dropData = n
	f.mu.Unlock()
}

func (f *linkedVolgaV6Factory) setDropAck(n int) {
	f.mu.Lock()
	f.dropAck = n
	f.mu.Unlock()
}

func (f *linkedVolgaV6Factory) sentSnapshot() []linkedVolgaV6Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]linkedVolgaV6Sent, len(f.sent))
	copy(out, f.sent)
	return out
}

func newLinkedVolgaV6Pair(t *testing.T, cfg volgaV6RuntimeConfig) (*volgaV6Runtime, *volgaV6Runtime, *linkedVolgaV6Factory, *linkedVolgaV6Factory, *[]string, *[]string) {
	t.Helper()
	factoryA := newLinkedVolgaV6Factory()
	factoryB := newLinkedVolgaV6Factory()
	var gotA []string
	var gotB []string
	runtimeA := newVolgaV6Runtime(10001, factoryA.create, cfg, func(payload [][]byte) {
		gotA = append(gotA, string(payload[0]))
	})
	runtimeB := newVolgaV6Runtime(20002, factoryB.create, cfg, func(payload [][]byte) {
		gotB = append(gotB, string(payload[0]))
	})
	factoryA.peer = runtimeB
	factoryB.peer = runtimeA
	start := time.Unix(1000, 0)
	if err := runtimeA.startAt(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := runtimeB.startAt(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	return runtimeA, runtimeB, factoryA, factoryB, &gotA, &gotB
}

func TestVolgaV6RuntimeNormalDataAndAckClearsReplay(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	a, b, _, _, _, gotB := newLinkedVolgaV6Pair(t, cfg)
	now := time.Unix(1001, 0)

	seq, err := a.sendAt([][]byte{[]byte("hello")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 || len(*gotB) != 1 || (*gotB)[0] != "hello" {
		t.Fatalf("seq=%d peer delivery=%v", seq, *gotB)
	}
	if snap := a.Snapshot(now); snap.Reliable.ReplayDepth != 1 {
		t.Fatalf("replay cleared before coalesced ACK tick: %+v", snap.Reliable)
	}
	if result := b.Tick(context.Background(), now.Add(time.Millisecond)); result.AckErr != nil {
		t.Fatalf("ACK tick failed: %v", result.AckErr)
	}
	if snap := a.Snapshot(now.Add(time.Millisecond)); snap.Reliable.ReplayDepth != 0 || snap.Reliable.AckBase != 1 {
		t.Fatalf("sender replay after ACK=%+v", snap.Reliable)
	}
}

func TestVolgaV6RuntimeCoalescesManyDataIntoOneCumulativeAck(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	a, b, _, factoryB, _, gotB := newLinkedVolgaV6Pair(t, cfg)
	start := time.Unix(1050, 0)

	for i, value := range []string{"one", "two", "three"} {
		if _, err := a.sendAt([][]byte{[]byte(value)}, start.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*gotB) != 3 {
		t.Fatalf("peer deliveries=%v", *gotB)
	}
	before := factoryB.sentSnapshot()
	for _, item := range before {
		if item.frame.Kind == volgaV6FrameAck {
			t.Fatalf("ACK emitted synchronously in websocket path: %+v", before)
		}
	}

	if result := b.Tick(context.Background(), start.Add(10*time.Millisecond)); result.AckErr != nil {
		t.Fatal(result.AckErr)
	}
	after := factoryB.sentSnapshot()
	acks := 0
	for _, item := range after {
		if item.frame.Kind == volgaV6FrameAck {
			acks++
			if item.frame.Ack.Base != 3 {
				t.Fatalf("cumulative ACK base=%d want 3", item.frame.Ack.Base)
			}
		}
	}
	if acks != 1 {
		t.Fatalf("ACK frames=%d want 1, sent=%+v", acks, after)
	}
	if snap := a.Snapshot(start.Add(10 * time.Millisecond)); snap.Reliable.ReplayDepth != 0 || snap.Reliable.AckBase != 3 {
		t.Fatalf("sender replay after cumulative ACK=%+v", snap.Reliable)
	}
}

func TestVolgaV6RuntimeLostAckRecoveredByRepeatedAckState(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.AckRepeatInterval = 100 * time.Millisecond
	a, b, _, factoryB, _, gotB := newLinkedVolgaV6Pair(t, cfg)
	start := time.Unix(1100, 0)
	factoryB.setDropAck(1)

	if _, err := a.sendAt([][]byte{[]byte("ack-loss")}, start); err != nil {
		t.Fatal(err)
	}
	if len(*gotB) != 1 {
		t.Fatalf("peer DATA delivery=%v", *gotB)
	}

	// First ACK is accepted by the fake Volga carrier but deliberately not
	// delivered to the sender.
	if result := b.Tick(context.Background(), start.Add(10*time.Millisecond)); result.AckErr != nil {
		t.Fatalf("first ACK send failed locally: %v", result.AckErr)
	}
	if snap := a.Snapshot(start.Add(10 * time.Millisecond)); snap.Reliable.ReplayDepth != 1 {
		t.Fatalf("lost ACK unexpectedly cleared replay: %+v", snap.Reliable)
	}

	// No duplicate DATA is required. Periodic ACK state is repeated and clears
	// the sender replay once that control operation reaches the peer.
	result := b.Tick(context.Background(), start.Add(150*time.Millisecond))
	if result.AckErr != nil {
		t.Fatalf("repeat ACK failed: %v", result.AckErr)
	}
	if snap := a.Snapshot(start.Add(150 * time.Millisecond)); snap.Reliable.ReplayDepth != 0 || snap.Reliable.AckBase != 1 {
		t.Fatalf("repeat ACK did not clear replay: %+v", snap.Reliable)
	}
}

func TestVolgaV6RuntimeProgressStallHandoffsBeforeRepair(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Reliable.BaseRTO = 100 * time.Millisecond
	cfg.Recovery.ProgressStall = 500 * time.Millisecond
	cfg.Recovery.RecycleCooldown = 2 * time.Second
	cfg.Recovery.RetryBurst = 2
	cfg.Recovery.RetryRatePerSecond = 2
	cfg.DrainGrace = 500 * time.Millisecond

	a, b, factoryA, _, _, gotB := newLinkedVolgaV6Pair(t, cfg)
	start := time.Unix(1200, 0)
	factoryA.setDropData(1)

	seq, err := a.sendAt([][]byte{[]byte("repair-me")}, start)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 || len(*gotB) != 0 {
		t.Fatalf("initial dropped DATA seq=%d gotB=%v", seq, *gotB)
	}

	result := a.Tick(context.Background(), start.Add(600*time.Millisecond))
	if !result.Handoff || result.OldGeneration != 1 || result.NewGeneration != 2 {
		t.Fatalf("stall did not handoff: %+v", result)
	}
	if result.Repairs != 1 || len(*gotB) != 1 || (*gotB)[0] != "repair-me" {
		t.Fatalf("post-handoff repair result=%+v delivery=%v", result, *gotB)
	}
	// The repaired DATA has reached B, but B intentionally coalesces ACK state
	// until its Tick instead of blocking its receive path on an HTTP ACK POST.
	if ackResult := b.Tick(context.Background(), start.Add(601*time.Millisecond)); ackResult.AckErr != nil {
		t.Fatalf("peer ACK tick failed: %v", ackResult.AckErr)
	}
	if snap := a.Snapshot(start.Add(601 * time.Millisecond)); snap.Reliable.ReplayDepth != 0 || snap.Carrier.ActiveGeneration != 2 {
		t.Fatalf("post-repair snapshot=%+v", snap)
	}

	sent := factoryA.sentSnapshot()
	if len(sent) < 2 {
		t.Fatalf("physical sends=%v", sent)
	}
	if sent[0].generation != 1 || sent[1].generation != 2 {
		t.Fatalf("repair did not move generations: first=%d second=%d", sent[0].generation, sent[1].generation)
	}
	if sent[0].frame.Session != sent[1].frame.Session || sent[0].frame.Seq != sent[1].frame.Seq {
		t.Fatalf("logical DATA changed across recycle: first=%+v second=%+v", sent[0].frame, sent[1].frame)
	}

	retire := a.Tick(context.Background(), start.Add(1200*time.Millisecond))
	if len(retire.Retired) != 1 || retire.Retired[0] != 1 {
		t.Fatalf("draining generation retirement=%+v", retire.Retired)
	}
	if !factoryA.carriers[1].stopped {
		t.Fatal("old generation was not stopped after drain grace")
	}
}

func TestVolgaV6RuntimeDynamicAdmissionBackpressuresBeforeStaticCap(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Reliable.MaxInFlight = 8
	cfg.Recovery.MaxWindow = 8
	cfg.Recovery.HighWatermark = 4
	cfg.Recovery.CriticalWatermark = 6
	cfg.Recovery.MinWindow = 2

	a, _, factoryA, _, _, _ := newLinkedVolgaV6Pair(t, cfg)
	factoryA.setDropData(10)
	start := time.Unix(1300, 0)

	for i := 0; i < 4; i++ {
		if _, err := a.sendAt([][]byte{[]byte{byte(i)}}, start.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatalf("send %d failed early: %v", i+1, err)
		}
	}
	if _, err := a.sendAt([][]byte{[]byte("blocked")}, start.Add(5*time.Millisecond)); !errors.Is(err, errVolgaV6RecoveryBackpressure) {
		t.Fatalf("fifth send error=%v want recovery backpressure", err)
	}
	if snap := a.Snapshot(start.Add(5 * time.Millisecond)); snap.Reliable.ReplayDepth != 4 {
		t.Fatalf("replay depth=%d want 4", snap.Reliable.ReplayDepth)
	}
}

func TestVolgaV6RuntimeRetryBudgetBoundsRepairPosts(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Reliable.BaseRTO = 100 * time.Millisecond
	cfg.Reliable.RetryBurst = 8
	cfg.Recovery.ProgressStall = 10 * time.Second
	cfg.Recovery.RetryBurst = 2
	cfg.Recovery.RetryRatePerSecond = 1
	cfg.Recovery.MaxWindow = 16
	cfg.Recovery.HighWatermark = 12
	cfg.Recovery.CriticalWatermark = 14
	cfg.Recovery.MinWindow = 4

	a, _, factoryA, _, _, _ := newLinkedVolgaV6Pair(t, cfg)
	factoryA.setDropData(20)
	start := time.Unix(1400, 0)
	for i := 0; i < 4; i++ {
		if _, err := a.sendAt([][]byte{[]byte{byte(i)}}, start); err != nil {
			t.Fatal(err)
		}
	}

	first := a.Tick(context.Background(), start.Add(100*time.Millisecond))
	if first.Repairs != 2 {
		t.Fatalf("first repair burst=%d want 2", first.Repairs)
	}
	second := a.Tick(context.Background(), start.Add(200*time.Millisecond))
	if second.Repairs != 0 {
		t.Fatalf("retry budget emitted early repairs=%d", second.Repairs)
	}
	third := a.Tick(context.Background(), start.Add(1100*time.Millisecond))
	if third.Repairs != 1 {
		t.Fatalf("retry refill repairs=%d want 1", third.Repairs)
	}
}
