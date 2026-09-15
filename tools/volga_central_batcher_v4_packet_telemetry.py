from pathlib import Path

path = Path("transport/yandex/vyandex.go")
s = path.read_text()


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# V4 is telemetry-only. It does not change batching, routing or transport behaviour.
# It proves whether packets larger than the V3 raw batch cap reach relayClient.Send,
# and whether HTTP 413 responses are caused by single oversized packets.
s = replace_once(
    s,
    "\tMinHTTP413Packets     atomic.Uint64\n\tWSReconnects          atomic.Uint64\n",
    "\tMinHTTP413Packets          atomic.Uint64\n"
    "\tOversizePackets            atomic.Uint64\n"
    "\tMaxPacketBytes             atomic.Uint64\n"
    "\tHTTP413SinglePackets       atomic.Uint64\n"
    "\tMinHTTP413SinglePacketBytes atomic.Uint64\n"
    "\tMaxHTTP413SinglePacketBytes atomic.Uint64\n"
    "\tWSReconnects               atomic.Uint64\n",
    "V4 packet telemetry fields",
)

s = replace_once(
    s,
    "\tif len(data) > r.config.MaxPayloadBytes {\n"
    "\t\treturn fmt.Errorf(\"packet too large: %d > %d\", len(data), r.config.MaxPayloadBytes)\n"
    "\t}\n\n"
    "\tcp := make([]byte, len(data))\n",
    "\tif len(data) > r.config.MaxPayloadBytes {\n"
    "\t\treturn fmt.Errorf(\"packet too large: %d > %d\", len(data), r.config.MaxPayloadBytes)\n"
    "\t}\n\n"
    "\tpacketBytes := uint64(len(data))\n"
    "\tatomicMax(&r.stats.MaxPacketBytes, packetBytes)\n"
    "\tif len(data) > r.config.BatchMaxBytes {\n"
    "\t\tr.stats.OversizePackets.Add(1)\n"
    "\t}\n\n"
    "\tcp := make([]byte, len(data))\n",
    "relay Send packet telemetry",
)

s = replace_once(
    s,
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\trawBytes := uint64(totalBytes)\n"
    "\tpacketCount := uint64(len(batch))\n",
    "\tbodyBytes := uint64(len(bodyCopy))\n"
    "\trawBytes := uint64(totalBytes)\n"
    "\tpacketCount := uint64(len(batch))\n"
    "\tvar largestPacketBytes uint64\n"
    "\tfor _, p := range batch {\n"
    "\t\tif uint64(len(p)) > largestPacketBytes {\n"
    "\t\t\tlargestPacketBytes = uint64(len(p))\n"
    "\t\t}\n"
    "\t}\n",
    "sendBatch largest packet capture",
)

s = replace_once(
    s,
    "\tif resp.StatusCode == http.StatusRequestEntityTooLarge {\n"
    "\t\tr.stats.HTTP413s.Add(1)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Bytes, bodyBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413RawBytes, rawBytes)\n"
    "\t\tatomicMinNonZero(&r.stats.MinHTTP413Packets, packetCount)\n"
    "\t\treturn fmt.Errorf(\"status 413\")\n"
    "\t}\n",
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
    "\t}\n",
    "413 single-packet telemetry",
)

s = replace_once(
    s,
    "\tvar lastSent, lastBytes, lastHTTP, lastFailed, last413, lastRecv, lastRecvBytes, lastBatches, lastBatched uint64\n",
    "\tvar lastSent, lastBytes, lastHTTP, lastFailed, last413, lastOversize, last413Single, lastRecv, lastRecvBytes, lastBatches, lastBatched uint64\n",
    "V4 stats last counters",
)

s = replace_once(
    s,
    "\t\t\thttp413 := t.stats.HTTP413s.Load()\n"
    "\t\t\trecv := t.stats.PacketsRecv.Load()\n",
    "\t\t\thttp413 := t.stats.HTTP413s.Load()\n"
    "\t\t\toversize := t.stats.OversizePackets.Load()\n"
    "\t\t\thttp413Single := t.stats.HTTP413SinglePackets.Load()\n"
    "\t\t\trecv := t.stats.PacketsRecv.Load()\n",
    "V4 stats load counters",
)

s = replace_once(
    s,
    "\t\t\t\tt.stats.MaxHTTPSuccessBytes.Load(), t.stats.MaxSuccessRawBytes.Load(), t.stats.MaxSuccessPackets.Load(),\n"
    "\t\t\t\tt.stats.MinHTTP413Bytes.Load(), t.stats.MinHTTP413RawBytes.Load(), t.stats.MinHTTP413Packets.Load())\n\n"
    "\t\t\tlastSent, lastBytes = sent, bytes\n",
    "\t\t\t\tt.stats.MaxHTTPSuccessBytes.Load(), t.stats.MaxSuccessRawBytes.Load(), t.stats.MaxSuccessPackets.Load(),\n"
    "\t\t\t\tt.stats.MinHTTP413Bytes.Load(), t.stats.MinHTTP413RawBytes.Load(), t.stats.MinHTTP413Packets.Load())\n"
    "\t\t\tutils.Debugf(\"[VOLGA-PKT] over-cap %d/5s cap=%d max-pkt=%d | 413-single %d/5s total=%d range=%d..%d\",\n"
    "\t\t\t\toversize-lastOversize, t.config.BatchMaxBytes, t.stats.MaxPacketBytes.Load(),\n"
    "\t\t\t\thttp413Single-last413Single, http413Single,\n"
    "\t\t\t\tt.stats.MinHTTP413SinglePacketBytes.Load(), t.stats.MaxHTTP413SinglePacketBytes.Load())\n\n"
    "\t\t\tlastSent, lastBytes = sent, bytes\n",
    "V4 packet stats output",
)

s = replace_once(
    s,
    "\t\t\tlastHTTP, lastFailed, last413 = httpReqs, failed, http413\n",
    "\t\t\tlastHTTP, lastFailed, last413 = httpReqs, failed, http413\n"
    "\t\t\tlastOversize, last413Single = oversize, http413Single\n",
    "V4 stats save counters",
)

path.write_text(s)
print("Volga central batcher v4 packet telemetry transform applied")
