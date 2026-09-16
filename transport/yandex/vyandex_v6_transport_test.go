package yandex

import (
	"context"
	"errors"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func newLinkedVolgaV6OuterPair(t *testing.T, cfg VolgaV6TransportConfig) (*YandexVolgaV6Transport, *YandexVolgaV6Transport, *linkedVolgaV6Factory, *linkedVolgaV6Factory, chan string, chan string) {
	t.Helper()
	factoryA := newLinkedVolgaV6Factory()
	factoryB := newLinkedVolgaV6Factory()
	baseCfg := transport.DefaultConfig()
	a := newYandexVolgaV6TransportWithFactory(baseCfg, cfg, factoryA.create)
	b := newYandexVolgaV6TransportWithFactory(baseCfg, cfg, factoryB.create)
	factoryA.peer = b.runtime
	factoryB.peer = a.runtime

	recvA := make(chan string, 32)
	recvB := make(chan string, 32)
	a.Receive(func(data []byte) { recvA <- string(data) })
	b.Receive(func(data []byte) { recvB <- string(data) })

	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		_ = a.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.Stop()
		_ = b.Stop()
	})
	return a, b, factoryA, factoryB, recvA, recvB
}

func waitVolgaV6String(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for V6 delivery", timeout)
		return ""
	}
}

func TestVolgaV6OuterTransportBatchesAndClearsReplay(t *testing.T) {
	cfg := DefaultVolgaV6TransportConfig([]string{"offline-a", "offline-b"})
	cfg.BatchPackets = 3
	cfg.BatchBytes = 5000
	cfg.BatchTimeout = 50 * time.Millisecond
	cfg.TickInterval = 10 * time.Millisecond
	cfg.QueueSize = 32

	a, _, factoryA, _, _, recvB := newLinkedVolgaV6OuterPair(t, cfg)

	for _, payload := range []string{"one", "two", "three"} {
		if err := a.Send([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"one", "two", "three"} {
		if got := waitVolgaV6String(t, recvB, time.Second); got != want {
			t.Fatalf("delivery=%q want=%q", got, want)
		}
	}

	deadline := time.Now().Add(time.Second)
	for {
		snap := a.Snapshot(time.Now())
		if snap.Reliable.ReplayDepth == 0 && snap.Reliable.AckBase >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sender replay did not clear: %+v", snap.Reliable)
		}
		time.Sleep(5 * time.Millisecond)
	}

	sent := factoryA.sentSnapshot()
	if len(sent) != 1 || sent[0].frame.Kind != volgaV6FrameData || len(sent[0].frame.Payload) != 3 {
		t.Fatalf("physical logical batches=%+v", sent)
	}
	stats := a.Stats()
	if stats.PacketsSent != 3 || stats.BytesSent != uint64(len("one")+len("two")+len("three")) {
		t.Fatalf("transport stats=%+v", stats)
	}
}

func TestVolgaV6OuterTransportProgressStallRecyclesBeforeRepair(t *testing.T) {
	cfg := DefaultVolgaV6TransportConfig([]string{"offline-a", "offline-b"})
	cfg.BatchPackets = 1
	cfg.BatchTimeout = time.Millisecond
	cfg.TickInterval = 10 * time.Millisecond
	cfg.QueueSize = 32
	cfg.Runtime.Reliable.BaseRTO = 200 * time.Millisecond
	cfg.Runtime.Recovery.ProgressStall = 50 * time.Millisecond
	cfg.Runtime.Recovery.RecycleCooldown = 500 * time.Millisecond
	cfg.Runtime.Recovery.RetryRatePerSecond = 20
	cfg.Runtime.Recovery.RetryBurst = 4
	cfg.Runtime.DrainGrace = 100 * time.Millisecond

	a, _, factoryA, _, _, recvB := newLinkedVolgaV6OuterPair(t, cfg)
	factoryA.setDropData(1)

	before := a.Snapshot(time.Now())
	logicalSession := before.Reliable.Session
	if err := a.Send([]byte("survive-handoff")); err != nil {
		t.Fatal(err)
	}
	if got := waitVolgaV6String(t, recvB, 2*time.Second); got != "survive-handoff" {
		t.Fatalf("delivery=%q", got)
	}

	deadline := time.Now().Add(time.Second)
	for {
		snap := a.Snapshot(time.Now())
		if snap.Carrier.ActiveGeneration >= 2 && snap.Reliable.ReplayDepth == 0 {
			if snap.Reliable.Session != logicalSession {
				t.Fatalf("logical session changed across recycle: before=%d after=%d", logicalSession, snap.Reliable.Session)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("handoff/recovery incomplete: %+v", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}

	sent := factoryA.sentSnapshot()
	if len(sent) < 2 {
		t.Fatalf("physical sends=%+v", sent)
	}
	if sent[0].generation != 1 {
		t.Fatalf("initial generation=%d want 1", sent[0].generation)
	}
	foundReplayOnNewGeneration := false
	for _, item := range sent[1:] {
		if item.generation >= 2 && item.frame.Kind == volgaV6FrameData && item.frame.Seq == sent[0].frame.Seq && item.frame.Session == sent[0].frame.Session {
			foundReplayOnNewGeneration = true
			break
		}
	}
	if !foundReplayOnNewGeneration {
		t.Fatalf("same logical DATA was not replayed on a fresh generation: %+v", sent)
	}
}

func TestVolgaV6OuterTransportRejectsSendBeforeStartAndAfterStop(t *testing.T) {
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	factory := newLinkedVolgaV6Factory()
	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factory.create)
	if err := v6.Send([]byte("before")); !errors.Is(err, errVolgaV6TransportNotStarted) {
		t.Fatalf("send before Start error=%v", err)
	}
	if err := v6.Start(); err != nil {
		t.Fatal(err)
	}
	if err := v6.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := v6.Send([]byte("after")); !errors.Is(err, errVolgaV6TransportNotStarted) {
		t.Fatalf("send after Stop error=%v", err)
	}
}

func TestVolgaV6OuterTransportStartHonorsFactoryFailure(t *testing.T) {
	factory := func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return nil, context.DeadlineExceeded
	}
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factory)
	if err := v6.Start(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start error=%v want deadline exceeded", err)
	}
	if v6.IsConnected() {
		t.Fatal("transport reports connected after failed initial carrier")
	}
}
