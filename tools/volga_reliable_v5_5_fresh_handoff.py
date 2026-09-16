#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER V5.3.1 and intentionally INSTEAD OF V5.4.
#
# Field testing showed the useful throughput burst follows a fresh Yandex
# authorization/session, while merely rotating between two long-lived warm
# sessions does not restore speed. V5.5 therefore keeps the known-good V5.3.1
# DATA/reliability path and recycles the inactive Yandex carrier shortly before
# each handoff. Each activation gets a newly authorized carrier and a newly
# connected WS. The stale carrier is retired shortly after handoff so its old
# replay engine cannot keep loading the backend while inactive.

p = Path("main.go")
s = p.read_text()
s = replace_once(
    s,
    '\tvolgaRotateSeconds := flag.Int("volga-rotate-seconds", 25, "Seconds of DATA on each warm Volga carrier before rotation")\n',
    '\tvolgaRotateSeconds := flag.Int("volga-rotate-seconds", 15, "Seconds of DATA on each fresh Yandex carrier before re-auth handoff")\n',
    "V5.5 rotation default",
)
s = replace_once(
    s,
    '\t\t\tlog.Printf("Volga dual carrier: warm A/B rotation every %ds", *volgaRotateSeconds)\n',
    '\t\t\tlog.Printf("Volga dual carrier: fresh-session A/B handoff every %ds", *volgaRotateSeconds)\n',
    "V5.5 dual log",
)
p.write_text(s)

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

s = replace_once(
    s,
    '\tctx    context.Context\n\tcancel context.CancelFunc\n}\n',
    '\tctx       context.Context\n\tcancel    context.CancelFunc\n\tconnected atomic.Bool\n}\n',
    "V5.5 WS connected state",
)

s = replace_once(
    s,
    'func (w *wsListener) Stop() {\n\tw.cancel()\n}\n\nfunc (w *wsListener) run() {\n',
    'func (w *wsListener) Stop() {\n\tw.cancel()\n}\n\nfunc (w *wsListener) IsConnected() bool {\n\treturn w != nil && w.connected.Load()\n}\n\nfunc (w *wsListener) run() {\n',
    "V5.5 WS readiness method",
)

s = replace_once(
    s,
    '\tconn, _, err := dialer.Dial(wsURL, header)\n\tif err != nil {\n\t\treturn fmt.Errorf("dial: %w", err)\n\t}\n\tdefer conn.Close()\n\n\tutils.Debugf("[VOLGA] WS connected: user=%s", w.auth.UserIDStr)\n',
    '\tconn, _, err := dialer.Dial(wsURL, header)\n\tif err != nil {\n\t\treturn fmt.Errorf("dial: %w", err)\n\t}\n\tw.connected.Store(true)\n\tdefer w.connected.Store(false)\n\tdefer conn.Close()\n\n\tutils.Debugf("[VOLGA] WS connected: user=%s", w.auth.UserIDStr)\n',
    "V5.5 WS readiness lifecycle",
)

p.write_text(s)

Path("transport/yandex/vyandex_dual_v5_3.go").write_text(
    Path("tools/volga_dual_v5_5.go.txt").read_text()
)
Path("transport/yandex/vyandex_dual_v5_3_test.go").write_text(
    Path("tools/volga_dual_v5_5_test.go.txt").read_text()
)

print("Volga V5.5 fresh re-auth handoff transform applied")
