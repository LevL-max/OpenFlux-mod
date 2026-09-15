#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER V1..V6 and tools/volga_benchmark_harness.py.
# Goals:
#   1. keep the final serialized Volga HTTP body below an empirical safe budget;
#   2. fragment oversized transport records without rewriting TCP/IP packets;
#   3. reassemble fragments byte-for-byte before handing data to the upper transport;
#   4. preserve runtime tuning and add fragment/body-guard telemetry.

# ---------------------------------------------------------------------------
# transport.TransportConfig: add the final HTTP body budget.
# ---------------------------------------------------------------------------
p = Path("transport/transport.go")
s = p.read_text()
s = replace_once(
    s,
    "\tVolgaBatchMaxBytes     int\n\tVolgaTelemetry         bool\n",
    "\tVolgaBatchMaxBytes     int\n"
    "\tVolgaHTTPBodyLimit     int\n"
    "\tVolgaTelemetry         bool\n",
    "TransportConfig HTTP body limit",
)
p.write_text(s)

# ---------------------------------------------------------------------------
# main.go: expose the body budget as a runtime knob.
# ---------------------------------------------------------------------------
p = Path("main.go")
s = p.read_text()
s = replace_once(
    s,
    "\tvolgaBatchBytes := flag.Int(\"volga-batch-bytes\", 5000, \"Maximum framed bytes per Volga internal batch\")\n"
    "\tvolgaBatchTimeoutUs := flag.Int(\"volga-batch-timeout-us\", 2000, \"Volga internal batch flush timeout in microseconds\")\n",
    "\tvolgaBatchBytes := flag.Int(\"volga-batch-bytes\", 5000, \"Maximum framed bytes per Volga internal batch\")\n"
    "\tvolgaHTTPBodyLimit := flag.Int(\"volga-http-body-limit\", 8000, \"Maximum estimated serialized Volga relay HTTP body bytes\")\n"
    "\tvolgaBatchTimeoutUs := flag.Int(\"volga-batch-timeout-us\", 2000, \"Volga internal batch flush timeout in microseconds\")\n",
    "main body limit flag",
)
s = replace_once(
    s,
    "\tif *volgaBatchTimeoutUs < 100 || *volgaBatchTimeoutUs > 100000 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-batch-timeout-us: %d\", *volgaBatchTimeoutUs)\n"
    "\t}\n",
    "\tif *volgaBatchTimeoutUs < 100 || *volgaBatchTimeoutUs > 100000 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-batch-timeout-us: %d\", *volgaBatchTimeoutUs)\n"
    "\t}\n"
    "\tif *volgaHTTPBodyLimit < 2048 || *volgaHTTPBodyLimit > 65535 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-http-body-limit: %d\", *volgaHTTPBodyLimit)\n"
    "\t}\n",
    "main body limit validation",
)
s = replace_once(
    s,
    "\tconfig.VolgaBatchMaxBytes = *volgaBatchBytes\n"
    "\tconfig.VolgaBatchTimeout = time.Duration(*volgaBatchTimeoutUs) * time.Microsecond\n",
    "\tconfig.VolgaBatchMaxBytes = *volgaBatchBytes\n"
    "\tconfig.VolgaHTTPBodyLimit = *volgaHTTPBodyLimit\n"
    "\tconfig.VolgaBatchTimeout = time.Duration(*volgaBatchTimeoutUs) * time.Microsecond\n",
    "main body limit assignment",
)
s = replace_once(
    s,
    "\t\tlog.Printf(\"Volga benchmark: workers=%d queue=%d batch_packets=%d batch_bytes=%d timeout=%dus normalize_mtu=%d telemetry=%t\",\n"
    "\t\t\t*volgaWorkers, *volgaQueueSize, *volgaBatchPackets, *volgaBatchBytes, *volgaBatchTimeoutUs, *volgaNormalizeMTU, *volgaTelemetry)\n",
    "\t\tlog.Printf(\"Volga benchmark: workers=%d queue=%d batch_packets=%d batch_bytes=%d http_body_limit=%d timeout=%dus normalize_mtu=%d telemetry=%t\",\n"
    "\t\t\t*volgaWorkers, *volgaQueueSize, *volgaBatchPackets, *volgaBatchBytes, *volgaHTTPBodyLimit, *volgaBatchTimeoutUs, *volgaNormalizeMTU, *volgaTelemetry)\n",
    "main benchmark log",
)
p.write_text(s)

# ---------------------------------------------------------------------------
# vyandex.go: body-aware batching and transport record fragmentation.
# ---------------------------------------------------------------------------
p = Path("transport/yandex/vyandex.go")
s = p.read_text()

s = replace_once(
    s,
    "\tBatchSize     int\n\tBatchTimeout  time.Duration\n\tBatchMaxBytes int\n",
    "\tBatchSize     int\n"
    "\tBatchTimeout  time.Duration\n"
    "\tBatchMaxBytes int\n"
    "\tHTTPBodyLimit int\n",
    "VolgaConfig HTTP body limit",
)
s = replace_once(
    s,
    "\t\tBatchSize:     20,\n\t\tBatchTimeout:  2 * time.Millisecond,\n\t\tBatchMaxBytes: 5000,\n",
    "\t\tBatchSize:     20,\n"
    "\t\tBatchTimeout:  2 * time.Millisecond,\n"
    "\t\tBatchMaxBytes: 5000,\n"
    "\t\tHTTPBodyLimit: 8000,\n",
    "VolgaConfig HTTP body default",
)
s = replace_once(
    s,
    "\tHTTP504s                    atomic.Uint64\n"
    "\tFrontierAdvances            atomic.Uint64\n",
    "\tHTTP504s                    atomic.Uint64\n"
    "\tBodyGuardTrips              atomic.Uint64\n"
    "\tFragmentedPackets           atomic.Uint64\n"
    "\tFragmentsSent               atomic.Uint64\n"
    "\tFragmentsRecv               atomic.Uint64\n"
    "\tReassembledPackets          atomic.Uint64\n"
    "\tReassemblyDrops             atomic.Uint64\n"
    "\tFrontierAdvances            atomic.Uint64\n",
    "VolgaStats fragment fields",
)

# Insert record framing helpers before relayClient.
marker = "type relayClient struct {\n"
if s.count(marker) != 1:
    raise SystemExit("relayClient marker missing or duplicated")
helpers = r'''const (
	volgaRecordMagic          = "OFX1"
	volgaRecordNormal   byte  = 0
	volgaRecordFragment byte  = 1
	volgaHTTPEnvelopeReserve  = 512
	volgaFragmentMetaBytes    = 12 // id(4) + index(2) + count(2) + total(4)
	volgaReassemblyTimeout    = 30 * time.Second
)

func estimateVolgaHTTPBody(recordBytes, recordCount int) int {
	if recordBytes < 0 || recordCount < 0 {
		return int(^uint(0) >> 1)
	}
	blobBytes := recordBytes + recordCount*2 // uint16 length prefix per record
	return base64.StdEncoding.EncodedLen(blobBytes) + volgaHTTPEnvelopeReserve
}

func maxVolgaRecordBytes(bodyLimit int) int {
	if bodyLimit <= volgaHTTPEnvelopeReserve {
		return 0
	}
	lo, hi := 0, 32767
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if estimateVolgaHTTPBody(mid, 1) <= bodyLimit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func makeVolgaRecords(data []byte, bodyLimit int, fragmentID uint32) ([][]byte, bool, error) {
	const normalHeader = 5 // magic(4) + type(1)
	const fragmentHeader = 5 + volgaFragmentMetaBytes

	maxRecord := maxVolgaRecordBytes(bodyLimit)
	if maxRecord <= fragmentHeader {
		return nil, false, fmt.Errorf("Volga HTTP body limit too small: %d", bodyLimit)
	}

	if len(data)+normalHeader <= maxRecord {
		record := make([]byte, normalHeader+len(data))
		copy(record[:4], volgaRecordMagic)
		record[4] = volgaRecordNormal
		copy(record[normalHeader:], data)
		return [][]byte{record}, false, nil
	}

	chunkSize := maxRecord - fragmentHeader
	count := (len(data) + chunkSize - 1) / chunkSize
	if count < 1 || count > 65535 {
		return nil, false, fmt.Errorf("invalid Volga fragment count %d for %d bytes", count, len(data))
	}
	if len(data) > int(^uint32(0)) {
		return nil, false, fmt.Errorf("Volga payload too large for fragment header: %d", len(data))
	}

	records := make([][]byte, 0, count)
	for i, off := 0, 0; off < len(data); i++ {
		end := off + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		record := make([]byte, fragmentHeader+len(chunk))
		copy(record[:4], volgaRecordMagic)
		record[4] = volgaRecordFragment
		binary.BigEndian.PutUint32(record[5:9], fragmentID)
		binary.BigEndian.PutUint16(record[9:11], uint16(i))
		binary.BigEndian.PutUint16(record[11:13], uint16(count))
		binary.BigEndian.PutUint32(record[13:17], uint32(len(data)))
		copy(record[17:], chunk)
		records = append(records, record)
		off = end
	}
	return records, true, nil
}

type volgaFragmentAssembly struct {
	created  time.Time
	total    int
	parts    [][]byte
	received int
	bytes    int
}

type volgaFragmentReassembler struct {
	items      map[uint32]*volgaFragmentAssembly
	timeout    time.Duration
	maxPayload int
	stats      *VolgaStats
	lastGC     time.Time
}

func newVolgaFragmentReassembler(timeout time.Duration, maxPayload int, stats *VolgaStats) *volgaFragmentReassembler {
	return &volgaFragmentReassembler{
		items:      make(map[uint32]*volgaFragmentAssembly),
		timeout:    timeout,
		maxPayload: maxPayload,
		stats:      stats,
		lastGC:     time.Now(),
	}
}

func (r *volgaFragmentReassembler) gc(now time.Time) {
	if now.Sub(r.lastGC) < time.Second {
		return
	}
	r.lastGC = now
	for id, a := range r.items {
		if now.Sub(a.created) > r.timeout {
			delete(r.items, id)
			if r.stats != nil {
				r.stats.ReassemblyDrops.Add(1)
			}
		}
	}
}

func (r *volgaFragmentReassembler) accept(record []byte) [][]byte {
	now := time.Now()
	r.gc(now)

	if len(record) < 5 || string(record[:4]) != volgaRecordMagic {
		// Backward-compatible pass-through for pre-fragment benchmark records.
		return [][]byte{record}
	}

	switch record[4] {
	case volgaRecordNormal:
		return [][]byte{record[5:]}

	case volgaRecordFragment:
		if len(record) < 17 {
			if r.stats != nil {
				r.stats.ReassemblyDrops.Add(1)
			}
			return nil
		}
		id := binary.BigEndian.Uint32(record[5:9])
		index := int(binary.BigEndian.Uint16(record[9:11]))
		count := int(binary.BigEndian.Uint16(record[11:13]))
		total := int(binary.BigEndian.Uint32(record[13:17]))
		chunk := record[17:]

		if r.stats != nil {
			r.stats.FragmentsRecv.Add(1)
		}
		if count < 1 || index < 0 || index >= count || total < 1 || total > r.maxPayload || len(chunk) == 0 {
			if r.stats != nil {
				r.stats.ReassemblyDrops.Add(1)
			}
			return nil
		}

		a := r.items[id]
		if a == nil {
			a = &volgaFragmentAssembly{
				created: now,
				total:   total,
				parts:   make([][]byte, count),
			}
			r.items[id] = a
		} else if a.total != total || len(a.parts) != count {
			delete(r.items, id)
			if r.stats != nil {
				r.stats.ReassemblyDrops.Add(1)
			}
			return nil
		}

		if a.parts[index] == nil {
			cp := append([]byte(nil), chunk...)
			a.parts[index] = cp
			a.received++
			a.bytes += len(cp)
		}
		if a.received != len(a.parts) {
			return nil
		}
		if a.bytes != a.total {
			delete(r.items, id)
			if r.stats != nil {
				r.stats.ReassemblyDrops.Add(1)
			}
			return nil
		}

		out := make([]byte, 0, a.total)
		for _, part := range a.parts {
			if part == nil {
				delete(r.items, id)
				if r.stats != nil {
					r.stats.ReassemblyDrops.Add(1)
				}
				return nil
			}
			out = append(out, part...)
		}
		delete(r.items, id)
		if len(out) != a.total {
			if r.stats != nil {
				r.stats.ReassemblyDrops.Add(1)
			}
			return nil
		}
		if r.stats != nil {
			r.stats.ReassembledPackets.Add(1)
		}
		return [][]byte{out}
	}

	if r.stats != nil {
		r.stats.ReassemblyDrops.Add(1)
	}
	return nil
}

'''
s = s.replace(marker, helpers + marker, 1)

s = replace_once(
    s,
    "\tbundleID atomic.Uint64\n\tseq      atomic.Uint64\n\tlocalID  atomic.Uint64\n\n"
    "\tmu                         sync.Mutex\n",
    "\tbundleID    atomic.Uint64\n"
    "\tseq         atomic.Uint64\n"
    "\tlocalID     atomic.Uint64\n"
    "\tfragmentSeq atomic.Uint64\n"
    "\tsendMu      sync.Mutex\n\n"
    "\tmu                         sync.Mutex\n",
    "relay fragment fields",
)

# Replace Send wholesale so one logical packet is either one normal record or
# an atomic set of fragment records. The huge queue means this lock only covers
# fast enqueue work; it prevents half-enqueued fragmented packets.
start = s.index("func (r *relayClient) Send(data []byte) error {")
end = s.index("func (r *relayClient) batcher() {")
send_func = r'''func (r *relayClient) Send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > r.config.MaxPayloadBytes {
		return fmt.Errorf("packet too large: %d > %d", len(data), r.config.MaxPayloadBytes)
	}

	packetBytes := uint64(len(data))
	atomicMax(&r.stats.MaxPacketBytes, packetBytes)
	if len(data) > r.config.BatchMaxBytes {
		r.stats.OversizePackets.Add(1)
	}

	fragmentID := uint32(r.fragmentSeq.Add(1))
	records, fragmented, err := makeVolgaRecords(data, r.config.HTTPBodyLimit, fragmentID)
	if err != nil {
		return err
	}
	if fragmented {
		r.stats.FragmentedPackets.Add(1)
		r.stats.FragmentsSent.Add(uint64(len(records)))
	}

	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	select {
	case <-r.ctx.Done():
		return fmt.Errorf("relay stopped")
	default:
	}

	if cap(r.packetQueue)-len(r.packetQueue) < len(records) {
		r.stats.QueueDrops.Add(1)
		return fmt.Errorf("queue full")
	}
	for _, record := range records {
		r.packetQueue <- record
	}
	return nil
}

'''
s = s[:start] + send_func + s[end:]

# Make the central batcher enforce both the existing raw cap and an estimate
# of the FINAL serialized HTTP body. The 512-byte reserve is intentionally
# larger than the ~420-430 bytes observed in the benchmark traces.
old_case = r'''		case pkt := <-r.packetQueue:
			// Flush the current batch BEFORE appending a packet that would cross
			// the byte cap. Flushing only after append can overshoot the HTTP
			// request-size ceiling and cause Volga to return 413.
			if len(batch) > 0 && totalBytes+len(pkt) > r.config.BatchMaxBytes {
				if !flush() {
					return
				}
			}

			batch = append(batch, pkt)
			totalBytes += len(pkt)

			if len(batch) >= batchCap || totalBytes >= r.config.BatchMaxBytes {
				if !flush() {
					return
				}
			} else if len(batch) == 1 {
				armTimer()
			}
'''
new_case = r'''		case pkt := <-r.packetQueue:
			candidateBytes := totalBytes + len(pkt)
			candidateCount := len(batch) + 1
			overRaw := r.config.BatchMaxBytes > 0 && candidateBytes > r.config.BatchMaxBytes
			overBody := r.config.HTTPBodyLimit > 0 && estimateVolgaHTTPBody(candidateBytes, candidateCount) > r.config.HTTPBodyLimit

			if len(batch) > 0 && (overRaw || overBody) {
				if !flush() {
					return
				}
			}

			batch = append(batch, pkt)
			totalBytes += len(pkt)

			atBodyLimit := r.config.HTTPBodyLimit > 0 && estimateVolgaHTTPBody(totalBytes, len(batch)) >= r.config.HTTPBodyLimit
			if len(batch) >= batchCap ||
				(r.config.BatchMaxBytes > 0 && totalBytes >= r.config.BatchMaxBytes) || atBodyLimit {
				if !flush() {
					return
				}
			} else if len(batch) == 1 {
				armTimer()
			}
'''
s = replace_once(s, old_case, new_case, "body-aware batcher")

# Exact local guard on the already-serialized JSON. It should never trip if
# the estimator and fragmentation logic are correct; if it does, do not send
# a known-oversized request to Yandex.
s = replace_once(
    s,
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\trawBytes := uint64(totalBytes)\n",
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\tif r.config.HTTPBodyLimit > 0 && len(bodyCopy) > r.config.HTTPBodyLimit {\n"
    "\t\tr.stats.BodyGuardTrips.Add(1)\n"
    "\t\treturn fmt.Errorf(\"local Volga body guard: %d > %d\", len(bodyCopy), r.config.HTTPBodyLimit)\n"
    "\t}\n"
    "\trawBytes := uint64(totalBytes)\n",
    "exact body guard",
)

# wsListener owns one serial receive loop, so reassembly does not need locks.
s = replace_once(
    s,
    "\trelay  *relayClient\n\tonData func([]byte)\n",
    "\trelay       *relayClient\n"
    "\treassembler *volgaFragmentReassembler\n"
    "\tonData      func([]byte)\n",
    "ws reassembler field",
)
s = replace_once(
    s,
    "\t\trelay:  relay,\n\t\tonData: onData,\n",
    "\t\trelay:       relay,\n"
    "\t\treassembler: newVolgaFragmentReassembler(volgaReassemblyTimeout, cfg.MaxPayloadBytes, stats),\n"
    "\t\tonData:      onData,\n",
    "ws reassembler constructor",
)

old_decode = r'''		packets := decodeBatch(decoded)
		w.stats.PacketsRecv.Add(uint64(len(packets)))
		w.stats.BytesReceived.Add(uint64(len(decoded)))
		for _, pkt := range packets {
			if w.onData != nil {
				w.onData(pkt)
			}
		}
'''
new_decode = r'''		records := decodeBatch(decoded)
		w.stats.BytesReceived.Add(uint64(len(decoded)))
		for _, record := range records {
			packets := w.reassembler.accept(record)
			w.stats.PacketsRecv.Add(uint64(len(packets)))
			for _, pkt := range packets {
				if w.onData != nil {
					w.onData(pkt)
				}
			}
		}
'''
s = replace_once(s, old_decode, new_decode, "receive reassembly")

# Runtime override.
s = replace_once(
    s,
    "\tif cfg.VolgaBatchMaxBytes > 0 {\n"
    "\t\tvolgaCfg.BatchMaxBytes = cfg.VolgaBatchMaxBytes\n"
    "\t}\n"
    "\tvolgaCfg.Telemetry = cfg.VolgaTelemetry\n",
    "\tif cfg.VolgaBatchMaxBytes > 0 {\n"
    "\t\tvolgaCfg.BatchMaxBytes = cfg.VolgaBatchMaxBytes\n"
    "\t}\n"
    "\tif cfg.VolgaHTTPBodyLimit > 0 {\n"
    "\t\tvolgaCfg.HTTPBodyLimit = cfg.VolgaHTTPBodyLimit\n"
    "\t}\n"
    "\tvolgaCfg.Telemetry = cfg.VolgaTelemetry\n",
    "Volga runtime body limit override",
)

# Add cumulative fragment/body-guard telemetry without exposing session values.
bench_anchor = (
    "\t\t\tutils.Debugf(\"[VOLGA-BENCH] age=%v | attempts %d/5s avg=%.1fms max=%dms | status 401=%d 504=%d | req-avg body=%d raw=%d | frontier +%d same-gen=%d\",\n"
    "\t\t\t\tt.BaseTransport.Stats().Uptime.Round(time.Second), attemptDelta, avgHTTPms, t.stats.HTTPMaxDurationMicros.Load()/1000,\n"
    "\t\t\t\thttp401-last401, http504-last504, avgBody, avgRaw, frontier-lastFrontier, sameFrontier-lastSameFrontier)\n\n"
)
bench_new = bench_anchor + (
    "\t\t\tutils.Debugf(\"[VOLGA-FRAG] fragmented=%d fragments-tx=%d fragments-rx=%d reassembled=%d drops=%d body-guard=%d limit=%d\",\n"
    "\t\t\t\tt.stats.FragmentedPackets.Load(), t.stats.FragmentsSent.Load(), t.stats.FragmentsRecv.Load(),\n"
    "\t\t\t\tt.stats.ReassembledPackets.Load(), t.stats.ReassemblyDrops.Load(), t.stats.BodyGuardTrips.Load(), t.config.HTTPBodyLimit)\n\n"
)
s = replace_once(s, bench_anchor, bench_new, "fragment telemetry output")

p.write_text(s)

# ---------------------------------------------------------------------------
# Tests generated into the transformed workspace.
# ---------------------------------------------------------------------------
test = r'''package yandex

import (
	"bytes"
	"testing"
	"time"
)

func TestVolgaNormalRecordRoundTrip(t *testing.T) {
	in := bytes.Repeat([]byte{0x5a}, 4396)
	records, fragmented, err := makeVolgaRecords(in, 8000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fragmented || len(records) != 1 {
		t.Fatalf("expected one normal record, fragmented=%v records=%d", fragmented, len(records))
	}
	if got := estimateVolgaHTTPBody(len(records[0]), 1); got > 8000 {
		t.Fatalf("normal record estimate exceeds body limit: %d", got)
	}
	r := newVolgaFragmentReassembler(time.Second, 5_000_000, &VolgaStats{})
	out := r.accept(records[0])
	if len(out) != 1 || !bytes.Equal(out[0], in) {
		t.Fatal("normal record round-trip mismatch")
	}
}

func TestVolgaFragmentRoundTripOutOfOrder(t *testing.T) {
	in := make([]byte, 10189)
	for i := range in {
		in[i] = byte(i * 31)
	}
	records, fragmented, err := makeVolgaRecords(in, 8000, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !fragmented || len(records) < 2 {
		t.Fatalf("expected fragmentation, fragmented=%v records=%d", fragmented, len(records))
	}
	for i, rec := range records {
		if got := estimateVolgaHTTPBody(len(rec), 1); got > 8000 {
			t.Fatalf("fragment %d estimate exceeds body limit: %d", i, got)
		}
	}

	stats := &VolgaStats{}
	r := newVolgaFragmentReassembler(time.Second, 5_000_000, stats)
	var out [][]byte
	for i := len(records) - 1; i >= 0; i-- {
		if pkt := r.accept(records[i]); len(pkt) > 0 {
			out = pkt
		}
	}
	if len(out) != 1 || !bytes.Equal(out[0], in) {
		t.Fatal("fragmented round-trip mismatch")
	}
	if stats.ReassembledPackets.Load() != 1 || stats.ReassemblyDrops.Load() != 0 {
		t.Fatalf("unexpected reassembly stats: complete=%d drops=%d", stats.ReassembledPackets.Load(), stats.ReassemblyDrops.Load())
	}
}

func TestVolgaBodyEstimateRejectsTwoLargeRecords(t *testing.T) {
	a, _, err := makeVolgaRecords(bytes.Repeat([]byte{1}, 2949), 8000, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := makeVolgaRecords(bytes.Repeat([]byte{2}, 2949), 8000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatal("unexpected fragmentation for 2949-byte records")
	}
	if estimateVolgaHTTPBody(len(a[0])+len(b[0]), 2) <= 8000 {
		t.Fatal("expected two 2949-byte records to exceed safe body estimate")
	}
}

func TestVolgaLegacyRecordPassThrough(t *testing.T) {
	legacy := []byte{0x00, 0x11, 0x22, 0x33}
	r := newVolgaFragmentReassembler(time.Second, 5_000_000, &VolgaStats{})
	out := r.accept(legacy)
	if len(out) != 1 || !bytes.Equal(out[0], legacy) {
		t.Fatal("legacy pass-through mismatch")
	}
}
'''
Path("transport/yandex/volga_fragment_test.go").write_text(test)

print("Volga body-aware fragmentation v1 transform applied")
