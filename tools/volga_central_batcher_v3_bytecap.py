from pathlib import Path

path = Path("transport/yandex/vyandex.go")
s = path.read_text()


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Use a conservative raw batch cap derived from the observed Volga HTTP 413 envelope.
# V2 telemetry observed success at body=6434/raw=4503 and 413 at body=8293/raw=5898.
# 5000 raw bytes leaves ~1 KiB headroom below an apparent ~8 KiB request ceiling.
s = replace_once(
    s,
    "\t\tBatchMaxBytes: 4 * 1024 * 1024,\n",
    "\t\tBatchMaxBytes: 5000,\n",
    "BatchMaxBytes default",
)

old = r'''\t\tcase pkt := <-r.packetQueue:
\t\t\tbatch = append(batch, pkt)
\t\t\ttotalBytes += len(pkt)

\t\t\tif len(batch) >= batchCap || totalBytes >= r.config.BatchMaxBytes {
\t\t\t\tif !flush() {
\t\t\t\t\treturn
\t\t\t\t}
\t\t\t} else if len(batch) == 1 {
\t\t\t\tarmTimer()
\t\t\t}
'''

new = r'''\t\tcase pkt := <-r.packetQueue:
\t\t\t// Flush the current batch BEFORE appending a packet that would cross
\t\t\t// the byte cap. Flushing only after append can overshoot the HTTP
\t\t\t// request-size ceiling and cause Volga to return 413.
\t\t\tif len(batch) > 0 && totalBytes+len(pkt) > r.config.BatchMaxBytes {
\t\t\t\tif !flush() {
\t\t\t\t\treturn
\t\t\t\t}
\t\t\t}

\t\t\tbatch = append(batch, pkt)
\t\t\ttotalBytes += len(pkt)

\t\t\tif len(batch) >= batchCap || totalBytes >= r.config.BatchMaxBytes {
\t\t\t\tif !flush() {
\t\t\t\t\treturn
\t\t\t\t}
\t\t\t} else if len(batch) == 1 {
\t\t\t\tarmTimer()
\t\t\t}
'''

s = replace_once(s, old, new, "byte-aware preflush")

path.write_text(s)
print("Volga central batcher v3 byte-cap transform applied")
