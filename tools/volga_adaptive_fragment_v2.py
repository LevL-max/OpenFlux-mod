#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER tools/volga_body_fragment_v1.py.
# V2 restores the original fast path for ordinary packets: they are sent
# byte-for-byte with no OFX1 wrapper. Only oversized packets use OFX1 fragment
# records. A conservative proactive threshold catches the observed 7293+
# coalesced packets, while an exact serialized-body guard + 413 retry handles
# the narrow boundary around 5845 bytes without packet loss.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

# Extra adaptive telemetry.
s = replace_once(
    s,
    "\tReassemblyDrops             atomic.Uint64\n\tFrontierAdvances            atomic.Uint64\n",
    "\tReassemblyDrops             atomic.Uint64\n"
    "\tAdaptiveLocalSplits         atomic.Uint64\n"
    "\tAdaptive413Retries          atomic.Uint64\n"
    "\tFrontierAdvances            atomic.Uint64\n",
    "adaptive stats fields",
)

# Replace the V1 record creator with V2 helpers. Keep support for V1 normal
# records on receive so mixed benchmark binaries fail gracefully during swaps.
start = s.index('const (\n\tvolgaRecordMagic')
end = s.index('type volgaFragmentAssembly struct {')
helpers = r'''const (
	volgaRecordMagic                      = "OFX1"
	volgaRecordNormal               byte  = 0 // V1 receive compatibility only.
	volgaRecordFragment             byte  = 1
	volgaHTTPEnvelopeReserve              = 512
	volgaFragmentMetaBytes                = 12 // id(4) + index(2) + count(2) + total(4)
	volgaReassemblyTimeout                = 30 * time.Second
	volgaAdaptiveRawFragmentAbove         = 6000
	volgaAdaptiveSingleHTTPBodyLimit uint64 = 8224
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

func isVolgaFragmentRecord(record []byte) bool {
	return len(record) >= 5 && string(record[:4]) == volgaRecordMagic && record[4] == volgaRecordFragment
}

func makeVolgaFragments(data []byte, bodyLimit int, fragmentID uint32) ([][]byte, error) {
	const fragmentHeader = 5 + volgaFragmentMetaBytes

	maxRecord := maxVolgaRecordBytes(bodyLimit)
	if maxRecord <= fragmentHeader {
		return nil, fmt.Errorf("Volga HTTP body limit too small: %d", bodyLimit)
	}

	chunkSize := maxRecord - fragmentHeader
	count := (len(data) + chunkSize - 1) / chunkSize
	if count < 1 || count > 65535 {
		return nil, fmt.Errorf("invalid Volga fragment count %d for %d bytes", count, len(data))
	}
	if len(data) > int(^uint32(0)) {
		return nil, fmt.Errorf("Volga payload too large for fragment header: %d", len(data))
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
	return records, nil
}

'''
s = s[:start] + helpers + s[end:]

# Per-relay learned exact singleton-body ceiling. Zero means the empirical
# initial value above. It only moves downward after a real 413.
s = replace_once(
    s,
    "\tfragmentSeq atomic.Uint64\n\tsendMu      sync.Mutex\n",
    "\tfragmentSeq            atomic.Uint64\n"
    "\tadaptiveSingleBodyLimit atomic.Uint64\n"
    "\tsendMu                 sync.Mutex\n",
    "adaptive relay field",
)

# Add helper methods and replace Send. Ordinary packets are copied directly to
# packetQueue. Only >6000-byte packets are fragmented proactively; this matches
# the observed packet-size classes (5845 vs 7293+) without rewriting TCP/IP.
start = s.index("func (r *relayClient) Send(data []byte) error {")
end = s.index("func (r *relayClient) batcher() {")
send_block = r'''var (
	errVolgaAdaptiveLocalSplit = fmt.Errorf("adaptive local split queued")
	errVolgaAdaptive413Retry   = fmt.Errorf("adaptive 413 retry queued")
)

func (r *relayClient) currentAdaptiveSingleBodyLimit() uint64 {
	v := r.adaptiveSingleBodyLimit.Load()
	if v == 0 {
		return volgaAdaptiveSingleHTTPBodyLimit
	}
	return v
}

func (r *relayClient) lowerAdaptiveSingleBodyLimit(bodyBytes uint64) {
	if bodyBytes <= 1 {
		return
	}
	next := bodyBytes - 1
	for {
		stored := r.adaptiveSingleBodyLimit.Load()
		current := stored
		if current == 0 {
			current = volgaAdaptiveSingleHTTPBodyLimit
		}
		if next >= current {
			return
		}
		if r.adaptiveSingleBodyLimit.CompareAndSwap(stored, next) {
			return
		}
	}
}

func (r *relayClient) enqueueRecords(records [][]byte) error {
	if len(records) == 0 {
		return nil
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

func (r *relayClient) queueFragments(data []byte) error {
	fragmentID := uint32(r.fragmentSeq.Add(1))
	records, err := makeVolgaFragments(data, r.config.HTTPBodyLimit, fragmentID)
	if err != nil {
		return err
	}
	r.stats.FragmentedPackets.Add(1)
	r.stats.FragmentsSent.Add(uint64(len(records)))
	return r.enqueueRecords(records)
}

func (r *relayClient) Send(data []byte) error {
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

	if len(data) > volgaAdaptiveRawFragmentAbove {
		return r.queueFragments(data)
	}

	// Fast path: preserve the exact compressed transport record. In V1 every
	// ordinary record gained a five-byte OFX1 header, which moved 5845-byte
	// records across the Yandex 413 boundary.
	cp := append([]byte(nil), data...)
	return r.enqueueRecords([][]byte{cp})
}

'''
s = s[:start] + send_block + s[end:]

# Local exact-body policy:
# - multi-packet batches and fragment records must stay under configured 8000;
# - a raw singleton may use the proven narrow envelope up to 8224;
# - above that it is split locally and requeued, never sent oversized.
old_guard = (
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\tif r.config.HTTPBodyLimit > 0 && len(bodyCopy) > r.config.HTTPBodyLimit {\n"
    "\t\tr.stats.BodyGuardTrips.Add(1)\n"
    "\t\treturn fmt.Errorf(\"local Volga body guard: %d > %d\", len(bodyCopy), r.config.HTTPBodyLimit)\n"
    "\t}\n"
    "\trawBytes := uint64(totalBytes)\n"
)
new_guard = (
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\tsingleRaw := len(batch) == 1 && !isVolgaFragmentRecord(batch[0])\n"
    "\tif singleRaw {\n"
    "\t\tif bodyBytes > r.currentAdaptiveSingleBodyLimit() {\n"
    "\t\t\tr.stats.AdaptiveLocalSplits.Add(1)\n"
    "\t\t\tif err := r.queueFragments(batch[0]); err != nil {\n"
    "\t\t\t\treturn fmt.Errorf(\"adaptive local split: %w\", err)\n"
    "\t\t\t}\n"
    "\t\t\treturn errVolgaAdaptiveLocalSplit\n"
    "\t\t}\n"
    "\t} else if r.config.HTTPBodyLimit > 0 && len(bodyCopy) > r.config.HTTPBodyLimit {\n"
    "\t\tr.stats.BodyGuardTrips.Add(1)\n"
    "\t\treturn fmt.Errorf(\"local Volga body guard: %d > %d\", len(bodyCopy), r.config.HTTPBodyLimit)\n"
    "\t}\n"
    "\trawBytes := uint64(totalBytes)\n"
)
s = replace_once(s, old_guard, new_guard, "adaptive exact body policy")

# A raw singleton that still gets 413 is not dropped. Lower the exact learned
# body limit, fragment that same record, and requeue it. Fragment-record or
# multi-record 413s remain hard errors because they indicate a different bug.
old_413 = (
    "\tif resp.StatusCode == http.StatusRequestEntityTooLarge {\n"
    "\t\tr.stats.HTTP413s.Add(1)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Bytes, bodyBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413RawBytes, rawBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Packets, packetCount)\n"
    "\t\tif packetCount == 1 {\n"
    "\t\t\tr.stats.HTTP413SinglePackets.Add(1)\n"
    "\t\t\tatomicMinNonZero(&r.stats.MinHTTP413SinglePacketBytes, largestPacketBytes)\n"
    "\t\t\tatomicMax(&r.stats.MaxHTTP413SinglePacketBytes, largestPacketBytes)\n"
    "\t\t}\n"
    "\t\treturn fmt.Errorf(\"status 413\")\n"
    "\t}\n"
)
new_413 = (
    "\tif resp.StatusCode == http.StatusRequestEntityTooLarge {\n"
    "\t\tr.stats.HTTP413s.Add(1)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Bytes, bodyBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413RawBytes, rawBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Packets, packetCount)\n"
    "\t\tif packetCount == 1 {\n"
    "\t\t\tr.stats.HTTP413SinglePackets.Add(1)\n"
    "\t\t\tatomicMinNonZero(&r.stats.MinHTTP413SinglePacketBytes, largestPacketBytes)\n"
    "\t\t\tatomicMax(&r.stats.MaxHTTP413SinglePacketBytes, largestPacketBytes)\n"
    "\t\t\tif !isVolgaFragmentRecord(batch[0]) {\n"
    "\t\t\t\tr.lowerAdaptiveSingleBodyLimit(bodyBytes)\n"
    "\t\t\t\tr.stats.Adaptive413Retries.Add(1)\n"
    "\t\t\t\tif err := r.queueFragments(batch[0]); err != nil {\n"
    "\t\t\t\t\treturn fmt.Errorf(\"status 413; adaptive retry queue failed: %w\", err)\n"
    "\t\t\t\t}\n"
    "\t\t\t\treturn errVolgaAdaptive413Retry\n"
    "\t\t\t}\n"
    "\t\t}\n"
    "\t\treturn fmt.Errorf(\"status 413\")\n"
    "\t}\n"
)
s = replace_once(s, old_413, new_413, "adaptive 413 retry")

# Do not count a local pre-send split as an HTTP failure. A real 413 retry is
# still counted as one failed HTTP request, but the packet itself is preserved.
old_worker = (
    "\t\t\terr := r.sendBatch(batch)\n"
    "\t\t\tif err != nil {\n"
    "\t\t\t\tr.stats.HTTPReqsFailed.Add(1)\n"
    "\t\t\t\tutils.Debugf(\"[VOLGA] batch send failed: %v\", err)\n"
    "\t\t\t} else {\n"
    "\t\t\t\tr.stats.HTTPReqsSent.Add(1)\n"
    "\t\t\t\tr.stats.BatchesSent.Add(1)\n"
    "\t\t\t}\n"
)
new_worker = (
    "\t\t\terr := r.sendBatch(batch)\n"
    "\t\t\tswitch err {\n"
    "\t\t\tcase nil:\n"
    "\t\t\t\tr.stats.HTTPReqsSent.Add(1)\n"
    "\t\t\t\tr.stats.BatchesSent.Add(1)\n"
    "\t\t\tcase errVolgaAdaptiveLocalSplit:\n"
    "\t\t\t\t// No HTTP request was made; replacement fragments are queued.\n"
    "\t\t\tcase errVolgaAdaptive413Retry:\n"
    "\t\t\t\tr.stats.HTTPReqsFailed.Add(1)\n"
    "\t\t\tdefault:\n"
    "\t\t\t\tr.stats.HTTPReqsFailed.Add(1)\n"
    "\t\t\t\tutils.Debugf(\"[VOLGA] batch send failed: %v\", err)\n"
    "\t\t\t}\n"
)
s = replace_once(s, old_worker, new_worker, "adaptive worker result handling")

# Extend V1 fragment telemetry with the adaptive decisions and the currently
# learned exact singleton body ceiling.
old_frag_log = (
    "\t\t\tutils.Debugf(\"[VOLGA-FRAG] fragmented=%d fragments-tx=%d fragments-rx=%d reassembled=%d drops=%d body-guard=%d limit=%d\",\n"
    "\t\t\t\tt.stats.FragmentedPackets.Load(), t.stats.FragmentsSent.Load(), t.stats.FragmentsRecv.Load(),\n"
    "\t\t\t\tt.stats.ReassembledPackets.Load(), t.stats.ReassemblyDrops.Load(), t.stats.BodyGuardTrips.Load(), t.config.HTTPBodyLimit)\n\n"
)
new_frag_log = (
    "\t\t\tutils.Debugf(\"[VOLGA-FRAG] fragmented=%d fragments-tx=%d fragments-rx=%d reassembled=%d drops=%d body-guard=%d local-split=%d retry413=%d batch-limit=%d single-limit=%d\",\n"
    "\t\t\t\tt.stats.FragmentedPackets.Load(), t.stats.FragmentsSent.Load(), t.stats.FragmentsRecv.Load(),\n"
    "\t\t\t\tt.stats.ReassembledPackets.Load(), t.stats.ReassemblyDrops.Load(), t.stats.BodyGuardTrips.Load(),\n"
    "\t\t\t\tt.stats.AdaptiveLocalSplits.Load(), t.stats.Adaptive413Retries.Load(), t.config.HTTPBodyLimit, t.relay.currentAdaptiveSingleBodyLimit())\n\n"
)
s = replace_once(s, old_frag_log, new_frag_log, "adaptive fragment telemetry")

p.write_text(s)

# Replace V1-generated tests with V2 policy tests.
test = r'''package yandex

import (
	"bytes"
	"testing"
	"time"
)

func TestVolgaRawFastPathRoundTrip(t *testing.T) {
	in := bytes.Repeat([]byte{0x5a}, 5845)
	if isVolgaFragmentRecord(in) {
		t.Fatal("raw fast-path payload misclassified as fragment")
	}
	r := newVolgaFragmentReassembler(time.Second, 5_000_000, &VolgaStats{})
	out := r.accept(in)
	if len(out) != 1 || !bytes.Equal(out[0], in) {
		t.Fatal("raw fast-path round-trip mismatch")
	}
}

func TestVolgaFragmentRoundTripOutOfOrder(t *testing.T) {
	in := make([]byte, 10189)
	for i := range in {
		in[i] = byte(i * 31)
	}
	records, err := makeVolgaFragments(in, 8000, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 2 {
		t.Fatalf("expected fragmentation, records=%d", len(records))
	}
	for i, rec := range records {
		if !isVolgaFragmentRecord(rec) {
			t.Fatalf("record %d is not marked as fragment", i)
		}
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

func TestVolgaObservedPacketClasses(t *testing.T) {
	if 5845 > volgaAdaptiveRawFragmentAbove {
		t.Fatal("5845-byte proven fast-path packet would be fragmented proactively")
	}
	if 7293 <= volgaAdaptiveRawFragmentAbove {
		t.Fatal("7293-byte known-413 packet would not be fragmented proactively")
	}
}

func TestVolgaBodyEstimateRejectsTwoLargeRecords(t *testing.T) {
	if got := estimateVolgaHTTPBody(2949+2949, 2); got <= 8000 {
		t.Fatalf("expected two 2949-byte records to exceed batch body budget, got %d", got)
	}
}
'''
Path("transport/yandex/volga_fragment_test.go").write_text(test)

print("Volga adaptive fragmentation v2 transform applied")
