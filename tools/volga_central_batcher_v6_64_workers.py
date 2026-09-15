#!/usr/bin/env python3
from pathlib import Path

p = Path("transport/yandex/vyandex.go")
s = p.read_text()
old = "\t\tWorkerCount: 1,"
new = "\t\tWorkerCount: 64,"
if old not in s:
    raise SystemExit("expected V5 WorkerCount default not found")
s = s.replace(old, new, 1)
p.write_text(s)
print("V6 transform applied: WorkerCount 1 -> 64")
