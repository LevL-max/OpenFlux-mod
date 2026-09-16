package yandex

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

type volgaV6PhysicalFrame struct {
	Generation uint64
	Frame      volgaV6WireFrame
}

type fakeVolgaV6CarrierManager struct {
	generation uint64
	frames     []volgaV6PhysicalFrame
	receiver   *volgaV6ReliableReceiver
	dropSeq    map[uint64]int
	holdSeq    map[uint64][]volgaV6WireFrame
}

func newFakeVolgaV6CarrierManager(receiver *volgaV6ReliableReceiver) *fakeVolgaV6CarrierManager {
	return &fakeVolgaV6CarrierManager{
		generation: 1,
		receiver:   receiver,
		dropSeq:    make(map[uint64]int),
		holdSeq:    make(map[uint64][]volgaV6WireFrame),
	}
}

func (f *fakeVolgaV6CarrierManager) SendVolgaV6(frame volgaV6WireFrame) error {
	copyFrame := frame
	copyFrame.Payload = cloneVolgaV6Payload(frame.Payload)
	f.frames = append(f.frames, volgaV6PhysicalFrame{Generation: f.generation, Frame: copyFrame})

	if frame.Kind != volgaV6FrameData || f.receiver == nil {
		return nil
	}
	if n := f.dropSeq[frame.Seq]; n > 0 {
		f.dropSeq[frame.Seq] = n - 1
		return nil
	}
	if _, held := f.holdSeq[frame.Seq]; held {
		f.holdSeq[frame.Seq] = append(f.holdSeq[frame.Seq], copyFrame)
		return nil
	}
	f.receiver.Accept(copyFrame)
	return nil
}

func (f *fakeVolgaV6CarrierManager) switchGeneration() {
	f.generation++
}

func (f *fakeVolgaV6CarrierManager) hold(seq uint64) {
	f.holdSeq[seq] = nil
}

func (f *fakeVolgaV6CarrierManager) release(seq uint64) {
	frames := append([]volgaV6WireFrame(nil), f.holdSeq[seq]...)
	delete(f.holdSeq, seq)
	for _, frame := range frames {
		if f.receiver != nil {
			f.receiver.Accept(frame)
		}
	}
}

func TestVolgaV6ReplayCrossesCarrierGenerationWithoutLogicalReset(t *testing.T) {
	var delivered []string
	receiver := newVolgaV6ReliableReceiver(func(payload [][]byte) {
		delivered = append(delivered, string(payload[0]))
	})
	wire := newFakeVolgaV6CarrierManager(receiver)
	session := newVolgaV6ReliableSession(1001, wire, defaultVolgaV6ReliableConfig())

	wire.dropSeq[1] = 1
	seq, err := session.sendAt([][]byte{[]byte("payload")}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 || len(delivered) != 0 {
		t.Fatalf("initial seq=%d delivered=%v", seq, delivered)
	}

	wire.switchGeneration()
	if err := session.replayAt(seq, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(delivered, []string{"payload"}) {
		t.Fatalf("delivery=%v", delivered)
	}
	if len(wire.frames) != 2 {
		t.Fatalf("physical frames=%d want 2", len(wire.frames))
	}
	first := wire.frames[0]
	second := wire.frames[1]
	if first.Generation != 1 || second.Generation != 2 {
		t.Fatalf("generations=%d,%d", first.Generation, second.Generation)
	}
	if first.Frame.Session != second.Frame.Session || first.Frame.Seq != second.Frame.Seq {
		t.Fatalf("logical identity changed across handoff: first=%+v second=%+v", first.Frame, second.Frame)
	}
}

func TestVolgaV6CarrierSwitchDoesNotDeleteReplay(t *testing.T) {
	wire := newFakeVolgaV6CarrierManager(nil)
	session := newVolgaV6ReliableSession(2002, wire, defaultVolgaV6ReliableConfig())
	_, err := session.sendAt([][]byte{[]byte("one")}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	wire.switchGeneration()

	snap := session.Snapshot(time.Unix(1, 0))
	if snap.Session != 2002 || snap.ReplayDepth != 1 || snap.NextSeq != 1 {
		t.Fatalf("snapshot after physical switch=%+v", snap)
	}
}

func TestVolgaV6LateOldGenerationDeliveryIsDeduplicated(t *testing.T) {
	deliveries := 0
	receiver := newVolgaV6ReliableReceiver(func(payload [][]byte) { deliveries++ })
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 3003,
		Seq:     1,
		Floor:   1,
		Payload: [][]byte{[]byte("same")},
	}

	if _, delivered := receiver.Accept(frame); !delivered {
		t.Fatal("first generation delivery was not accepted")
	}
	if _, delivered := receiver.Accept(frame); delivered {
		t.Fatal("late duplicate from draining generation was delivered twice")
	}

	snap := receiver.Snapshot()
	if deliveries != 1 || snap.Duplicates != 1 || snap.Base != 1 {
		t.Fatalf("deliveries=%d snapshot=%+v", deliveries, snap)
	}
}

func TestVolgaV6AckCanBeRepeatedAfterControlLoss(t *testing.T) {
	receiver := newVolgaV6ReliableReceiver(nil)
	wire := newFakeVolgaV6CarrierManager(receiver)
	session := newVolgaV6ReliableSession(4004, wire, defaultVolgaV6ReliableConfig())

	if _, err := session.sendAt([][]byte{[]byte("one")}, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}

	// Simulate the first ACK control operation being accepted by Yandex but not
	// observed by the sender: obtain it, then deliberately do not apply it.
	first, ok := receiver.AckSnapshot()
	if !ok {
		t.Fatal("missing ACK snapshot")
	}
	if snap := session.Snapshot(time.Unix(1, 0)); snap.ReplayDepth != 1 {
		t.Fatalf("replay unexpectedly cleared after lost ACK: %+v", snap)
	}

	// The receiver can send the same state again without receiving duplicate
	// DATA. Applying the repeated ACK clears the sender replay.
	second, ok := receiver.AckSnapshot()
	if !ok || !reflect.DeepEqual(first, second) {
		t.Fatalf("ACK is not repeatable: first=%+v second=%+v", first, second)
	}
	if released := session.HandleAck(second); released != 1 {
		t.Fatalf("released=%d want 1", released)
	}
	if snap := session.Snapshot(time.Unix(1, 0)); snap.ReplayDepth != 0 || snap.AckBase != 1 {
		t.Fatalf("post-ACK snapshot=%+v", snap)
	}
}

func TestVolgaV6ReorderAdvancesBaseWhenHoleArrives(t *testing.T) {
	var delivered []string
	receiver := newVolgaV6ReliableReceiver(func(payload [][]byte) {
		delivered = append(delivered, string(payload[0]))
	})
	wire := newFakeVolgaV6CarrierManager(receiver)
	session := newVolgaV6ReliableSession(5005, wire, defaultVolgaV6ReliableConfig())

	if _, err := session.sendAt([][]byte{[]byte("one")}, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	wire.hold(2)
	if _, err := session.sendAt([][]byte{[]byte("two")}, time.Unix(0, int64(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if _, err := session.sendAt([][]byte{[]byte("three")}, time.Unix(0, int64(2*time.Millisecond))); err != nil {
		t.Fatal(err)
	}

	mid := receiver.Snapshot()
	if mid.Base != 1 || mid.Pending != 1 {
		t.Fatalf("mid reorder state=%+v want base=1 pending=1", mid)
	}
	wire.release(2)
	end := receiver.Snapshot()
	if end.Base != 3 || end.Pending != 0 {
		t.Fatalf("final reorder state=%+v want base=3 pending=0", end)
	}
	if !reflect.DeepEqual(delivered, []string{"one", "three", "two"}) {
		t.Fatalf("immediate delivery order=%v", delivered)
	}
}

func TestVolgaV6ReceiverResumeUsesReplayFloor(t *testing.T) {
	wire := newFakeVolgaV6CarrierManager(nil)
	session := newVolgaV6ReliableSession(6006, wire, defaultVolgaV6ReliableConfig())

	if _, err := session.sendAt([][]byte{[]byte("one")}, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.sendAt([][]byte{[]byte("two")}, time.Unix(0, int64(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if _, err := session.sendAt([][]byte{[]byte("three")}, time.Unix(0, int64(2*time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	session.HandleAck(volgaV6Ack{Session: 6006, Base: 1})

	freshReceiver := newVolgaV6ReliableReceiver(nil)
	wire.receiver = freshReceiver
	wire.frames = nil

	// Replay seq 3 first. The replay floor must still be 2, so the restarted
	// receiver can establish base=1 instead of waiting forever for seq 1.
	if err := session.replayAt(3, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	mid := freshReceiver.Snapshot()
	if mid.Base != 1 || mid.Pending != 1 || mid.PeerSession != 6006 {
		t.Fatalf("resume midpoint=%+v", mid)
	}
	if len(wire.frames) != 1 || wire.frames[0].Frame.Floor != 2 {
		t.Fatalf("replay floor frame=%+v", wire.frames)
	}

	if err := session.replayAt(2, time.Unix(1, int64(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	end := freshReceiver.Snapshot()
	if end.Base != 3 || end.Pending != 0 {
		t.Fatalf("resume final=%+v", end)
	}
}

func TestVolgaV6RepairSelectionIsOldestFirstAndBurstBounded(t *testing.T) {
	wire := newFakeVolgaV6CarrierManager(nil)
	cfg := defaultVolgaV6ReliableConfig()
	cfg.RetryBurst = 3
	cfg.BaseRTO = 100 * time.Millisecond
	session := newVolgaV6ReliableSession(7007, wire, cfg)

	start := time.Unix(10, 0)
	for i := 0; i < 6; i++ {
		if _, err := session.sendAt([][]byte{[]byte{byte(i)}}, start); err != nil {
			t.Fatal(err)
		}
	}

	got := session.DueRepairs(start.Add(100 * time.Millisecond))
	want := []uint64{1, 2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repair candidates=%v want=%v", got, want)
	}
}

func TestVolgaV6ReplayBackoffPreventsImmediateRetryLoop(t *testing.T) {
	wire := newFakeVolgaV6CarrierManager(nil)
	cfg := defaultVolgaV6ReliableConfig()
	cfg.BaseRTO = 100 * time.Millisecond
	session := newVolgaV6ReliableSession(8008, wire, cfg)
	start := time.Unix(20, 0)

	if _, err := session.sendAt([][]byte{[]byte("one")}, start); err != nil {
		t.Fatal(err)
	}
	if err := session.replayAt(1, start.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got := session.DueRepairs(start.Add(299 * time.Millisecond)); len(got) != 0 {
		t.Fatalf("retry became due too early after attempt 1: %v", got)
	}
	if got := session.DueRepairs(start.Add(300 * time.Millisecond)); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("retry after exponential delay=%v want [1]", got)
	}
}

func TestVolgaV6InFlightWindowBackpressuresNewDataUntilAck(t *testing.T) {
	wire := newFakeVolgaV6CarrierManager(nil)
	cfg := defaultVolgaV6ReliableConfig()
	cfg.MaxInFlight = 2
	session := newVolgaV6ReliableSession(9009, wire, cfg)

	if _, err := session.sendAt([][]byte{[]byte("one")}, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.sendAt([][]byte{[]byte("two")}, time.Unix(0, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.sendAt([][]byte{[]byte("blocked")}, time.Unix(0, 2)); !errors.Is(err, errVolgaV6WindowFull) {
		t.Fatalf("third send error=%v want window full", err)
	}

	if released := session.HandleAck(volgaV6Ack{Session: 9009, Base: 1}); released != 1 {
		t.Fatalf("released=%d want 1", released)
	}
	if seq, err := session.sendAt([][]byte{[]byte("three")}, time.Unix(0, 3)); err != nil || seq != 3 {
		t.Fatalf("send after ACK seq=%d err=%v", seq, err)
	}
}

func TestVolgaV6RepeatedCarrierChangesKeepOneLogicalSession(t *testing.T) {
	wire := newFakeVolgaV6CarrierManager(nil)
	session := newVolgaV6ReliableSession(10101, wire, defaultVolgaV6ReliableConfig())

	seq, err := session.sendAt([][]byte{[]byte("x")}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		wire.switchGeneration()
		if err := session.replayAt(seq, time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}

	if len(wire.frames) != 5 {
		t.Fatalf("frames=%d want 5", len(wire.frames))
	}
	for i, frame := range wire.frames {
		if frame.Generation != uint64(i+1) {
			t.Fatalf("frame %d generation=%d", i, frame.Generation)
		}
		if frame.Frame.Session != 10101 || frame.Frame.Seq != 1 {
			t.Fatalf("frame %d logical identity=%+v", i, frame.Frame)
		}
	}
}
