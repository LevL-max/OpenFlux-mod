# Sequential mini-PC functional check on the existing AWS

Accepted topology: one AWS Volga container, one pair of documents and the existing
recovery key set. PC1 and PC2 use matching configurations **one at a time**. No
multi-instance or multi-client wire changes were retained.

The final integration executable was run in a temporary bounded AWS container
and temporary mini-PC services on a separate loopback SOCKS port. Legacy,
installed panels, router configuration and routes were not changed. Both clients
used copies of their existing browser cookies; the AWS used its own cookie copy.

| Check | PC2 first | PC1 after stopping PC2 |
|---|---:|---:|
| Simultaneous SOCKS connections | 64 | 64 |
| 65th connection rejected | PASS | PASS |
| Downloads / uploads | 32 / 32 | 32 / 32 |
| Payload per transfer | 128 KiB | 128 KiB |
| SHA-256 matches | 64 / 64 | 64 / 64 |
| Verified cohort payload | 8 MiB | 8 MiB |
| Public HTTPS with TLS verification | PASS | PASS |
| Private/metadata destinations denied | PASS | PASS |
| Half-close and recovery after abort | PASS | PASS |
| Functional helper exit | 0 | 0 |
| Client cgroup peak memory | 94.81 MiB | 64.36 MiB |
| AWS cgroup peak memory | 81.32 MiB | 96.26 MiB |

The same AWS Volga process served PC2 and then PC1. AWS memory peaks and CPU
counters are cumulative across both stages and include the small loopback test
origin. Limits were 256 MiB, no swap and 50% of one CPU on each Volga runtime.
`nr_throttled` was 18 on PC2, 14 on PC1 and 67 cumulatively on AWS. Full CPU and
memory counters, per-transfer hashes and event results are in the accompanying
[JSON evidence](VOLGA-AWS-FUNCTIONAL-2026-09-28.json). These are functional checks,
not capacity estimates or new speed benchmarks; the frozen profile is unchanged.

The first cohort attempt followed an HTTPS warm-up and could not admit all 64
new connections, consistent with the earlier connection still occupying a slot. It stopped
before payload transfer. The final check starts its 64-connection cohort on an
unused listener and performs HTTPS afterwards; both clients passed that sequence.
The configured limit was not raised to accommodate the warm-up connection.

Executable SHA-256:
`116e9b2b17edc3510b3db3278eec0ed769b70fed852c666a835cc446587dede4`.
This is the already CI-verified integration executable. New Python changes only
separate the Volga release channel from Legacy and explain single-client use.

All temporary containers, units, timers and private directories were cleaned.
Original service PIDs/states matched their pre-test values on all three hosts.
Installing the RC through the real updater, testing Disk cookie recovery and
Legacy/Volga panel selection/rollback remain the deployment checks; this report
does not claim those steps are already complete.

## Subsequent installed rollout

The deployment checks discussed above were later exercised on AWS and both
mini-PCs. See [the RC2 rollout report](VOLGA-ROLLOUT-2026-09-28.md) for the final
installed versions, preserved state, cleanup, PC1 compatibility fix and observed
transient failures. RC2 remains a prerelease.
