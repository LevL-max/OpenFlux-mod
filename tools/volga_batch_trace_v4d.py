#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER volga_ordered_batch_v4c.py.
#
# V4d is diagnostic only. It does not change sender retry, ordering policy,
# batching, workers, HTTP shards, compression, fragmentation, TCP buffers,
# auth/reconnect or router behaviour. It adds exact logical-batch lifecycle
# telemetry so a receive-side sequence hole can be correlated with the EXIT
# relay POST result for that same batchSeq.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

# Diagnostic build only. Debug output is still gated by the existing --debug
# flag through utils.Debugf.
marker = "type volgaOrderedBatch struct {\n"
if marker not in s:
    raise SystemExit("V4d ordered batch marker not found")
s = s.replace(marker, "const volgaForensicBatchTrace = true\n\n" + marker, 1)

# Record creation of the central logical batch before HTTP worker fan-out.
s = replace_once(
    s,
    "\t\tnextBatchSeq++\n"
    "\t\tready := volgaOrderedBatch{seq: nextBatchSeq, packets: batch}\n"
    "\t\tbatch = make([][]byte, 0, batchCap)\n",
    "\t\tnextBatchSeq++\n"
    "\t\tready := volgaOrderedBatch{seq: nextBatchSeq, packets: batch}\n"
    "\t\tif volgaForensicBatchTrace {\n"
    "\t\t\tutils.Debugf(\"[VOLGA-TRACE-TX] created seq=%d pkts=%d\", nextBatchSeq, len(batch))\n"
    "\t\t}\n"
    "\t\tbatch = make([][]byte, 0, batchCap)\n",
    "V4d batch creation trace",
)

# Exact relay request result for every logical sequence. This is the decisive
# correlation point: for a Mini gap at N we can see whether EXIT got 200/204
# for that same N, got an HTTP/network error, or never reached HTTP at all.
s = replace_once(
    s,
    "\tshard := int((r.httpShardSeq.Add(1) - 1) % uint64(len(r.httpClients)))\n"
    "\thttpStart := time.Now()\n"
    "\tresp, err := r.httpClients[shard].Do(req)\n"
    "\telapsedMicros := uint64(time.Since(httpStart).Microseconds())\n"
    "\tif elapsedMicros == 0 {\n"
    "\t\telapsedMicros = 1\n"
    "\t}\n"
    "\tr.stats.HTTPAttempts.Add(1)\n"
    "\tr.stats.HTTPDurationMicros.Add(elapsedMicros)\n"
    "\tatomicMax(&r.stats.HTTPMaxDurationMicros, elapsedMicros)\n"
    "\tif err != nil {\n"
    "\t\treturn err\n"
    "\t}\n",
    "\tshard := int((r.httpShardSeq.Add(1) - 1) % uint64(len(r.httpClients)))\n"
    "\tif volgaForensicBatchTrace {\n"
    "\t\tutils.Debugf(\"[VOLGA-TRACE-TX] http-start seq=%d shard=%d body=%d pkts=%d\", batchSeq, shard, bodyBytes, packetCount)\n"
    "\t}\n"
    "\thttpStart := time.Now()\n"
    "\tresp, err := r.httpClients[shard].Do(req)\n"
    "\telapsedMicros := uint64(time.Since(httpStart).Microseconds())\n"
    "\tif elapsedMicros == 0 {\n"
    "\t\telapsedMicros = 1\n"
    "\t}\n"
    "\tr.stats.HTTPAttempts.Add(1)\n"
    "\tr.stats.HTTPDurationMicros.Add(elapsedMicros)\n"
    "\tatomicMax(&r.stats.HTTPMaxDurationMicros, elapsedMicros)\n"
    "\tif err != nil {\n"
    "\t\tif volgaForensicBatchTrace {\n"
    "\t\t\tutils.Debugf(\"[VOLGA-TRACE-TX] http-error seq=%d shard=%d dur_us=%d err=%v\", batchSeq, shard, elapsedMicros, err)\n"
    "\t\t}\n"
    "\t\treturn err\n"
    "\t}\n"
    "\tif volgaForensicBatchTrace {\n"
    "\t\tutils.Debugf(\"[VOLGA-TRACE-TX] http-result seq=%d shard=%d status=%d dur_us=%d\", batchSeq, shard, resp.StatusCode, elapsedMicros)\n"
    "\t}\n",
    "V4d HTTP lifecycle trace",
)

# Receive-side gap opening, natural fill, and late-after-skip arrival.
s = replace_once(
    s,
    "\tif seq < w.orderNextSeq {\n"
    "\t\tw.stats.OrderDuplicates.Add(1)\n"
    "\t\tw.orderMu.Unlock()\n"
    "\t\treturn\n"
    "\t}\n",
    "\tif seq < w.orderNextSeq {\n"
    "\t\tif volgaForensicBatchTrace {\n"
    "\t\t\tutils.Debugf(\"[VOLGA-TRACE-RX] late seq=%d next=%d\", seq, w.orderNextSeq)\n"
    "\t\t}\n"
    "\t\tw.stats.OrderDuplicates.Add(1)\n"
    "\t\tw.orderMu.Unlock()\n"
    "\t\treturn\n"
    "\t}\n",
    "V4d late arrival trace",
)

s = replace_once(
    s,
    "\tif seq > w.orderNextSeq {\n"
    "\t\tw.stats.OrderBuffered.Add(1)\n"
    "\t\tif w.orderGapSince.IsZero() {\n"
    "\t\t\tw.orderGapSince = now\n"
    "\t\t}\n"
    "\t}\n"
    "\tw.orderPending[seq] = volgaPendingBundle{items: cloneVolgaRawItems(items), arrived: now}\n",
    "\tif seq == w.orderNextSeq && !w.orderGapSince.IsZero() && volgaForensicBatchTrace {\n"
    "\t\tutils.Debugf(\"[VOLGA-TRACE-RX] gap-fill seq=%d wait_us=%d pending=%d\", seq, now.Sub(w.orderGapSince).Microseconds(), len(w.orderPending))\n"
    "\t}\n"
    "\tif seq > w.orderNextSeq {\n"
    "\t\tw.stats.OrderBuffered.Add(1)\n"
    "\t\tif w.orderGapSince.IsZero() {\n"
    "\t\t\tif volgaForensicBatchTrace {\n"
    "\t\t\t\tutils.Debugf(\"[VOLGA-TRACE-RX] gap-open expected=%d seen=%d pending=%d\", w.orderNextSeq, seq, len(w.orderPending))\n"
    "\t\t\t}\n"
    "\t\t\tw.orderGapSince = now\n"
    "\t\t}\n"
    "\t}\n"
    "\tw.orderPending[seq] = volgaPendingBundle{items: cloneVolgaRawItems(items), arrived: now}\n",
    "V4d gap open/fill trace",
)

# Emit the exact skipped logical range and whether timeout or pending-cap forced
# the skip. Existing V4c behaviour is preserved exactly after this log line.
s = replace_once(
    s,
    "\tif minSeq == 0 || minSeq <= w.orderNextSeq {\n"
    "\t\treturn nil\n"
    "\t}\n"
    "\tw.stats.OrderGapSkips.Add(minSeq - w.orderNextSeq)\n"
    "\tw.orderNextSeq = minSeq\n"
    "\tw.orderGapSince = time.Time{}\n"
    "\treturn w.collectReadyLocked(now)\n",
    "\tif minSeq == 0 || minSeq <= w.orderNextSeq {\n"
    "\t\treturn nil\n"
    "\t}\n"
    "\tif volgaForensicBatchTrace {\n"
    "\t\treason := \"timeout\"\n"
    "\t\tif len(w.orderPending) >= volgaOrderMaxPending {\n"
    "\t\t\treason = \"pending-cap\"\n"
    "\t\t}\n"
    "\t\tutils.Debugf(\"[VOLGA-TRACE-RX] gap-skip from=%d to=%d missing=%d wait_us=%d pending=%d reason=%s\",\n"
    "\t\t\tw.orderNextSeq, minSeq, minSeq-w.orderNextSeq, now.Sub(w.orderGapSince).Microseconds(), len(w.orderPending), reason)\n"
    "\t}\n"
    "\tw.stats.OrderGapSkips.Add(minSeq - w.orderNextSeq)\n"
    "\tw.orderNextSeq = minSeq\n"
    "\tw.orderGapSince = time.Time{}\n"
    "\treturn w.collectReadyLocked(now)\n",
    "V4d gap skip trace",
)

p.write_text(s)
print("Volga V4d forensic batch trace transform applied")
