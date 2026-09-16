#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


def replace_func(text: str, start_marker: str, end_marker: str, new_block: str, label: str) -> str:
    start = text.find(start_marker)
    if start < 0:
        raise SystemExit(f"{label}: start marker not found")
    end = text.find(end_marker, start)
    if end < 0:
        raise SystemExit(f"{label}: end marker not found")
    return text[:start] + new_block + text[end:]


# Applied AFTER volga_ordered_batch_v4b.py.
#
# V4c is a deliberately narrow receive-ordering hardening experiment:
#   * use sender-controlled bundleId as the primary stable batch sequence;
#   * retain textInsert op-id parsing as fallback / cross-check;
#   * never pass an unsequenced data-bearing bundle directly to inner TCP;
#   * initialise receive order at the first sequence observed for a peer;
#   * extend the bounded gap wait to 10s and the safety pending cap to 32768;
#   * add telemetry for sequence source, mismatches, unparsed data, and resets.
#
# Deliberately unchanged: sender retry behaviour, workers, H1/H2, shards,
# batching, compression, fragmentation format, TCP buffers, auth/reconnect,
# packet normalisation, and all production/router logic.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

# ---------------------------------------------------------------------------
# Extra ordering telemetry.
# ---------------------------------------------------------------------------
s = replace_once(
    s,
    "\tOrderMaxWaitMicros          atomic.Uint64\n}",
    "\tOrderMaxWaitMicros          atomic.Uint64\n"
    "\tOrderSeqBundleID            atomic.Uint64\n"
    "\tOrderSeqOpID                atomic.Uint64\n"
    "\tOrderSeqMismatch            atomic.Uint64\n"
    "\tOrderUnparsedData           atomic.Uint64\n"
    "\tOrderResets                 atomic.Uint64\n}",
    "V4c ordering telemetry fields",
)

# ---------------------------------------------------------------------------
# V4b's 100ms / 512 policy can skip a merely-late request long before the
# already-observed multi-second HTTP tail has elapsed. V4c waits 10 seconds and
# raises the safety cap so the count cap does not normally fire first.
# ---------------------------------------------------------------------------
s = replace_once(
    s,
    "\tvolgaOrderGapWait    = 100 * time.Millisecond\n\tvolgaOrderMaxPending = 512\n",
    "\tvolgaOrderGapWait    = 10 * time.Second\n\tvolgaOrderMaxPending = 32768\n",
    "V4c reorder bounds",
)

# ---------------------------------------------------------------------------
# bundleId parser and data-bearing detector. The sender already writes the
# central batchSeq into message.bundleId; V4b receiver ignored it.
# ---------------------------------------------------------------------------
helper_marker = "func volgaBundleBatchSeq(items []json.RawMessage) (uint64, bool) {\n"
helpers = r'''func parseVolgaBundleID(raw json.RawMessage) (uint64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		return n, true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil && str != "" {
		n, err := strconv.ParseUint(str, 10, 64)
		if err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func volgaBundleHasData(items []json.RawMessage) bool {
	for _, raw := range items {
		var payload string
		if err := json.Unmarshal(raw, &payload); err == nil && payload != "" {
			return true
		}
	}
	return false
}

'''.replace('\\t', '\t')
if helper_marker not in s:
    raise SystemExit("V4c helper insertion marker not found")
s = s.replace(helper_marker, helpers + helper_marker, 1)

# ---------------------------------------------------------------------------
# Relay messages expose bundleId. Exchange messages may not, so they continue
# through the op-id fallback wrapper.
# ---------------------------------------------------------------------------
handlers_start = "func (w *wsListener) handleRelayMessage(userID int, raw json.RawMessage) {\n"
deliver_marker = "func (w *wsListener) deliverOrderedBundle(items []json.RawMessage) {\n"
new_handlers = r'''func (w *wsListener) handleRelayMessage(userID int, raw json.RawMessage) {
	var relay struct {
		BundleID json.RawMessage   `json:"bundleId"`
		Bundle   []json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(raw, &relay); err != nil {
		return
	}
	bundleSeq, bundleSeqOK := parseVolgaBundleID(relay.BundleID)
	w.handleOrderedBundleWithSeq(userID, bundleSeq, bundleSeqOK, relay.Bundle)
}

func (w *wsListener) handleBundle(userID int, raw json.RawMessage) {
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err == nil {
		w.handleOrderedBundle(userID, asArray)
		return
	}

	var asObject struct {
		BundleID json.RawMessage   `json:"bundleId"`
		Value    []json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		bundleSeq, bundleSeqOK := parseVolgaBundleID(asObject.BundleID)
		w.handleOrderedBundleWithSeq(userID, bundleSeq, bundleSeqOK, asObject.Value)
	}
}

'''.replace('\\t', '\t')
s = replace_func(s, handlers_start, deliver_marker, new_handlers, "V4c relay/exchange handlers")

# ---------------------------------------------------------------------------
# Keep the V4b reset function for test/source compatibility, but add a reset-at
# variant used by live receive so a fresh listener starts at the first observed
# peer sequence rather than manufacturing a gap from sequence 1.
# ---------------------------------------------------------------------------
reset_start = "func (w *wsListener) resetOrderLocked(userID int) {\n"
collect_marker = "func (w *wsListener) collectReadyLocked(now time.Time) [][]json.RawMessage {\n"
new_reset = r'''func (w *wsListener) resetOrderLocked(userID int) {
	w.resetOrderLockedAt(userID, 1)
}

func (w *wsListener) resetOrderLockedAt(userID int, firstSeq uint64) {
	if firstSeq == 0 {
		firstSeq = 1
	}
	w.orderUserID = userID
	w.orderNextSeq = firstSeq
	w.orderPending = make(map[uint64]volgaPendingBundle)
	w.orderGapSince = time.Time{}
	w.stats.OrderResets.Add(1)
}

'''.replace('\\t', '\t')
s = replace_func(s, reset_start, collect_marker, new_reset, "V4c first-sequence reset")

# ---------------------------------------------------------------------------
# Primary sequence selection:
#   1) bundleId written by our central batcher,
#   2) textInsert op-id fallback,
#   3) metadata-only bundles may pass; unsequenced data bundles are withheld.
# ---------------------------------------------------------------------------
ordered_start = "func (w *wsListener) handleOrderedBundle(userID int, items []json.RawMessage) {\n"
state_marker = "func (w *wsListener) orderState() (pending int, next uint64) {\n"
new_ordered = r'''func (w *wsListener) handleOrderedBundle(userID int, items []json.RawMessage) {
	w.handleOrderedBundleWithSeq(userID, 0, false, items)
}

func (w *wsListener) handleOrderedBundleWithSeq(userID int, bundleSeq uint64, bundleSeqOK bool, items []json.RawMessage) {
	opSeq, opSeqOK := volgaBundleBatchSeq(items)

	var seq uint64
	switch {
	case bundleSeqOK:
		seq = bundleSeq
		w.stats.OrderSeqBundleID.Add(1)
		if opSeqOK && opSeq != bundleSeq {
			w.stats.OrderSeqMismatch.Add(1)
		}
	case opSeqOK:
		seq = opSeq
		w.stats.OrderSeqOpID.Add(1)
	default:
		if volgaBundleHasData(items) {
			w.stats.OrderUnparsedData.Add(1)
			return
		}
		w.deliverOrderedBundle(items)
		return
	}

	now := time.Now()
	w.orderMu.Lock()
	if w.orderUserID != userID || w.orderNextSeq == 0 {
		w.resetOrderLockedAt(userID, seq)
	}

	if seq < w.orderNextSeq {
		w.stats.OrderDuplicates.Add(1)
		w.orderMu.Unlock()
		return
	}
	if _, exists := w.orderPending[seq]; exists {
		w.stats.OrderDuplicates.Add(1)
		w.orderMu.Unlock()
		return
	}

	if seq > w.orderNextSeq {
		w.stats.OrderBuffered.Add(1)
		if w.orderGapSince.IsZero() {
			w.orderGapSince = now
		}
	}
	w.orderPending[seq] = volgaPendingBundle{items: cloneVolgaRawItems(items), arrived: now}
	atomicMax(&w.stats.OrderMaxDepth, uint64(len(w.orderPending)))

	ready := w.collectReadyLocked(now)
	ready = append(ready, w.skipExpiredGapLocked(now)...)
	w.orderMu.Unlock()

	for _, bundle := range ready {
		w.deliverOrderedBundle(bundle)
	}
}

'''.replace('\\t', '\t')
s = replace_func(s, ordered_start, state_marker, new_ordered, "V4c sequence selection")

# ---------------------------------------------------------------------------
# Extend ordering telemetry line without changing the existing V4b counters.
# ---------------------------------------------------------------------------
old_log = (
    "\t\t\tutils.Debugf(\"[VOLGA-ORDER] pre-split=%d buffered=%d gap-skip=%d dup=%d max-depth=%d max-wait=%dus pending=%d next=%d\",\n"
    "\t\t\t\tt.stats.OrderPreSplits.Load(), t.stats.OrderBuffered.Load(), t.stats.OrderGapSkips.Load(),\n"
    "\t\t\t\tt.stats.OrderDuplicates.Load(), t.stats.OrderMaxDepth.Load(), t.stats.OrderMaxWaitMicros.Load(), pending, nextSeq)\n"
)
new_log = (
    "\t\t\tutils.Debugf(\"[VOLGA-ORDER] pre-split=%d buffered=%d gap-skip=%d dup=%d max-depth=%d max-wait=%dus pending=%d next=%d seq-bid=%d seq-op=%d mismatch=%d unparsed-data=%d resets=%d\",\n"
    "\t\t\t\tt.stats.OrderPreSplits.Load(), t.stats.OrderBuffered.Load(), t.stats.OrderGapSkips.Load(),\n"
    "\t\t\t\tt.stats.OrderDuplicates.Load(), t.stats.OrderMaxDepth.Load(), t.stats.OrderMaxWaitMicros.Load(), pending, nextSeq,\n"
    "\t\t\t\tt.stats.OrderSeqBundleID.Load(), t.stats.OrderSeqOpID.Load(), t.stats.OrderSeqMismatch.Load(),\n"
    "\t\t\t\tt.stats.OrderUnparsedData.Load(), t.stats.OrderResets.Load())\n"
)
s = replace_once(s, old_log, new_log, "V4c telemetry log")

p.write_text(s)

# Focused V4c tests reuse makeOrderedTestBundle from the V4b generated test.
Path("transport/yandex/volga_ordered_batch_v4c_test.go").write_text(r'''package yandex

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestVolgaV4cBundleIDParsing(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want uint64
		ok   bool
	}{
		{`17`, 17, true},
		{`"23"`, 23, true},
		{`0`, 0, false},
		{`null`, 0, false},
	} {
		got, ok := parseVolgaBundleID(json.RawMessage(tc.raw))
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseVolgaBundleID(%s)=%d,%v want %d,%v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestVolgaV4cFirstObservedSequenceBecomesBaseline(t *testing.T) {
	stats := &VolgaStats{}
	var got []string
	w := newWSListener(
		&volgaAuth{UserID: 42},
		DefaultVolgaConfig(),
		stats,
		&relayClient{stats: stats},
		func(pkt []byte) { got = append(got, string(pkt)) },
	)

	w.handleOrderedBundleWithSeq(99, 10, true, makeOrderedTestBundle(t, 99, 10, []byte("ten")))
	if !reflect.DeepEqual(got, []string{"ten"}) {
		t.Fatalf("delivery=%v want [ten]", got)
	}
	pending, next := w.orderState()
	if pending != 0 || next != 11 {
		t.Fatalf("state pending=%d next=%d want 0,11", pending, next)
	}
	if stats.OrderSeqBundleID.Load() != 1 || stats.OrderResets.Load() != 1 {
		t.Fatalf("seq-bid=%d resets=%d want 1,1", stats.OrderSeqBundleID.Load(), stats.OrderResets.Load())
	}
}

func TestVolgaV4cBundleIDPrimaryAndMismatchTelemetry(t *testing.T) {
	stats := &VolgaStats{}
	var got []string
	w := newWSListener(
		&volgaAuth{UserID: 42},
		DefaultVolgaConfig(),
		stats,
		&relayClient{stats: stats},
		func(pkt []byte) { got = append(got, string(pkt)) },
	)

	// bundleId=10 is primary even though the embedded op-id claims batch 12.
	w.handleOrderedBundleWithSeq(99, 10, true, makeOrderedTestBundle(t, 99, 12, []byte("payload")))
	if !reflect.DeepEqual(got, []string{"payload"}) {
		t.Fatalf("delivery=%v want [payload]", got)
	}
	if stats.OrderSeqMismatch.Load() != 1 {
		t.Fatalf("mismatch=%d want 1", stats.OrderSeqMismatch.Load())
	}
}

func TestVolgaV4cUnparsedDataIsNotDeliveredUnordered(t *testing.T) {
	stats := &VolgaStats{}
	called := false
	w := newWSListener(
		&volgaAuth{UserID: 42},
		DefaultVolgaConfig(),
		stats,
		&relayClient{stats: stats},
		func(pkt []byte) { called = true },
	)

	items := []json.RawMessage{json.RawMessage(`"AQID"`)}
	w.handleOrderedBundle(99, items)
	if called {
		t.Fatal("unsequenced data-bearing bundle was delivered")
	}
	if stats.OrderUnparsedData.Load() != 1 {
		t.Fatalf("unparsed-data=%d want 1", stats.OrderUnparsedData.Load())
	}
}
''')
