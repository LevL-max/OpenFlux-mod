#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER V5.2. Keep both Yandex documents authorized and their WS
# listeners warm, but send payload DATA through only one at a time. Rotation is
# make-before-break: the old carrier stays alive for late ACK/replay traffic.
# This first experiment deliberately does not stripe DATA and does not increase
# worker count, so it isolates per-document/session fatigue from concurrency.

p = Path("main.go")
s = p.read_text()

s = replace_once(
    s,
    '\tvolgaTelemetry := flag.Bool("volga-telemetry", true, "Enable aggregate Volga benchmark telemetry")\n',
    '\tvolgaTelemetry := flag.Bool("volga-telemetry", true, "Enable aggregate Volga benchmark telemetry")\n'
    '\tvolgaURLSecondary := flag.String("volga-url-secondary", "", "Second Yandex document URL for warm dual-carrier rotation")\n'
    '\tvolgaRotateSeconds := flag.Int("volga-rotate-seconds", 25, "Seconds of DATA on each warm Volga carrier before rotation")\n',
    "V5.3 dual CLI flags",
)

s = replace_once(
    s,
    '\tif *volgaNormalizeMTU != 0 && (*volgaNormalizeMTU < 576 || *volgaNormalizeMTU > 9000) {\n'
    '\t\tlog.Fatalf("Invalid --volga-normalize-mtu: %d", *volgaNormalizeMTU)\n'
    '\t}\n'
    '\tvar trans transport.Transport\n',
    '\tif *volgaNormalizeMTU != 0 && (*volgaNormalizeMTU < 576 || *volgaNormalizeMTU > 9000) {\n'
    '\t\tlog.Fatalf("Invalid --volga-normalize-mtu: %d", *volgaNormalizeMTU)\n'
    '\t}\n'
    '\tif *volgaRotateSeconds < 5 || *volgaRotateSeconds > 300 {\n'
    '\t\tlog.Fatalf("Invalid --volga-rotate-seconds: %d", *volgaRotateSeconds)\n'
    '\t}\n'
    '\tvar trans transport.Transport\n',
    "V5.3 rotation validation",
)

s = replace_once(
    s,
    '\tcase "vyandex":\n\t\tvolgaTransport := yandex.NewYandexVolgaTransport(globalDocUrl, config)\n',
    '\tcase "vyandex":\n'
    '\t\tvar volgaTransport transport.Transport\n'
    '\t\tif *volgaURLSecondary != "" {\n'
    '\t\t\tvolgaTransport = yandex.NewDualYandexVolgaTransport(globalDocUrl, *volgaURLSecondary, time.Duration(*volgaRotateSeconds)*time.Second, config)\n'
    '\t\t\tlog.Printf("Volga dual carrier: warm A/B rotation every %ds", *volgaRotateSeconds)\n'
    '\t\t} else {\n'
    '\t\t\tvolgaTransport = yandex.NewYandexVolgaTransport(globalDocUrl, config)\n'
    '\t\t}\n',
    "V5.3 dual constructor",
)

p.write_text(s)

Path("transport/yandex/vyandex_dual_v5_3.go").write_text(
    Path("tools/volga_dual_v5_3.go.txt").read_text()
)
Path("transport/yandex/vyandex_dual_v5_3_test.go").write_text(
    Path("tools/volga_dual_v5_3_test.go.txt").read_text()
)

print("Volga V5.3 dual warm-carrier transform applied")
