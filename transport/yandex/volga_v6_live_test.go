//go:build volga

package yandex

// Explicit opt-in diagnostic binary for two authorized hosts. It only uses
// outbound Volga HTTP/WS connections: no raw sockets, listeners or host routing.
import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

const liveHeader = 32
const livePayload = 1500 - liveHeader

func liveRecord(token [16]byte, kind byte, round, index int, data []byte) []byte {
	p := make([]byte, liveHeader+len(data))
	copy(p, "V6BT")
	copy(p[4:20], token[:])
	p[20] = kind
	binary.BigEndian.PutUint32(p[24:], uint32(round))
	binary.BigEndian.PutUint32(p[28:], uint32(index))
	copy(p[liveHeader:], data)
	return p
}

type liveLedger struct {
	seen         []bool
	count, bytes int
	hash         [32]byte
}

func (l *liveLedger) accept(index, total int, data []byte) error {
	count := (total + livePayload - 1) / livePayload
	if index < 0 || index >= count || len(data) != min(livePayload, total-index*livePayload) {
		return fmt.Errorf("bad DATA index/length")
	}
	if l.seen == nil {
		l.seen = make([]bool, count)
	}
	if l.seen[index] {
		return fmt.Errorf("duplicate DATA index %d", index)
	}
	l.seen[index] = true
	l.count++
	l.bytes += len(data)
	hash := sha256.Sum256(data)
	for i := range hash {
		l.hash[i] ^= hash[i]
	}
	return nil
}

func TestVolgaV6LiveLedger(t *testing.T) {
	var ledger liveLedger
	data := bytes.Repeat([]byte{7}, livePayload)
	if err := ledger.accept(1, livePayload+17, data[:17]); err != nil {
		t.Fatal(err)
	}
	if err := ledger.accept(0, livePayload+17, data); err != nil {
		t.Fatal(err)
	}
	if ledger.bytes != livePayload+17 || ledger.count != 2 {
		t.Fatal("incorrect byte accounting")
	}
	if ledger.accept(0, livePayload+17, data) == nil {
		t.Fatal("duplicate counted as useful throughput")
	}
	if ledger.accept(2, livePayload+17, data) == nil {
		t.Fatal("out-of-range packet accepted")
	}
}

func TestVolgaV6LiveDiagnostic(t *testing.T) {
	doc, role, run := os.Getenv("OPENFLUX_V6_DOC"), os.Getenv("OPENFLUX_V6_ROLE"), os.Getenv("OPENFLUX_V6_RUN")
	if doc == "" || role == "" || run == "" {
		t.Skip("live diagnostic requires explicit document, role and run ID")
	}
	if role != "sender" && role != "receiver" {
		t.Fatal("invalid role")
	}
	readInt := func(key string, fallback, maximum int) int {
		s := os.Getenv(key)
		if s == "" {
			return fallback
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > maximum {
			t.Fatalf("invalid %s", key)
		}
		return n
	}
	rounds := readInt("OPENFLUX_V6_ROUNDS", 6, 10)
	size := readInt("OPENFLUX_V6_MIB", 50, 100) << 20
	rate := readInt("OPENFLUX_V6_MBPS", 35, 40)
	idle := readInt("OPENFLUX_V6_IDLE_SECONDS", 10, 300)
	if rounds == 0 || size == 0 || rate == 0 {
		t.Fatal("zero test dimensions")
	}
	seed := sha256.Sum256([]byte(run))
	var token [16]byte
	copy(token[:], seed[:16])
	cfg := DefaultVolgaV6TransportConfig([]string{doc})
	cfg.Telemetry = false
	cfg.Yandex.HTTPProtocol = os.Getenv("OPENFLUX_V6_HTTP")
	cfg.Yandex.RelayEnvelope = os.Getenv("OPENFLUX_V6_ENVELOPE")
	if cfg.Yandex.RelayEnvelope != "" && cfg.Yandex.RelayEnvelope != "minimal" {
		t.Fatal("invalid diagnostic relay envelope")
	}
	cfg.Yandex.RelayPostsPerSecond = float64(readInt("OPENFLUX_V6_POST_RATE", 0, 1000))
	cfg.SendWorkers = readInt("OPENFLUX_V6_WORKERS", 32, 128)
	if cfg.SendWorkers == 0 {
		t.Fatal("zero workers")
	}
	cfg.QueueSize = 512
	cfg.SendQueueSize = 64
	factory := newVolgaV6SingleDocumentCarrierFactory([]string{doc}, cfg.Yandex)
	if pool := os.Getenv("OPENFLUX_V6_POOL"); pool != "" {
		documents := strings.Split(pool, ",")
		for _, document := range documents {
			if strings.TrimSpace(document) == "" {
				t.Fatal("empty pool document")
			}
		}
		factory = livePoolFactory(documents, cfg.Yandex)
		cfg.Runtime.CarrierStartTimeout = 30 * time.Second
	}
	tr := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factory)
	var printMu sync.Mutex
	emit := func(kind string, data any) {
		printMu.Lock()
		defer printMu.Unlock()
		b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "role": role, "event": kind, "data": data})
		fmt.Printf("V6LIVE %s\n", b)
	}
	var mu sync.Mutex
	ledgers := make([]liveLedger, rounds)
	completed := make(map[int][32]byte)
	ready := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	var totalReceived atomic.Int64
	var receiveError atomic.Bool
	sendControl := func(kind byte, round int, data []byte) { _ = tr.Send(liveRecord(token, kind, round, 0, data)) }
	tr.Receive(func(p []byte) {
		if len(p) < liveHeader || string(p[:4]) != "V6BT" || !bytes.Equal(p[4:20], token[:]) {
			return
		}
		kind, round, index := p[20], int(binary.BigEndian.Uint32(p[24:])), int(binary.BigEndian.Uint32(p[28:]))
		switch kind {
		case 'H':
			if role == "receiver" {
				sendControl('R', 0, nil)
			}
		case 'R':
			if role == "sender" {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		case 'D':
			if role != "receiver" || round < 0 || round >= rounds {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if err := ledgers[round].accept(index, size, p[liveHeader:]); err != nil {
				receiveError.Store(true)
				emit("invalid", err.Error())
				return
			}
			totalReceived.Add(int64(len(p) - liveHeader))
			if ledgers[round].bytes == size {
				emit("round_received", map[string]any{"round": round + 1, "bytes": size, "packets": ledgers[round].count, "hash": hex.EncodeToString(ledgers[round].hash[:])})
				sendControl('C', round, ledgers[round].hash[:])
			}
		case 'C':
			if role != "sender" || round < 0 || round >= rounds || len(p) != liveHeader+32 {
				return
			}
			mu.Lock()
			var h [32]byte
			copy(h[:], p[liveHeader:])
			completed[round] = h
			mu.Unlock()
		case 'Q':
			if role == "receiver" {
				select {
				case done <- struct{}{}:
				default:
				}
			}
		}
	})
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	emit("started", map[string]any{"rounds": rounds, "bytes_per_round": size, "offered_mbps_cap": rate, "compression": false, "workers": cfg.SendWorkers, "queue": cfg.QueueSize, "send_queue": cfg.SendQueueSize, "http_mode": cfg.Yandex.HTTPProtocol})
	stopTelemetry := make(chan struct{})
	defer close(stopTelemetry)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTelemetry:
				return
			case now := <-ticker.C:
				s := tr.Snapshot(now)
				s.Carrier.ActiveHealth.Document = ""
				emit("snapshot", map[string]any{"runtime": s, "unique_received_bytes": totalReceived.Load(), "queue": len(tr.queue), "send_queue": len(tr.sendQueue)})
				liveLaneSnapshots(tr)
			}
		}
	}()
	if role == "receiver" {
		select {
		case <-done:
		case <-time.After(12 * time.Minute):
			t.Fatal("receiver lifetime expired")
		}
		time.Sleep(2 * time.Second)
		if receiveError.Load() || totalReceived.Load() != int64(rounds*size) {
			t.Fatalf("invalid/incomplete receiver bytes=%d", totalReceived.Load())
		}
		return
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		sendControl('H', 0, nil)
		select {
		case <-ready:
			goto connected
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			t.Fatal("no peer handshake within 45s")
		}
	}
connected:
	rng := rand.NewChaCha8(seed)
	for round := 0; round < rounds; round++ {
		start := time.Now()
		deadline := start.Add(120 * time.Second)
		var want [32]byte
		offered := 0
		for index := 0; offered < size; index++ {
			n := min(livePayload, size-offered)
			payload := make([]byte, n)
			_, _ = rng.Read(payload)
			h := sha256.Sum256(payload)
			for i := range h {
				want[i] ^= h[i]
			}
			packet := liveRecord(token, 'D', round, index, payload)
			for tr.Send(packet) != nil {
				if time.Now().After(deadline) {
					t.Fatalf("round %d admission timed out", round+1)
				}
				time.Sleep(time.Millisecond)
			}
			offered += n
			if index%32 == 31 {
				due := start.Add(time.Duration(float64(offered) * 8 / float64(rate*1000000) * float64(time.Second)))
				if wait := time.Until(due); wait > 0 {
					time.Sleep(wait)
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d send timed out", round+1)
			}
		}
		for {
			mu.Lock()
			got, ok := completed[round]
			mu.Unlock()
			if ok {
				if got != want {
					t.Fatalf("round %d integrity mismatch", round+1)
				}
				elapsed := time.Since(start).Seconds()
				emit("round_complete", map[string]any{"round": round + 1, "bytes": size, "seconds": elapsed, "payload_mbps": float64(size) * 8 / elapsed / 1000000, "integrity": "OK"})
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d delivery confirmation timed out", round+1)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if round == 2 && idle > 0 {
			emit("idle", idle)
			time.Sleep(time.Duration(idle) * time.Second)
		}
	}
	sendControl('Q', 0, nil)
	time.Sleep(2 * time.Second)
}
