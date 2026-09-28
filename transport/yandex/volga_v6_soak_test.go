//go:build volga

package yandex

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

// Opt-in: 600 MiB through production batching, HTTP JSON/base64 serialization,
// websocket decoding, reliability, and serialized ACKs in one logical session.
// This is an in-memory relay, not a WAN throughput measurement.
func TestVolgaV6RegressionWireSoak(t *testing.T) {
	if os.Getenv("OPENFLUX_LOCAL_SOAK") != "1" {
		t.Skip("set OPENFLUX_LOCAL_SOAK=1 for the 600 MiB local soak")
	}
	const rounds = 6
	const bytesPerRound = 100 << 20
	const packetSize = 1500
	const packetsPerRound = (bytesPerRound + packetSize - 1) / packetSize
	var a, b *YandexVolgaV6Transport
	var dataPosts, ackPosts *atomic.Int64
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.Telemetry = false
	cfg.QueueSize = 4096
	cfg.SendQueueSize = 512
	a = newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(g uint64, fn func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		wire, posts := localVolgaV6Wire(t, func(f volgaV6WireFrame) { b.runtime.handleIncoming(f) })
		wire.generation = g
		dataPosts = posts
		return &volgaV6StartedWire{wire}, nil
	})
	b = newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(g uint64, fn func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		wire, posts := localVolgaV6Wire(t, func(f volgaV6WireFrame) { a.runtime.handleIncoming(f) })
		wire.generation = g
		ackPosts = posts
		return &volgaV6StartedWire{wire}, nil
	})
	var mu sync.Mutex
	seen := make([]bool, rounds*packetsPerRound)
	var receivedBytes, receivedPackets atomic.Int64
	var invalid atomic.Bool
	var gotHash, wantHash [sha256.Size]byte
	b.Receive(func(data []byte) {
		if len(data) < 8 {
			invalid.Store(true)
			return
		}
		id := binary.BigEndian.Uint64(data)
		hash := sha256.Sum256(data)
		mu.Lock()
		defer mu.Unlock()
		if id >= uint64(len(seen)) || seen[id] {
			invalid.Store(true)
			return
		}
		seen[id] = true
		for i := range gotHash {
			gotHash[i] ^= hash[i]
		}
		receivedBytes.Add(int64(len(data)))
		receivedPackets.Add(1)
	})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	session := a.Snapshot(time.Now()).Reliable.Session
	template := make([]byte, packetSize)
	for i := range template {
		template[i] = byte(i % 251)
	}
	for round := 0; round < rounds; round++ {
		started := time.Now()
		deadline := started.Add(60 * time.Second)
		remaining := bytesPerRound
		for p := 0; remaining > 0; p++ {
			size := min(remaining, packetSize)
			payload := append([]byte(nil), template[:size]...)
			binary.BigEndian.PutUint64(payload, uint64(round*packetsPerRound+p))
			hash := sha256.Sum256(payload)
			for i := range wantHash {
				wantHash[i] ^= hash[i]
			}
			for {
				err := a.Send(payload)
				if err == nil {
					break
				}
				if !errors.Is(err, errVolgaV6TransportQueueFull) {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					t.Fatalf("round %d stalled while admitting data: %+v", round+1, a.Snapshot(time.Now()))
				}
				time.Sleep(time.Millisecond)
			}
			remaining -= size
		}
		for receivedBytes.Load() != int64((round+1)*bytesPerRound) || a.Snapshot(time.Now()).Reliable.ReplayDepth != 0 {
			if invalid.Load() || time.Now().After(deadline) {
				t.Fatalf("round %d incomplete/corrupt: bytes=%d invalid=%t sender=%+v", round+1, receivedBytes.Load(), invalid.Load(), a.Snapshot(time.Now()))
			}
			time.Sleep(time.Millisecond)
		}
		mu.Lock()
		matches := gotHash == wantHash
		mu.Unlock()
		snap := a.Snapshot(time.Now())
		if !matches || snap.Reliable.Session != session || snap.Carrier.ActiveGeneration != 1 {
			t.Fatalf("round %d hash/session changed: %+v", round+1, snap)
		}
		t.Logf("round=%d cumulative_bytes=%d unique_packets=%d replay=%d pending=%d session=%d generation=%d local_elapsed=%s data_posts=%d ack_posts=%d integrity=OK", round+1, receivedBytes.Load(), receivedPackets.Load(), snap.Reliable.ReplayDepth, b.Snapshot(time.Now()).Receiver.Pending, session, snap.Carrier.ActiveGeneration, time.Since(started), dataPosts.Load(), ackPosts.Load())
		if round == 2 {
			time.Sleep(3 * time.Second)
		}
	}
	if invalid.Load() || receivedPackets.Load() != rounds*packetsPerRound {
		t.Fatal("duplicate or missing packet")
	}
}
