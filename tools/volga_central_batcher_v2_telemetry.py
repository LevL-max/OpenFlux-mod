from pathlib import Path

path = Path("transport/yandex/vyandex.go")
s = path.read_text()


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Add size-envelope telemetry without changing batching behaviour.
s = replace_once(
    s,
    "\tHTTPReqsSent   atomic.Uint64\n\tHTTPReqsFailed atomic.Uint64\n\tWSReconnects   atomic.Uint64\n",
    "\tHTTPReqsSent          atomic.Uint64\n"
    "\tHTTPReqsFailed        atomic.Uint64\n"
    "\tHTTP413s              atomic.Uint64\n"
    "\tMaxHTTPSuccessBytes   atomic.Uint64\n"
    "\tMinHTTP413Bytes       atomic.Uint64\n"
    "\tMaxSuccessRawBytes    atomic.Uint64\n"
    "\tMinHTTP413RawBytes    atomic.Uint64\n"
    "\tMaxSuccessPackets     atomic.Uint64\n"
    "\tMinHTTP413Packets     atomic.Uint64\n"
    "\tWSReconnects          atomic.Uint64\n",
    "VolgaStats telemetry fields",
)

s = replace_once(
    s,
    "\tbodyCopy := make([]byte, buf.Len())\n\tcopy(bodyCopy, buf.Bytes())\n\tjsonBufPool.Put(buf)\n",
    "\tbodyCopy := make([]byte, buf.Len())\n"
    "\tcopy(bodyCopy, buf.Bytes())\n"
    "\tjsonBufPool.Put(buf)\n\n"
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\trawBytes := uint64(totalBytes)\n"
    "\tpacketCount := uint64(len(batch))\n",
    "sendBatch size capture",
)

s = replace_once(
    s,
    "\tif resp.StatusCode != 204 && resp.StatusCode != 200 {\n"
    "\t\treturn fmt.Errorf(\"status %d\", resp.StatusCode)\n"
    "\t}\n\n"
    "\tr.stats.PacketsSent.Add(uint64(len(batch)))\n",
    "\tif resp.StatusCode == http.StatusRequestEntityTooLarge {\n"
    "\t\tr.stats.HTTP413s.Add(1)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Bytes, bodyBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413RawBytes, rawBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Packets, packetCount)\n"
    "\t\treturn fmt.Errorf(\"status 413\")\n"
    "\t}\n"
    "\tif resp.StatusCode != 204 && resp.StatusCode != 200 {\n"
    "\t\treturn fmt.Errorf(\"status %d\", resp.StatusCode)\n"
    "\t}\n\n"
    "\tatomicMax(&r.stats.MaxHTTPSuccessBytes, bodyBytes)\n"
    "\tatomicMax(&r.stats.MaxSuccessRawBytes, rawBytes)\n"
    "\tatomicMax(&r.stats.MaxSuccessPackets, packetCount)\n\n"
    "\tr.stats.PacketsSent.Add(uint64(len(batch)))\n",
    "sendBatch status telemetry",
)

# Redact known sensitive session/auth values from debug logs in this test build.
s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] action_url: %s\", actionURL)\n",
    "\tutils.Debugf(\"[VOLGA] action_url present: %v\", actionURL != \"\")\n",
    "redact action_url",
)
s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] POST %s (body %d bytes)\", actionURL, len(body))\n",
    "\tutils.Debugf(\"[VOLGA] POST auth/initial (body %d bytes)\", len(body))\n",
    "redact auth POST URL",
)
s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] Location: %s\", location[:minInt(len(location), 300)])\n",
    "\tutils.Debugf(\"[VOLGA] Location received (%d bytes)\", len(location))\n",
    "redact Location",
)
s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] auth OK: user=%d(%s) rp=%s sign=%s ts=%s\",\n"
    "\t\ta.UserID, a.UserIDStr, a.RequestPath, a.Sign, a.TS)\n",
    "\tutils.Debugf(\"[VOLGA] auth OK\")\n",
    "redact auth OK",
)
s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] WS connected: user=%s\", w.auth.UserIDStr)\n",
    "\tutils.Debugf(\"[VOLGA] WS connected\")\n",
    "redact WS connected",
)
s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] transport started: user=%d(%s) rp=%s\",\n"
    "\t\tauth.UserID, auth.UserIDStr, auth.RequestPath)\n",
    "\tutils.Debugf(\"[VOLGA] transport started\")\n",
    "redact transport started",
)

# Extend stats output with cumulative size envelope around HTTP 413.
s = replace_once(
    s,
    "\tvar lastSent, lastBytes, lastHTTP, lastFailed, lastRecv, lastRecvBytes, lastBatches, lastBatched uint64\n",
    "\tvar lastSent, lastBytes, lastHTTP, lastFailed, last413, lastRecv, lastRecvBytes, lastBatches, lastBatched uint64\n",
    "stats last counters",
)

s = replace_once(
    s,
    "\t\t\tfailed := t.stats.HTTPReqsFailed.Load()\n"
    "\t\t\trecv := t.stats.PacketsRecv.Load()\n",
    "\t\t\tfailed := t.stats.HTTPReqsFailed.Load()\n"
    "\t\t\thttp413 := t.stats.HTTP413s.Load()\n"
    "\t\t\trecv := t.stats.PacketsRecv.Load()\n",
    "stats load 413",
)

old_stats = "\t\t\tutils.Debugf(\"[VOLGA-STATS] send %d pkt/s (%d KB/s) | http %d req/s fail %d | batch %d (avg %.1f pkt) | recv %d pkt/s (%d KB/s) | busy %d/%d | q %d/%d batchq %d/%d\",\n\t\t\t\t(sent-lastSent)/5, (bytes-lastBytes)/5/1024,\n\t\t\t\t(httpReqs-lastHTTP)/5, failed-lastFailed,\n\t\t\t\t(batches-lastBatches)/5,\n\t\t\t\tfloat64(batched-lastBatched)/float64(maxU64(batches-lastBatches, 1)),\n\t\t\t\t(recv-lastRecv)/5, (recvBytes-lastRecvBytes)/5/1024,\n\t\t\t\tt.stats.WorkerBusy.Load(), t.config.WorkerCount,\n\t\t\t\tlen(t.relay.packetQueue), cap(t.relay.packetQueue),\n\t\t\t\tlen(t.relay.batchQueue), cap(t.relay.batchQueue))\n"
new_stats = "\t\t\tutils.Debugf(\"[VOLGA-STATS] send %d pkt/s (%d KB/s) | http %d req/s fail %d 413 %d | batch %d (avg %.1f pkt) | recv %d pkt/s (%d KB/s) | busy %d/%d | q %d/%d batchq %d/%d | size-ok body<=%d raw<=%d pkts<=%d | size-413 body>=%d raw>=%d pkts>=%d\",\n\t\t\t\t(sent-lastSent)/5, (bytes-lastBytes)/5/1024,\n\t\t\t\t(httpReqs-lastHTTP)/5, failed-lastFailed, http413-last413,\n\t\t\t\t(batches-lastBatches)/5,\n\t\t\t\tfloat64(batched-lastBatched)/float64(maxU64(batches-lastBatches, 1)),\n\t\t\t\t(recv-lastRecv)/5, (recvBytes-lastRecvBytes)/5/1024,\n\t\t\t\tt.stats.WorkerBusy.Load(), t.config.WorkerCount,\n\t\t\t\tlen(t.relay.packetQueue), cap(t.relay.packetQueue),\n\t\t\t\tlen(t.relay.batchQueue), cap(t.relay.batchQueue),\n\t\t\t\tt.stats.MaxHTTPSuccessBytes.Load(), t.stats.MaxSuccessRawBytes.Load(), t.stats.MaxSuccessPackets.Load(),\n\t\t\t\tt.stats.MinHTTP413Bytes.Load(), t.stats.MinHTTP413RawBytes.Load(), t.stats.MinHTTP413Packets.Load())\n"
s = replace_once(s, old_stats, new_stats, "stats format telemetry")

s = replace_once(
    s,
    "\t\t\tlastHTTP, lastFailed = httpReqs, failed\n",
    "\t\t\tlastHTTP, lastFailed, last413 = httpReqs, failed, http413\n",
    "stats save 413",
)

# Helpers for lock-free cumulative extrema.
s = replace_once(
    s,
    "func maxU64(a, b uint64) uint64 {\n"
    "\tif a > b {\n"
    "\t\treturn a\n"
    "\t}\n"
    "\treturn b\n"
    "}\n",
    "func maxU64(a, b uint64) uint64 {\n"
    "\tif a > b {\n"
    "\t\treturn a\n"
    "\t}\n"
    "\treturn b\n"
    "}\n\n"
    "func atomicMax(dst *atomic.Uint64, v uint64) {\n"
    "\tfor {\n"
    "\t\tcur := dst.Load()\n"
    "\t\tif v <= cur || dst.CompareAndSwap(cur, v) {\n"
    "\t\t\treturn\n"
    "\t\t}\n"
    "\t}\n"
    "}\n\n"
    "func atomicMinNonZero(dst *atomic.Uint64, v uint64) {\n"
    "\tfor {\n"
    "\t\tcur := dst.Load()\n"
    "\t\tif cur != 0 && v >= cur {\n"
    "\t\t\treturn\n"
    "\t\t}\n"
    "\t\tif dst.CompareAndSwap(cur, v) {\n"
    "\t\t\treturn\n"
    "\t\t}\n"
    "\t}\n"
    "}\n",
    "atomic extrema helpers",
)

path.write_text(s)
print("Volga central batcher v2 telemetry transform applied")
