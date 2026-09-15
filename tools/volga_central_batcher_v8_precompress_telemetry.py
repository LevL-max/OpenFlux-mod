from pathlib import Path

path = Path("transport/compressor.go")
s = path.read_text()


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# V8 is telemetry-only and intentionally instruments BEFORE the compression
# wrapper hands data to Volga. V7 proved that telemetry inside vyandex.Send sees
# the compressed/framed bytes rather than the original IP packet.
s = replace_once(
    s,
    'import (\n\t"bytes"\n\t"io"\n\n\t"github.com/pierrec/lz4/v4"\n)',
    'import (\n\t"bytes"\n\t"encoding/binary"\n\t"io"\n\n\t"github.com/pierrec/lz4/v4"\n\t"universal-bypass-tool/utils"\n)',
    "V8 compressor imports",
)

s = replace_once(
    s,
    'func (c *CompressedTransport) Send(data []byte) error {\n\tcompressed := compress(data)\n\treturn c.Transport.Send(compressed)\n}',
    '''func (c *CompressedTransport) Send(data []byte) error {
\tcompressed := compress(data)
\tif len(compressed) > 5000 {
\t\trawLen := len(data)
\t\tframedLen := len(compressed)
\t\tmarker := -1
\t\tif framedLen > 0 {
\t\t\tmarker = int(compressed[0])
\t\t}

\t\tversion := -1
\t\tipTotalLen := -1
\t\tihl := -1
\t\tdf := false
\t\tmf := false
\t\tfragOffset := -1
\t\ttcpPayloadLen := -1
\t\tif rawLen >= 20 {
\t\t\tversion = int(data[0] >> 4)
\t\t\tif version == 4 {
\t\t\t\tihl = int(data[0]&0x0f) * 4
\t\t\t\tipTotalLen = int(binary.BigEndian.Uint16(data[2:4]))
\t\t\t\tflagsFrag := binary.BigEndian.Uint16(data[6:8])
\t\t\t\tdf = flagsFrag&0x4000 != 0
\t\t\t\tmf = flagsFrag&0x2000 != 0
\t\t\t\tfragOffset = int(flagsFrag & 0x1fff)
\t\t\t\tif data[9] == 6 && fragOffset == 0 && ihl >= 20 && rawLen >= ihl+20 {
\t\t\t\t\ttcpHeaderLen := int((data[ihl+12] >> 4) & 0x0f) * 4
\t\t\t\t\tif tcpHeaderLen >= 20 && ipTotalLen >= ihl+tcpHeaderLen {
\t\t\t\t\t\ttcpPayloadLen = ipTotalLen - ihl - tcpHeaderLen
\t\t\t\t\t}
\t\t\t\t}
\t\t\t}
\t\t}

\t\tutils.Debugf("[COMP-IPV4] over-cap raw=%d framed=%d marker=%d version=%d ip-total=%d ihl=%d df=%t mf=%t frag-off=%d tcp-payload=%d",
\t\t\trawLen, framedLen, marker, version, ipTotalLen, ihl, df, mf, fragOffset, tcpPayloadLen)
\t}
\treturn c.Transport.Send(compressed)
}''',
    "V8 pre-compression telemetry",
)

path.write_text(s)
print("Volga v8 pre-compression IPv4/TCP telemetry transform applied")
