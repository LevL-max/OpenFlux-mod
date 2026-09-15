#!/usr/bin/env python3
from pathlib import Path

p = Path("transport/yandex/vyandex.go")
s = p.read_text()
old = "\t\tWorkerCount: 2000,"
new = "\t\tWorkerCount: 1,"
if old not in s:
    raise SystemExit("expected WorkerCount default not found")
s = s.replace(old, new, 1)
p.write_text(s)
print("V5 transform applied: WorkerCount 2000 -> 1")
