//go:build volga

package yandex

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestVolgaV6RecoveryBacksOffRecyclesWithoutProgress(t *testing.T) {
	c := newVolgaV6RecoveryController(volgaV6RecoveryConfig{ProgressStall: time.Second, RecycleCooldown: 5 * time.Second, MaxRecycleCooldown: 30 * time.Second})
	start := time.Unix(2000, 0)
	c.Observe(start, volgaV6ReliableSnapshot{})
	stalled := func(at time.Time, base uint64) volgaV6ReliableSnapshot {
		return volgaV6ReliableSnapshot{AckBase: base, ReplayDepth: 1, OldestUnackedAge: at.Sub(start)}
	}
	var got []int
	for s := 0; s <= 120; s++ {
		at := start.Add(time.Duration(s) * time.Second)
		if c.Observe(at, stalled(at, 0)).Recycle {
			got = append(got, s)
		}
	}
	// A peer that never ACKs: base cooldown twice, then doubling up to the cap.
	if want := []int{1, 6, 16, 36, 66, 96}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recycles at %v s, want %v", got, want)
	}

	// ACK progress restores the base cooldown.
	c.Observe(start.Add(121*time.Second), stalled(start.Add(121*time.Second), 1))
	got = got[:0]
	for s := 122; s <= 140; s++ {
		at := start.Add(time.Duration(s) * time.Second)
		if c.Observe(at, stalled(at, 1)).Recycle {
			got = append(got, s)
		}
	}
	if want := []int{122, 127, 137}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after progress recycles at %v s, want %v", got, want)
	}
}

func TestVolgaV6RecoveryPeerActivityRestoresRepairRate(t *testing.T) {
	c := newVolgaV6RecoveryController(volgaV6RecoveryConfig{RetryRatePerSecond: 16, RetryBurst: 1})
	start := time.Unix(3000, 0)
	c.unproductive = 5 // four doublings: 1 repair/s
	if got := c.TakeRetryBudget(start, 1); got != 1 {
		t.Fatalf("initial burst=%d", got)
	}
	if got := c.TakeRetryBudget(start.Add(500*time.Millisecond), 1); got != 0 {
		t.Fatalf("backed-off budget after 500 ms=%d, want 0", got)
	}
	c.PeerActive()
	if got := c.TakeRetryBudget(start.Add(time.Second), 1); got != 1 {
		t.Fatalf("budget after peer activity=%d, want 1", got)
	}
}

func TestVolgaV6RuntimePeerFrameEndsRecoveryBackoff(t *testing.T) {
	a, b, _, _, _, _ := newLinkedVolgaV6Pair(t, defaultVolgaV6RuntimeConfig())
	a.recovery.unproductive = 4
	if _, err := b.sendAt([][]byte{[]byte("peer")}, time.Unix(4000, 0)); err != nil {
		t.Fatal(err)
	}
	a.Tick(context.Background(), time.Unix(4000, 0))
	if a.recovery.unproductive != 0 {
		t.Fatalf("peer DATA left recovery backoff at %d", a.recovery.unproductive)
	}
}

func TestVolgaV6RuntimeStopsRepeatingAckAfterPeerSilence(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.AckRepeatInterval = time.Millisecond
	cfg.AckRepeatWindow = 50 * time.Millisecond
	a, b, _, factoryB, _, _ := newLinkedVolgaV6Pair(t, cfg)
	acks := func() int {
		n := 0
		for _, s := range factoryB.sentSnapshot() {
			if s.frame.Kind == volgaV6FrameAck {
				n++
			}
		}
		return n
	}
	if _, err := a.Send([][]byte{[]byte("one")}); err != nil {
		t.Fatal(err)
	}
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 1 {
		t.Fatalf("new DATA not ACKed: err=%v acks=%d", err, acks())
	}
	time.Sleep(5 * time.Millisecond)
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 2 {
		t.Fatalf("recent peer not re-ACKed: err=%v acks=%d", err, acks())
	}
	time.Sleep(80 * time.Millisecond)
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 2 {
		t.Fatalf("silent peer still re-ACKed: err=%v acks=%d", err, acks())
	}
	if _, err := a.Send([][]byte{[]byte("two")}); err != nil {
		t.Fatal(err)
	}
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 3 {
		t.Fatalf("returning peer not ACKed: err=%v acks=%d", err, acks())
	}
}
