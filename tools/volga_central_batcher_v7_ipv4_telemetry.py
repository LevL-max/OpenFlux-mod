from pathlib import Path

path = Path("transport/yandex/vyandex.go")
s = path.read_text()


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# V7 is telemetry-only. It does not change batching, routing, packet contents,
# HTTP concurrency, MTU, fragmentation or transport behaviour.
# For packets already identified by V4 as larger than BatchMaxBytes, print only
# numeric IPv4/TCP header-derived fields. No addresses, ports, payload, auth or
# session identifiers are logged.
s = replace_once(
    s,
    "\tif len(data) > r.config.BatchMaxBytes {\n"
    "\t\tr.stats.OversizePackets.Add(1)\n"
    "\t}\n\n"
    "\tcp := make([]byte, len(data))\n",
    "\tif len(data) > r.config.BatchMaxBytes {\n"
    "\t\tr.stats.OversizePackets.Add(1)\n"
    "\t\tactualLen := len(data)\n"
    "\t\tipTotalLen := -1\n"
    "\t\tihl := -1\n"
    "\t\tdf := false\n"
    "\t\tmf := false\n"
    "\t\tfragOffset := -1\n"
    "\t\ttcpPayloadLen := -1\n"
    "\t\tif len(data) >= 20 && data[0]>>4 == 4 {\n"
    "\t\t\tihl = int(data[0]&0x0f) * 4\n"
    "\t\t\tipTotalLen = int(binary.BigEndian.Uint16(data[2:4]))\n"
    "\t\t\tflagsFrag := binary.BigEndian.Uint16(data[6:8])\n"
    "\t\t\tdf = flagsFrag&0x4000 != 0\n"
    "\t\t\tmf = flagsFrag&0x2000 != 0\n"
    "\t\t\tfragOffset = int(flagsFrag & 0x1fff)\n"
    "\t\t\tif data[9] == 6 && ihl >= 20 && len(data) >= ihl+20 {\n"
    "\t\t\t\ttcpHeaderLen := int((data[ihl+12] >> 4) & 0x0f) * 4\n"
    "\t\t\t\tif tcpHeaderLen >= 20 && ipTotalLen >= ihl+tcpHeaderLen {\n"
    "\t\t\t\t\ttcpPayloadLen = ipTotalLen - ihl - tcpHeaderLen\n"
    "\t\t\t\t}\n"
    "\t\t\t}\n"
    "\t\t}\n"
    "\t\tutils.Debugf(\"[VOLGA-IPV4] oversize actual=%d ip-total=%d ihl=%d df=%t mf=%t frag-off=%d tcp-payload=%d\",\n"
    "\t\t\tactualLen, ipTotalLen, ihl, df, mf, fragOffset, tcpPayloadLen)\n"
    "\t}\n\n"
    "\tcp := make([]byte, len(data))\n",
    "V7 oversized IPv4/TCP telemetry",
)

path.write_text(s)
print("Volga central batcher v7 IPv4/TCP telemetry transform applied")
