package yandex

import (
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestVolgaV6OuterTransportCoalescesBulkTrafficBeforePhysicalCarrier(t *testing.T) {
	cfg := DefaultVolgaV6TransportConfig([]string{"offline-a", "offline-b"})
	cfg.BatchPackets = 20
	cfg.BatchBytes = 5000
	cfg.BatchTimeout = 2 * time.Millisecond
	cfg.QueueSize = 20000
	cfg.SendQueueSize = 2048
	cfg.SendWorkers = 32
	cfg.TickInterval = 5 * time.Millisecond
	cfg.Telemetry = false
	cfg.Runtime.AckRepeatInterval = 20 * time.Millisecond
	cfg.Runtime.Recovery.ProgressStall = 2 * time.Second

	factoryA := newLinkedVolgaV6Factory()
	factoryB := newLinkedVolgaV6Factory()
	a := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factoryA.create)
	b := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factoryB.create)
	factoryA.peer = b.runtime
	factoryB.peer = a.runtime

	const packets = 10000
	payload := []byte{0x45, 0x00, 0x00, 0x28, 0xaa, 0x55, 0x01, 0x02}
	var received atomic.Int64
	b.Receive(func(data []byte) {
		if len(data) != len(payload) {
			t.Errorf("received payload length=%d want=%d", len(data), len(payload))
		}
		received.Add(1)
	})

	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		_ = a.Stop()
		t.Fatal(err)
	}
	defer func() {
		_ = a.Stop()
		_ = b.Stop()
	}()

	for i := 0; i < packets; i++ {
		if err := a.Send(payload); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for received.Load() != packets {
		if time.Now().After(deadline) {
			t.Fatalf("received=%d want=%d sender=%+v", received.Load(), packets, a.Snapshot(time.Now()))
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Allow the receiver's cumulative ACK tick to clear the final replay tail.
	for {
		snap := a.Snapshot(time.Now())
		if snap.Reliable.ReplayDepth == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replay did not drain after bulk delivery: %+v", snap.Reliable)
		}
		time.Sleep(2 * time.Millisecond)
	}

	physical := factoryA.sentSnapshot()
	dataPosts := 0
	for _, item := range physical {
		if item.frame.Kind == volgaV6FrameData {
			dataPosts++
		}
	}
	if dataPosts >= packets/2 {
		t.Fatalf("coalescing ineffective: packets=%d logical DATA posts=%d", packets, dataPosts)
	}
	if dataPosts > 1200 {
		t.Fatalf("operation pressure too high for offline bulk gate: DATA posts=%d", dataPosts)
	}

	stats := a.Stats()
	if stats.PacketsSent != packets {
		t.Fatalf("transport sent packets=%d want=%d", stats.PacketsSent, packets)
	}
}
