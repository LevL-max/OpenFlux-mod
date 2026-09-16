#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


def insert_before_once(text: str, marker: str, block: str, label: str) -> str:
    count = text.count(marker)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one marker, got {count}")
    return text.replace(marker, block + marker, 1)


# Applied AFTER the existing V1..V6 + benchmark harness + body/adaptive
# fragmentation + HTTP-shards-v3 transforms.
#
# Experiment goal:
#   preserve the central batcher's logical order while keeping many HTTP relay
#   workers in flight. Yandex/WebSocket delivery can complete those relay POSTs
#   out of order, which exposes the inner TCP stream to large packet reordering.
#
# V4b assigns every centrally formed batch a stable sequence before worker
# fan-out, encodes that sequence into the existing Yandex operation/bundle IDs
# (no tunnel-payload header), and reorders complete received bundles before
# passing their records to the existing fragmentation/reassembly path.
#
# A conservative pre-fragment check is added for raw singleton records whose
# estimated final relay body would exceed the adaptive singleton ceiling. This
# moves the already-existing adaptive split before central sequencing so those
# records cannot create permanent sequence holes by being requeued from a
# worker after later batches have already been formed.
#
# Deliberately unchanged: H1/H2 selection, worker counts, HTTP shards, batch
# byte/time limits, compression, TCP buffers, auth/reconnect lifecycle, packet
# normalization and production routing.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

# ---------------------------------------------------------------------------
# Ordering telemetry.
# ---------------------------------------------------------------------------
stats_marker = "type VolgaStats struct {\n"
start = s.index(stats_marker)
end = s.index("}\n", start)
stats_block = s[start:end]
extra = (
    "\tOrderPreSplits              atomic.Uint64\n"
    "\tOrderBuffered               atomic.Uint64\n"
    "\tOrderGapSkips               atomic.Uint64\n"
    "\tOrderDuplicates             atomic.Uint64\n"
    "\tOrderMaxDepth               atomic.Uint64\n"
    "\tOrderMaxWaitMicros          atomic.Uint64\n"
)
if "OrderBuffered" in stats_block:
    raise SystemExit("ordering telemetry already present")
s = s[:end] + extra + s[end:]

# ---------------------------------------------------------------------------
# Move conservative adaptive singleton splitting before the central batcher.
# This preserves the existing fragment format/body limits, but avoids assigning
# an order number to a raw batch that will only be split/requeued later.
# ---------------------------------------------------------------------------
s = replace_once(
    s,
    "\tif len(data) > volgaAdaptiveRawFragmentAbove {\n"
    "\t\treturn r.queueFragments(data)\n"
    "\t}\n\n"
    "\t// Fast path: preserve the exact compressed transport record.",
    "\tif len(data) > volgaAdaptiveRawFragmentAbove {\n"
    "\t\treturn r.queueFragments(data)\n"
    "\t}\n"
    "\tif estimateVolgaHTTPBody(len(data), 1) > int(r.currentAdaptiveSingleBodyLimit()) {\n"
    "\t\tr.stats.OrderPreSplits.Add(1)\n"
    "\t\treturn r.queueFragments(data)\n"
    "\t}\n\n"
    "\t// Fast path: preserve the exact compressed transport record.",
    "pre-batcher adaptive singleton split",
)

# ---------------------------------------------------------------------------
# Stable logical batch sequence before worker fan-out.
# ---------------------------------------------------------------------------
insert_marker = "type relayClient struct {\n"
ordered_type = r'''type volgaOrderedBatch struct {
\tseq     uint64
\tpackets [][]byte
}

'''
s = insert_before_once(s, insert_marker, ordered_type, "ordered batch type")
s = replace_once(
    s,
    "\tbatchQueue  chan [][]byte\n",
    "\tbatchQueue  chan volgaOrderedBatch\n",
    "ordered batch queue field",
)
s = replace_once(
    s,
    "\t\tbatchQueue:  make(chan [][]byte, batchQueueSize),\n",
    "\t\tbatchQueue:  make(chan volgaOrderedBatch, batchQueueSize),\n",
    "ordered batch queue constructor",
)

batcher_start = s.index("func (r *relayClient) batcher() {")
send_start = s.index("func (r *relayClient) sendBatch(", batcher_start)
ordered_workers = r'''func (r *relayClient) batcher() {
\tdefer r.wg.Done()

\tbatchCap := r.config.BatchSize
\tif batchCap < 1 {
\t\tbatchCap = 1
\t}

\tbatch := make([][]byte, 0, batchCap)
\ttotalBytes := 0
\tvar nextBatchSeq uint64
\ttimer := time.NewTimer(r.config.BatchTimeout)
\tif !timer.Stop() {
\t\t<-timer.C
\t}
\tdefer timer.Stop()
\ttimerArmed := false

\tstopTimer := func() {
\t\tif !timerArmed {
\t\t\treturn
\t\t}
\t\tif !timer.Stop() {
\t\t\tselect {
\t\t\tcase <-timer.C:
\t\t\tdefault:
\t\t\t}
\t\t}
\t\ttimerArmed = false
\t}

\tarmTimer := func() {
\t\tstopTimer()
\t\ttimer.Reset(r.config.BatchTimeout)
\t\ttimerArmed = true
\t}

\tflush := func() bool {
\t\tif len(batch) == 0 {
\t\t\tstopTimer()
\t\t\treturn true
\t\t}

\t\tstopTimer()
\t\tnextBatchSeq++
\t\tready := volgaOrderedBatch{seq: nextBatchSeq, packets: batch}
\t\tbatch = make([][]byte, 0, batchCap)
\t\ttotalBytes = 0

\t\tselect {
\t\tcase r.batchQueue <- ready:
\t\t\treturn true
\t\tcase <-r.ctx.Done():
\t\t\treturn false
\t\t}
\t}

\tfor {
\t\tvar timerC <-chan time.Time
\t\tif timerArmed {
\t\t\ttimerC = timer.C
\t\t}

\t\tselect {
\t\tcase <-r.ctx.Done():
\t\t\treturn

\t\tcase pkt := <-r.packetQueue:
\t\t\tcandidateBytes := totalBytes + len(pkt)
\t\t\tcandidateCount := len(batch) + 1
\t\t\toverRaw := r.config.BatchMaxBytes > 0 && candidateBytes > r.config.BatchMaxBytes
\t\t\toverBody := r.config.HTTPBodyLimit > 0 && estimateVolgaHTTPBody(candidateBytes, candidateCount) > r.config.HTTPBodyLimit

\t\t\tif len(batch) > 0 && (overRaw || overBody) {
\t\t\t\tif !flush() {
\t\t\t\t\treturn
\t\t\t\t}
\t\t\t}

\t\t\tbatch = append(batch, pkt)
\t\t\ttotalBytes += len(pkt)

\t\t\tatBodyLimit := r.config.HTTPBodyLimit > 0 && estimateVolgaHTTPBody(totalBytes, len(batch)) >= r.config.HTTPBodyLimit
\t\t\tif len(batch) >= batchCap ||
\t\t\t\t(r.config.BatchMaxBytes > 0 && totalBytes >= r.config.BatchMaxBytes) || atBodyLimit {
\t\t\t\tif !flush() {
\t\t\t\t\treturn
\t\t\t\t}
\t\t\t} else if len(batch) == 1 {
\t\t\t\tarmTimer()
\t\t\t}

\t\tcase <-timerC:
\t\t\ttimerArmed = false
\t\t\tif !flush() {
\t\t\t\treturn
\t\t\t}
\t\t}
\t}
}

func (r *relayClient) worker(id int) {
\tdefer r.wg.Done()

\tfor {
\t\tselect {
\t\tcase <-r.ctx.Done():
\t\t\treturn

\t\tcase ready := <-r.batchQueue:
\t\t\tif len(ready.packets) == 0 {
\t\t\t\tcontinue
\t\t\t}

\t\t\tr.stats.WorkerBusy.Add(1)
\t\t\terr := r.sendBatch(ready.seq, ready.packets)
\t\t\tswitch err {
\t\t\tcase nil:
\t\t\t\tr.stats.HTTPReqsSent.Add(1)
\t\t\t\tr.stats.BatchesSent.Add(1)
\t\t\tcase errVolgaAdaptiveLocalSplit:
\t\t\t\t// V4b should normally prevent this by pre-splitting before the
\t\t\t\t// central batcher. Keep the legacy fallback for safety.
\t\t\tcase errVolgaAdaptive413Retry:
\t\t\t\tr.stats.HTTPReqsFailed.Add(1)
\t\t\tdefault:
\t\t\t\tr.stats.HTTPReqsFailed.Add(1)
\t\t\t\tutils.Debugf("[VOLGA] batch send failed: %v", err)
\t\t\t}
\t\t\tr.stats.WorkerBusy.Add(-1)
\t\t}
\t}
}

'''
s = s[:batcher_start] + ordered_workers + s[send_start:]

s = replace_once(
    s,
    "func (r *relayClient) sendBatch(batch [][]byte) error {",
    "func (r *relayClient) sendBatch(batchSeq uint64, batch [][]byte) error {",
    "sendBatch ordered signature",
)

s = replace_once(
    s,
    "\tfrontier := r.getFrontier()\n"
    "\topID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, r.seq.Add(1))\n"
    "\trelayOpID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, r.seq.Add(1))\n",
    "\tif batchSeq == 0 {\n"
    "\t\treturn fmt.Errorf(\"invalid Volga batch sequence 0\")\n"
    "\t}\n"
    "\tfrontier := r.getFrontier()\n"
    "\topSeq := batchSeq*2 - 1\n"
    "\trelaySeq := batchSeq * 2\n"
    "\topID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, opSeq)\n"
    "\trelayOpID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, relaySeq)\n",
    "ordered operation IDs",
)

# Keep Yandex's local/bundle IDs aligned with the same logical sequence too.
s = replace_once(s, '"localId":    r.localID.Add(1),', '"localId":    opSeq,', "textInsert localId")
s = replace_once(s, '"localId":    r.localID.Add(1),', '"localId":    relaySeq,', "setCaret localId")
s = replace_once(s, '"bundleId": r.bundleID.Add(1),', '"bundleId": batchSeq,', "ordered bundleId")

# ---------------------------------------------------------------------------
# Receive-side bounded reorder buffer. Entire Yandex bundles are held/released
# so the existing decode + fragment reassembly path remains byte-for-byte.
# ---------------------------------------------------------------------------
ws_marker = "type wsListener struct {\n"
ws_helpers = r'''const (
\tvolgaOrderGapWait    = 100 * time.Millisecond
\tvolgaOrderMaxPending = 512
)

type volgaPendingBundle struct {
\titems   []json.RawMessage
\tarrived time.Time
}

func parseVolgaBatchSeq(opID string) (uint64, bool) {
\tdot := strings.LastIndexByte(opID, '.')
\tif dot < 0 || dot+1 >= len(opID) {
\t\treturn 0, false
\t}
\topSeq, err := strconv.ParseUint(opID[dot+1:], 10, 64)
\tif err != nil || opSeq == 0 || opSeq%2 == 0 {
\t\treturn 0, false
\t}
\treturn (opSeq + 1) / 2, true
}

func volgaBundleBatchSeq(items []json.RawMessage) (uint64, bool) {
\tfor _, raw := range items {
\t\tvar obj struct {
\t\t\tID     string `json:"id"`
\t\t\tAction string `json:"actionName"`
\t\t}
\t\tif err := json.Unmarshal(raw, &obj); err == nil && obj.Action == "textInsert" && obj.ID != "" {
\t\t\treturn parseVolgaBatchSeq(obj.ID)
\t\t}
\t}
\treturn 0, false
}

func cloneVolgaRawItems(items []json.RawMessage) []json.RawMessage {
\tout := make([]json.RawMessage, len(items))
\tfor i, item := range items {
\t\tout[i] = append(json.RawMessage(nil), item...)
\t}
\treturn out
}

'''
s = insert_before_once(s, ws_marker, ws_helpers, "receive ordering helpers")

# Add receive-order state to wsListener without disturbing the fragment reassembler.
struct_start = s.index(ws_marker)
struct_end = s.index("}\n", struct_start)
ws_struct = s[struct_start:struct_end]
if "orderPending" in ws_struct:
    raise SystemExit("ws ordering state already present")
order_fields = (
    "\n\torderMu       sync.Mutex\n"
    "\torderUserID   int\n"
    "\torderNextSeq  uint64\n"
    "\torderPending  map[uint64]volgaPendingBundle\n"
    "\torderGapSince time.Time\n"
)
s = s[:struct_end] + order_fields + s[struct_end:]

# Initialize the reorder map in the production constructor.
constructor_marker = "func newWSListener(auth *volgaAuth, cfg VolgaConfig, stats *VolgaStats,"
cs = s.index(constructor_marker)
ce = s.index("}\n", s.index("return &wsListener{", cs))
constructor_region = s[cs:ce]
needle = "\t\tcancel: cancel,\n"
if constructor_region.count(needle) != 1:
    raise SystemExit("ws constructor cancel field missing or duplicated")
constructor_region = constructor_region.replace(
    needle,
    "\t\torderPending: make(map[uint64]volgaPendingBundle),\n" + needle,
    1,
)
s = s[:cs] + constructor_region + s[ce:]

# Pass the remote Yandex user ID to bundle handlers so a peer process restart
# can reset sequence state when its collaboration user/session changes.
s = replace_once(
    s,
    "\tswitch inner.T {\n"
    "\tcase \"relay\":\n"
    "\t\tw.handleRelayMessage(inner.Message)\n"
    "\tcase \"exchange\":\n"
    "\t\tw.handleBundle(inner.Bundle)\n"
    "\t}\n",
    "\tswitch inner.T {\n"
    "\tcase \"relay\":\n"
    "\t\tw.handleRelayMessage(inner.UserID, inner.Message)\n"
    "\tcase \"exchange\":\n"
    "\t\tw.handleBundle(inner.UserID, inner.Bundle)\n"
    "\t}\n",
    "remote user routing",
)

handlers_start = s.index("func (w *wsListener) handleRelayMessage(")
item_start = s.index("func (w *wsListener) handleBundleItem(", handlers_start)
ordered_handlers = r'''func (w *wsListener) handleRelayMessage(userID int, raw json.RawMessage) {
\tvar relay struct {
\t\tBundle []json.RawMessage `json:"bundle"`
\t}
\tif err := json.Unmarshal(raw, &relay); err != nil {
\t\treturn
\t}
\tw.handleOrderedBundle(userID, relay.Bundle)
}

func (w *wsListener) handleBundle(userID int, raw json.RawMessage) {
\tvar asArray []json.RawMessage
\tif err := json.Unmarshal(raw, &asArray); err == nil {
\t\tw.handleOrderedBundle(userID, asArray)
\t\treturn
\t}

\tvar asObject struct {
\t\tValue []json.RawMessage `json:"value"`
\t}
\tif err := json.Unmarshal(raw, &asObject); err == nil {
\t\tw.handleOrderedBundle(userID, asObject.Value)
\t}
}

func (w *wsListener) deliverOrderedBundle(items []json.RawMessage) {
\tfor _, item := range items {
\t\tw.handleBundleItem(item)
\t}
}

func (w *wsListener) resetOrderLocked(userID int) {
\tw.orderUserID = userID
\tw.orderNextSeq = 1
\tw.orderPending = make(map[uint64]volgaPendingBundle)
\tw.orderGapSince = time.Time{}
}

func (w *wsListener) collectReadyLocked(now time.Time) [][]json.RawMessage {
\tvar ready [][]json.RawMessage
\tfor {
\t\tentry, ok := w.orderPending[w.orderNextSeq]
\t\tif !ok {
\t\t\tbreak
\t\t}
\t\tdelete(w.orderPending, w.orderNextSeq)
\t\twait := now.Sub(entry.arrived)
\t\tif wait > 0 {
\t\t\tatomicMax(&w.stats.OrderMaxWaitMicros, uint64(wait/time.Microsecond))
\t\t}
\t\tready = append(ready, entry.items)
\t\tw.orderNextSeq++
\t}
\tif len(w.orderPending) == 0 {
\t\tw.orderGapSince = time.Time{}
\t} else if w.orderGapSince.IsZero() {
\t\tw.orderGapSince = now
\t}
\treturn ready
}

func (w *wsListener) skipExpiredGapLocked(now time.Time) [][]json.RawMessage {
\tif len(w.orderPending) == 0 || w.orderGapSince.IsZero() {
\t\treturn nil
\t}
\tif len(w.orderPending) < volgaOrderMaxPending && now.Sub(w.orderGapSince) < volgaOrderGapWait {
\t\treturn nil
\t}

\tminSeq := uint64(0)
\tfor seq := range w.orderPending {
\t\tif minSeq == 0 || seq < minSeq {
\t\t\tminSeq = seq
\t\t}
\t}
\tif minSeq == 0 || minSeq <= w.orderNextSeq {
\t\treturn nil
\t}
\tw.stats.OrderGapSkips.Add(minSeq - w.orderNextSeq)
\tw.orderNextSeq = minSeq
\tw.orderGapSince = time.Time{}
\treturn w.collectReadyLocked(now)
}

func (w *wsListener) handleOrderedBundle(userID int, items []json.RawMessage) {
\tseq, ok := volgaBundleBatchSeq(items)
\tif !ok {
\t\tw.deliverOrderedBundle(items)
\t\treturn
\t}

\tnow := time.Now()
\tw.orderMu.Lock()
\tif w.orderUserID != userID || w.orderNextSeq == 0 {
\t\tw.resetOrderLocked(userID)
\t}

\tif seq < w.orderNextSeq {
\t\tw.stats.OrderDuplicates.Add(1)
\t\tw.orderMu.Unlock()
\t\treturn
\t}
\tif _, exists := w.orderPending[seq]; exists {
\t\tw.stats.OrderDuplicates.Add(1)
\t\tw.orderMu.Unlock()
\t\treturn
\t}

\tif seq > w.orderNextSeq {
\t\tw.stats.OrderBuffered.Add(1)
\t\tif w.orderGapSince.IsZero() {
\t\t\tw.orderGapSince = now
\t\t}
\t}
\tw.orderPending[seq] = volgaPendingBundle{items: cloneVolgaRawItems(items), arrived: now}
\tatomicMax(&w.stats.OrderMaxDepth, uint64(len(w.orderPending)))

\tready := w.collectReadyLocked(now)
\tready = append(ready, w.skipExpiredGapLocked(now)...)
\tw.orderMu.Unlock()

\tfor _, bundle := range ready {
\t\tw.deliverOrderedBundle(bundle)
\t}
}

func (w *wsListener) orderState() (pending int, next uint64) {
\tw.orderMu.Lock()
\tdefer w.orderMu.Unlock()
\treturn len(w.orderPending), w.orderNextSeq
}

'''
s = s[:handlers_start] + ordered_handlers + s[item_start:]

# Add ordering telemetry as a separate line so the established benchmark lines
# remain directly comparable with V3/V4a logs.
telemetry_anchor = "\t\t\tlastSent, lastBytes = sent, bytes\n"
if s.count(telemetry_anchor) != 1:
    raise SystemExit("stats loop anchor missing or duplicated")
telemetry = (
    "\t\t\tpending, nextSeq := 0, uint64(0)\n"
    "\t\t\tif t.ws != nil {\n"
    "\t\t\t\tpending, nextSeq = t.ws.orderState()\n"
    "\t\t\t}\n"
    "\t\t\tutils.Debugf(\"[VOLGA-ORDER] pre-split=%d buffered=%d gap-skip=%d dup=%d max-depth=%d max-wait=%dus pending=%d next=%d\",\n"
    "\t\t\t\tt.stats.OrderPreSplits.Load(), t.stats.OrderBuffered.Load(), t.stats.OrderGapSkips.Load(),\n"
    "\t\t\t\tt.stats.OrderDuplicates.Load(), t.stats.OrderMaxDepth.Load(), t.stats.OrderMaxWaitMicros.Load(), pending, nextSeq)\n\n"
)
s = s.replace(telemetry_anchor, telemetry + telemetry_anchor, 1)

p.write_text(s)

# ---------------------------------------------------------------------------
# Unit tests generated into the transformed workspace.
# ---------------------------------------------------------------------------
Path("transport/yandex/volga_ordered_batch_v4b_test.go").write_text(r'''package yandex

import (
\t"encoding/base64"
\t"encoding/binary"
\t"encoding/json"
\t"reflect"
\t"testing"
\t"time"
)

func makeOrderedTestBundle(t *testing.T, userID int, seq uint64, packet []byte) []json.RawMessage {
\tt.Helper()
\tframed := make([]byte, 2+len(packet))
\tbinary.BigEndian.PutUint16(framed[:2], uint16(len(packet)))
\tcopy(framed[2:], packet)

\topSeq := seq*2 - 1
\titems := []interface{}{
\t\tmap[string]interface{}{
\t\t\t"id":         "1-" + strconv.Itoa(userID) + "." + strconv.FormatUint(opSeq, 10),
\t\t\t"actionName": "textInsert",
\t\t},
\t\tbase64.StdEncoding.EncodeToString(framed),
\t}
\traw, err := json.Marshal(items)
\tif err != nil {
\t\tt.Fatal(err)
\t}
\tvar out []json.RawMessage
\tif err := json.Unmarshal(raw, &out); err != nil {
\t\tt.Fatal(err)
\t}
\treturn out
}

func TestVolgaBatchSeqParsing(t *testing.T) {
\tif got, ok := parseVolgaBatchSeq("1-42.19"); !ok || got != 10 {
\t\tt.Fatalf("parse = %d,%v want 10,true", got, ok)
\t}
\tif _, ok := parseVolgaBatchSeq("1-42.20"); ok {
\t\tt.Fatal("even setCaret operation sequence must not parse as batch id")
\t}
}

func TestVolgaOrderedBundlesReleaseInSequence(t *testing.T) {
\tstats := &VolgaStats{}
\tvar got []string
\tw := newWSListener(
\t\t&volgaAuth{UserID: 42},
\t\tDefaultVolgaConfig(),
\t\tstats,
\t\t&relayClient{stats: stats},
\t\tfunc(pkt []byte) { got = append(got, string(pkt)) },
\t)

\tw.handleOrderedBundle(99, makeOrderedTestBundle(t, 99, 1, []byte("one")))
\tw.handleOrderedBundle(99, makeOrderedTestBundle(t, 99, 3, []byte("three")))
\tif !reflect.DeepEqual(got, []string{"one"}) {
\t\tt.Fatalf("after 1,3 got=%v", got)
\t}
\tw.handleOrderedBundle(99, makeOrderedTestBundle(t, 99, 2, []byte("two")))
\tif !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
\t\tt.Fatalf("ordered delivery got=%v", got)
\t}
\tpending, next := w.orderState()
\tif pending != 0 || next != 4 {
\t\tt.Fatalf("state pending=%d next=%d want 0,4", pending, next)
\t}
\tif stats.OrderBuffered.Load() != 1 {
\t\tt.Fatalf("buffered=%d want 1", stats.OrderBuffered.Load())
\t}
}

func TestVolgaOrderedBundlesSkipExpiredGap(t *testing.T) {
\tstats := &VolgaStats{}
\tvar got []string
\tw := newWSListener(
\t\t&volgaAuth{UserID: 42},
\t\tDefaultVolgaConfig(),
\t\tstats,
\t\t&relayClient{stats: stats},
\t\tfunc(pkt []byte) { got = append(got, string(pkt)) },
\t)

\tw.handleOrderedBundle(99, makeOrderedTestBundle(t, 99, 1, []byte("one")))
\tw.handleOrderedBundle(99, makeOrderedTestBundle(t, 99, 3, []byte("three")))
\tw.orderMu.Lock()
\tw.orderGapSince = time.Now().Add(-2 * volgaOrderGapWait)
\tw.orderMu.Unlock()
\tw.handleOrderedBundle(99, makeOrderedTestBundle(t, 99, 4, []byte("four")))

\tif !reflect.DeepEqual(got, []string{"one", "three", "four"}) {
\t\tt.Fatalf("gap-skip delivery got=%v", got)
\t}
\tif stats.OrderGapSkips.Load() != 1 {
\t\tt.Fatalf("gap-skip=%d want 1", stats.OrderGapSkips.Load())
\t}
}
''')

# The generated test uses strconv explicitly.
tp = Path("transport/yandex/volga_ordered_batch_v4b_test.go")
ts = tp.read_text()
ts = ts.replace('"reflect"\n', '"reflect"\n\t"strconv"\n', 1)
tp.write_text(ts)

print("Volga ordered-batch v4b transform applied")
