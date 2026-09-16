#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


def replace_func_region(text: str, start_marker: str, end_marker: str, new_block: str, label: str) -> str:
    start = text.find(start_marker)
    if start < 0:
        raise SystemExit(f"{label}: start marker not found")
    end = text.find(end_marker, start)
    if end < 0:
        raise SystemExit(f"{label}: end marker not found")
    return text[:start] + new_block + text[end:]


# Applied AFTER V4e. V5 turns the Yandex relay into an unreliable carrier and
# adds an OpenFlux-owned reliability layer:
#   * logical DATA session+sequence is carried inside the encoded payload;
#   * receiver delivers new batches immediately (no global HOL reorder gate);
#   * cumulative ACK + SACK ranges report actual end-to-end receipt;
#   * sender keeps a bounded replay window until ACKed;
#   * fast selective retransmit repairs SACK-visible holes;
#   * adaptive RTO repairs tail loss when no later packet exposes the hole;
#   * retransmits keep the same OpenFlux logical seq but use fresh Yandex ids;
#   * duplicate and stale-session data is suppressed receiver-side.
#
# Existing batching, fragmentation, compression, HTTP shards, reconnect/auth,
# TCP buffers and router logic remain unchanged.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

# Reserve room in the central batcher for the V5 DATA metadata record. This
# keeps the existing local body guard valid after adding one small record to
# every logical DATA batch.
s = replace_once(
    s,
    "estimateVolgaHTTPBody(len(data), 1) > int(r.currentAdaptiveSingleBodyLimit())",
    "estimateVolgaHTTPBody(len(data)+volgaReliableDataMetaBytes, 2) > int(r.currentAdaptiveSingleBodyLimit())",
    "V5 singleton pre-split metadata reserve",
)
s = replace_once(
    s,
    "estimateVolgaHTTPBody(candidateBytes, candidateCount) > r.config.HTTPBodyLimit",
    "estimateVolgaHTTPBody(candidateBytes+volgaReliableDataMetaBytes, candidateCount+1) > r.config.HTTPBodyLimit",
    "V5 batch candidate metadata reserve",
)
s = replace_once(
    s,
    "estimateVolgaHTTPBody(totalBytes, len(batch)) >= r.config.HTTPBodyLimit",
    "estimateVolgaHTTPBody(totalBytes+volgaReliableDataMetaBytes, len(batch)+1) >= r.config.HTTPBodyLimit",
    "V5 active batch metadata reserve",
)

# Keep the existing send implementation as the wire POST primitive. The public
# sendBatch wrapper now owns replay lifecycle for logical DATA batches. Control
# ACK frames use logical seq 0 and bypass replay tracking.
s = replace_once(
    s,
    "func (r *relayClient) sendBatch(batchSeq uint64, batch [][]byte) error {\n",
    "func (r *relayClient) sendBatch(batchSeq uint64, batch [][]byte) error {\n"
    "\tif batchSeq == 0 {\n"
    "\t\treturn r.postBatchV5(0, batch)\n"
    "\t}\n"
    "\tif err := r.volgaReliableRemember(batchSeq, batch); err != nil {\n"
    "\t\treturn err\n"
    "\t}\n"
    "\tr.volgaReliableMarkSend(batchSeq, false)\n"
    "\terr := r.postBatchV5(batchSeq, batch)\n"
    "\tif err == errVolgaAdaptiveLocalSplit || err == errVolgaAdaptive413Retry {\n"
    "\t\tr.volgaReliableForget(batchSeq)\n"
    "\t}\n"
    "\treturn err\n"
    "}\n\n"
    "func (r *relayClient) postBatchV5(batchSeq uint64, batch [][]byte) error {\n",
    "V5 sendBatch replay wrapper",
)

old_blob = (
    "\tblob := blobBufPool.Get().(*bytes.Buffer)\n"
    "\tblob.Reset()\n\n"
    "\tvar lenBuf [2]byte\n"
    "\tvar totalBytes int\n"
    "\tfor _, p := range batch {\n"
    "\t\tbinary.BigEndian.PutUint16(lenBuf[:], uint16(len(p)))\n"
    "\t\tblob.Write(lenBuf[:])\n"
    "\t\tblob.Write(p)\n"
    "\t\ttotalBytes += len(p)\n"
    "\t}\n"
)
new_blob = (
    "\tblob := blobBufPool.Get().(*bytes.Buffer)\n"
    "\tblob.Reset()\n\n"
    "\twireBatch := batch\n"
    "\tif batchSeq != 0 {\n"
    "\t\tmeta := makeVolgaReliableDataMeta(r.volgaReliableSession(), batchSeq)\n"
    "\t\twireBatch = make([][]byte, 0, len(batch)+1)\n"
    "\t\twireBatch = append(wireBatch, meta)\n"
    "\t\twireBatch = append(wireBatch, batch...)\n"
    "\t}\n\n"
    "\tvar totalBytes int\n"
    "\tfor _, p := range batch {\n"
    "\t\ttotalBytes += len(p)\n"
    "\t}\n"
    "\tvar lenBuf [2]byte\n"
    "\tfor _, p := range wireBatch {\n"
    "\t\tbinary.BigEndian.PutUint16(lenBuf[:], uint16(len(p)))\n"
    "\t\tblob.Write(lenBuf[:])\n"
    "\t\tblob.Write(p)\n"
    "\t}\n"
)
s = replace_once(s, old_blob, new_blob, "V5 payload metadata record")

# Retransmission must not reuse the Yandex operation identity. OpenFlux logical
# sequence now lives in the V5 payload, while every POST gets fresh op/local/
# bundle identities from atomics.
s = replace_once(
    s,
    "\tif batchSeq == 0 {\n"
    "\t\treturn fmt.Errorf(\"invalid Volga batch sequence 0\")\n"
    "\t}\n"
    "\tfrontier := r.getFrontier()\n"
    "\topSeq := batchSeq*2 - 1\n"
    "\trelaySeq := batchSeq * 2\n"
    "\topID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, opSeq)\n"
    "\trelayOpID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, relaySeq)\n",
    "\tfrontier := r.getFrontier()\n"
    "\twireBase := r.seq.Add(2)\n"
    "\topSeq := wireBase - 1\n"
    "\trelaySeq := wireBase\n"
    "\twireBundleID := r.bundleID.Add(1)\n"
    "\topID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, opSeq)\n"
    "\trelayOpID := fmt.Sprintf(\"1-%d.%d\", r.auth.UserID, relaySeq)\n",
    "V5 fresh Yandex wire identities",
)
s = replace_once(s, '"bundleId": batchSeq,', '"bundleId": wireBundleID,', "V5 fresh wire bundleId")

# Bypass V4b/V4c/V4e global receive ordering in live traffic. V5 dedupe/SACK
# operates on the payload-level logical sequence and deliberately delivers new
# IP records immediately so a single carrier hole cannot head-of-line block
# the tunnel. Old ordering helpers/tests remain compiled for forensic history.
handlers_start = "func (w *wsListener) handleRelayMessage(userID int, raw json.RawMessage) {\n"
deliver_marker = "func (w *wsListener) deliverOrderedBundle(items []json.RawMessage) {\n"
handlers = r'''func (w *wsListener) handleRelayMessage(userID int, raw json.RawMessage) {
	var relay struct {
		Bundle []json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(raw, &relay); err != nil {
		return
	}
	for _, item := range relay.Bundle {
		w.handleBundleItem(item)
	}
}

func (w *wsListener) handleBundle(userID int, raw json.RawMessage) {
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err == nil {
		for _, item := range asArray {
			w.handleBundleItem(item)
		}
		return
	}

	var asObject struct {
		Value []json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		for _, item := range asObject.Value {
			w.handleBundleItem(item)
		}
	}
}

'''.replace('\\t', '\t')
s = replace_func_region(s, handlers_start, deliver_marker, handlers, "V5 live receive routing")

old_decode = r'''		records := decodeBatch(decoded)
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
new_decode = r'''		records := decodeBatch(decoded)
		w.handleVolgaReliableRecords(records, len(decoded))
'''
s = replace_once(s, old_decode, new_decode, "V5 reliable payload decode")

# Add a compact lifecycle line beside the existing V4 telemetry. This is the
# acceptance-test view: replay depth, ACK progress, retransmit cause, adaptive
# RTO and receiver SACK state.
anchor = "\t\t\tlastSent, lastBytes = sent, bytes\n"
reliable_log = (
    "\t\t\tif t.relay != nil && t.ws != nil {\n"
    "\t\t\t\tsession, depth, acked, ackRx, rtx, fast, timeoutRtx, replayMax, rtoMs := t.relay.volgaReliableSnapshot()\n"
    "\t\t\t\tpeer, base, pending, ackTx, dupV5, resetsV5, staleV5, legacyV5 := t.ws.volgaReliableRecvSnapshot()\n"
    "\t\t\t\tutils.Debugf(\"[VOLGA-RELIABLE] tx-session=%d replay=%d max=%d acked=%d ack-rx=%d rtx=%d fast=%d timeout=%d rto=%dms | rx-session=%d base=%d sack-pending=%d ack-tx=%d dup=%d resets=%d stale=%d legacy=%d\",\n"
    "\t\t\t\t\tsession, depth, replayMax, acked, ackRx, rtx, fast, timeoutRtx, rtoMs, peer, base, pending, ackTx, dupV5, resetsV5, staleV5, legacyV5)\n"
    "\t\t\t}\n"
)
s = replace_once(s, anchor, reliable_log + anchor, "V5 reliability telemetry")

p.write_text(s)

Path("transport/yandex/volga_reliable_v5.go").write_text(
    Path("tools/volga_reliable_v5_impl.go.txt").read_text()
)
Path("transport/yandex/volga_reliable_v5_test.go").write_text(
    Path("tools/volga_reliable_v5_test.go.txt").read_text()
)

print("Volga V5 reliable transport transform applied")
