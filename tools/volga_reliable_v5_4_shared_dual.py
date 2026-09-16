#!/usr/bin/env python3
import re
from pathlib import Path


def insert_struct_field(text: str, struct_marker: str, end_marker: str, field: str, label: str) -> str:
    start = text.find(struct_marker)
    if start < 0:
        raise SystemExit(f"{label}: struct marker not found")
    end = text.find(end_marker, start)
    if end < 0:
        raise SystemExit(f"{label}: end marker not found")
    region = text[start:end]
    close = region.rfind("\n}")
    if close < 0:
        raise SystemExit(f"{label}: struct close not found")
    if field.strip() in region:
        return text
    region = region[:close] + field + region[close:]
    return text[:start] + region + text[end:]


# V5.4 fixes the architectural flaw found in the V5.3.1 field test:
# the two child transports had independent replay/ACK engines, so a standby
# document kept retransmitting old DATA and never actually cooled down.
#
# V5.4 keeps ONE logical V5.2 reliability engine (owned by carrier A) above two
# warm Yandex wire carriers. New DATA, ACKs and retransmits all use the current
# active carrier. Both WS listeners remain warm and feed one shared receive/
# dedupe/ACK state. Therefore the inactive document sends no logical traffic.
# A long application idle resets the next DATA window to carrier A.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

s = insert_struct_field(
    s,
    "type relayClient struct {",
    "\nfunc newRelayClient(",
    "\n\t// V5.4: immutable selector installed by the dual wrapper. Reliability\n"
    "\t// state stays on this relay, while the selected relay supplies Yandex\n"
    "\t// auth/frontier/http/wire identities for each POST.\n"
    "\tvolgaReliableWireSelector func() *relayClient\n",
    "relayClient V5.4 selector",
)

s = insert_struct_field(
    s,
    "type wsListener struct {",
    "\nfunc newWSListener(",
    "\n\t// V5.4: secondary WS delegates V5 DATA/ACK processing to the primary\n"
    "\t// listener so both documents share one receive sequence space.\n"
    "\tvolgaReliableOwner *wsListener\n",
    "wsListener V5.4 owner",
)

s = insert_struct_field(
    s,
    "type YandexVolgaTransport struct {",
    "\nfunc NewYandexVolgaTransport(",
    "\n\t// V5.4 dual-only hooks. Single-carrier construction leaves them nil.\n"
    "\tvolgaReliableOwner        *wsListener\n"
    "\tvolgaReliableWireSelector func() *relayClient\n",
    "YandexVolgaTransport V5.4 hooks",
)

needle = "\tt.relay = newRelayClient(auth, t.config, t.stats)\n\tt.relay.Start()\n"
replace = (
    "\tt.relay = newRelayClient(auth, t.config, t.stats)\n"
    "\tif t.volgaReliableWireSelector != nil {\n"
    "\t\tt.relay.volgaReliableWireSelector = t.volgaReliableWireSelector\n"
    "\t}\n"
    "\tt.relay.Start()\n"
)
if needle not in s:
    raise SystemExit("V5.4 relay start hook not found")
s = s.replace(needle, replace, 1)

needle = "\tt.ws.Start()\n"
replace = (
    "\tif t.volgaReliableOwner != nil {\n"
    "\t\tt.ws.volgaReliableOwner = t.volgaReliableOwner\n"
    "\t}\n"
    "\tt.ws.Start()\n"
)
if needle not in s:
    raise SystemExit("V5.4 WS start hook not found")
s = s.replace(needle, replace, 1)

anchor = "func (r *relayClient) SetFrontier(opID string) {\n"
if anchor not in s:
    raise SystemExit("V5.4 wire target anchor not found")
helper = '''func (r *relayClient) volgaReliableWireTarget() *relayClient {
\tif r != nil && r.volgaReliableWireSelector != nil {
\t\tif target := r.volgaReliableWireSelector(); target != nil {
\t\t\treturn target
\t\t}
\t}
\treturn r
}

'''
if helper not in s:
    s = s.replace(anchor, helper + anchor, 1)

start = s.find("func (r *relayClient) postBatchV5(")
if start < 0:
    raise SystemExit("V5.4 postBatchV5 start not found")
end = s.find("\nfunc (r *relayClient) volgaReliableWireTarget()", start)
if end < 0:
    end = s.find("\nfunc (r *relayClient) SetFrontier", start)
if end < 0:
    raise SystemExit("V5.4 postBatchV5 end not found")
region = s[start:end]
brace = region.find("{\n")
if brace < 0:
    raise SystemExit("V5.4 postBatchV5 brace not found")
head = region[: brace + 2]
body = region[brace + 2 :]
if "wire := r.volgaReliableWireTarget()" not in body:
    # Rewrite only the relay receiver token `r.`. A plain string replacement
    # corrupts identifiers such as req.Header -> req.Headewire.
    body = re.sub(r"\br\.", "wire.", body)
    body = body.replace("wire.volgaReliableSession()", "r.volgaReliableSession()")
    body = body.replace("wire.queueFragments(", "r.queueFragments(")
    body = body.replace("wire.lowerAdaptiveSingleBodyLimit(", "r.lowerAdaptiveSingleBodyLimit(")
    region = head + "\twire := r.volgaReliableWireTarget()\n" + body
    s = s[:start] + region + s[end:]

p.write_text(s)

p = Path("transport/yandex/volga_reliable_v5.go")
s = p.read_text()

needle = "func (w *wsListener) volgaReliableRecvState() *volgaReliableRecvState {\n"
replace = needle + "\tif w.volgaReliableOwner != nil && w.volgaReliableOwner != w {\n\t\treturn w.volgaReliableOwner.volgaReliableRecvState()\n\t}\n"
if needle not in s:
    raise SystemExit("V5.4 recv state function not found")
if "return w.volgaReliableOwner.volgaReliableRecvState()" not in s:
    s = s.replace(needle, replace, 1)

needle = "func (w *wsListener) handleVolgaReliableRecords(records [][]byte, decodedBytes int) {\n"
replace = needle + "\tif w.volgaReliableOwner != nil && w.volgaReliableOwner != w {\n\t\tw.volgaReliableOwner.handleVolgaReliableRecords(records, decodedBytes)\n\t\treturn\n\t}\n"
if needle not in s:
    raise SystemExit("V5.4 record handler not found")
if "w.volgaReliableOwner.handleVolgaReliableRecords(records, decodedBytes)" not in s:
    s = s.replace(needle, replace, 1)

p.write_text(s)

Path("transport/yandex/vyandex_dual_v5_3.go").write_text(
    Path("tools/volga_dual_v5_4.go.txt").read_text()
)
Path("transport/yandex/vyandex_dual_v5_3_test.go").write_text(
    Path("tools/volga_dual_v5_4_test.go.txt").read_text()
)

print("Volga V5.4 shared-reliability dual carrier transform applied")
