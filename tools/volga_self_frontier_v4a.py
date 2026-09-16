#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


def replace_in_region(text: str, start_marker: str, end_marker: str,
                      old: str, new: str, expected: int, label: str) -> str:
    start = text.index(start_marker)
    end = text.index(end_marker, start)
    region = text[start:end]
    count = region.count(old)
    if count != expected:
        raise SystemExit(f"{label}: expected {expected} matches, got {count}")
    region = region.replace(old, new)
    return text[:start] + region + text[end:]


# Applied AFTER the existing V1..V6 + benchmark harness + fragmentation +
# HTTP-shards-v3 transforms.
#
# Experiment goal:
#   Yandex echoes our own collaboration operations on the websocket.  The
#   current transport discards every self-authored websocket message before
#   reading operation IDs, so the sender frontier can remain stale while many
#   relay POSTs fan out from the same generation.  Keep suppressing our own
#   encoded tunnel payload (no loopback), but consume self operation IDs so the
#   confirmed Yandex frontier can advance.
#
# Deliberately NOT changed here: worker/batch/shard settings, HTTP transport,
# fragmentation, auth/reconnect lifecycle, retries, compression or TCP buffers.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

s = replace_once(
    s,
    "\tif inner.UserID == w.auth.UserID {\n"
    "\t\treturn\n"
    "\t}\n\n"
    "\tswitch inner.T {\n"
    "\tcase \"relay\":\n"
    "\t\tw.handleRelayMessage(inner.Message)\n"
    "\tcase \"exchange\":\n"
    "\t\tw.handleBundle(inner.Bundle)\n"
    "\t}\n",
    "\tdeliverData := inner.UserID != w.auth.UserID\n\n"
    "\tswitch inner.T {\n"
    "\tcase \"relay\":\n"
    "\t\tw.handleRelayMessage(inner.Message, deliverData)\n"
    "\tcase \"exchange\":\n"
    "\t\tw.handleBundle(inner.Bundle, deliverData)\n"
    "\t}\n",
    "self-message routing",
)

s = replace_once(
    s,
    "func (w *wsListener) handleRelayMessage(raw json.RawMessage) {",
    "func (w *wsListener) handleRelayMessage(raw json.RawMessage, deliverData bool) {",
    "relay handler signature",
)
s = replace_in_region(
    s,
    "func (w *wsListener) handleRelayMessage(",
    "func (w *wsListener) handleBundle(",
    "w.handleBundleItem(item)",
    "w.handleBundleItem(item, deliverData)",
    1,
    "relay bundle item delivery flag",
)

s = replace_once(
    s,
    "func (w *wsListener) handleBundle(raw json.RawMessage) {",
    "func (w *wsListener) handleBundle(raw json.RawMessage, deliverData bool) {",
    "exchange handler signature",
)
s = replace_in_region(
    s,
    "func (w *wsListener) handleBundle(",
    "func (w *wsListener) handleBundleItem(",
    "w.handleBundleItem(item)",
    "w.handleBundleItem(item, deliverData)",
    2,
    "exchange bundle item delivery flags",
)

s = replace_once(
    s,
    "func (w *wsListener) handleBundleItem(raw json.RawMessage) {",
    "func (w *wsListener) handleBundleItem(raw json.RawMessage, deliverData bool) {",
    "bundle item signature",
)
s = replace_in_region(
    s,
    "func (w *wsListener) handleBundleItem(",
    "func decodeBatch(",
    "\tvar asStr string\n",
    "\t// Self-authored websocket messages are acknowledgements for frontier\n"
    "\t// progression only. Never feed their encoded payload back into TUN.\n"
    "\tif !deliverData {\n"
    "\t\treturn\n"
    "\t}\n\n"
    "\tvar asStr string\n",
    1,
    "self payload suppression",
)

p.write_text(s)

Path("transport/yandex/volga_self_frontier_v4a_test.go").write_text(r'''package yandex

import (
    "encoding/base64"
    "encoding/json"
    "testing"
)

func makeVolgaWSMessage(t *testing.T, userID int, opID string, packet []byte) []byte {
    t.Helper()

    framed := make([]byte, 2+len(packet))
    framed[0] = byte(len(packet) >> 8)
    framed[1] = byte(len(packet))
    copy(framed[2:], packet)

    relayMessage := map[string]interface{}{
        "bundle": []interface{}{
            map[string]interface{}{
                "id":         opID,
                "actionName": "textInsert",
            },
            base64.StdEncoding.EncodeToString(framed),
        },
    }
    relayJSON, err := json.Marshal(relayMessage)
    if err != nil {
        t.Fatal(err)
    }

    inner := map[string]interface{}{
        "t":       "relay",
        "userId":  userID,
        "message": json.RawMessage(relayJSON),
    }
    innerJSON, err := json.Marshal(inner)
    if err != nil {
        t.Fatal(err)
    }

    envelope := map[string]interface{}{
        "operation": "SESSION",
        "message":   string(innerJSON),
    }
    raw, err := json.Marshal(envelope)
    if err != nil {
        t.Fatal(err)
    }
    return raw
}

func TestVolgaSelfMessageAdvancesFrontierWithoutLoopback(t *testing.T) {
    relay := &relayClient{stats: &VolgaStats{}}
    delivered := 0
    w := &wsListener{
        auth:  &volgaAuth{UserID: 42},
        relay: relay,
        onData: func([]byte) {
            delivered++
        },
    }

    w.handleMessage(makeVolgaWSMessage(t, 42, "1-42.123", []byte("self")))

    frontier := relay.getFrontier()
    if len(frontier) != 1 || frontier[0] != "1-42.123" {
        t.Fatalf("self frontier = %#v, want [1-42.123]", frontier)
    }
    if delivered != 0 {
        t.Fatalf("self payload looped back %d times, want 0", delivered)
    }
}

func TestVolgaPeerMessageStillDeliversPayload(t *testing.T) {
    relay := &relayClient{stats: &VolgaStats{}}
    var got []byte
    w := &wsListener{
        auth:  &volgaAuth{UserID: 42},
        relay: relay,
        onData: func(pkt []byte) {
            got = append([]byte(nil), pkt...)
        },
    }

    w.handleMessage(makeVolgaWSMessage(t, 99, "1-99.456", []byte("peer")))

    frontier := relay.getFrontier()
    if len(frontier) != 1 || frontier[0] != "1-99.456" {
        t.Fatalf("peer frontier = %#v, want [1-99.456]", frontier)
    }
    if string(got) != "peer" {
        t.Fatalf("peer payload = %q, want %q", string(got), "peer")
    }
}
''')

print("Volga self-frontier v4a transform applied")
