# OpenFlux Delta — transport reliability and one-document Legacy pool

Date: 2026-10-11 UTC; tests and restoration completed by 19:41 UTC.
Classification: REVIEW CANDIDATE; no production upgrade.

## Purpose and machines

Reproduce Mail.ru's roughly minute-long connection lifecycle and low upload rate,
restore useful Legacy data transfer under cursor size/rate limits, and test whether
multiple participants can accelerate the same Yandex document.

Mini PC2 and the existing AWS exit were used. Mini PC1 was not modified. One
mini-PC used each document at a time. Test processes used temporary directories,
a separate loopback SOCKS port, a private HTTP origin and an isolated AWS exit
container. AWS's corresponding installed exit services were temporarily paused
with a restoration timer. Installed binaries, router configuration and routes
were not replaced.

## Source and checkpoints

- Original release/base: v4.2.0, `8a5e21fd82a062bb9047423ae7d76550af08ac71`.
- Installed executable SHA-256 on both machines:
  `7279771e182cc5103efc5b6531893f919749d1c793591bb3026fd2e722e6adda`.
- Tested Legacy runtime source: `af3677e04a92b02db3a8a5e3d2b8f663c5279930`.
- Final Mail.ru runtime source: `72672c2ace08037c4a86bda0c00924db490ecfbd`.
- Tested candidate executable SHA-256:
  `9bb08cb236703e632f631da1598f78f2fa309b4f14ead6aff4653e289dc632d4`.
  Earlier Legacy matrix: `19817520cf612eaaa17ad72290844a6baf7739eb4dd76aed0c3b9065cbe12117`.
- Review branch: `codex/transport-reliability-v420`,
  [draft PR #39](https://github.com/LevL-max/OpenFlux-mod/pull/39).

Changes: `main.go`; `transport/mailru` authorization, controls, bounded reconnect
queue, document-key refresh and shutdown; `transport/yandex` cursor sizing, pacing, receiver parsing and
participant creation; `transport/fragments*` and `transport/striped*`; dedicated
tests; `cmd/legacy-lane-probe`; `scripts/transport_probe.py`; this report and its
evidence. Two integration test fixtures isolate `UNIT_DIR` from installed units.
Volga runtime and its frozen performance profile are unchanged.

## Verified outcome

Legacy: one document URL, 32 participants per endpoint. A single 8 MiB TCP file
passed in both directions at 1.426 / 0.405 Mbit/s. Four simultaneous 2 MiB files
passed at aggregate 1.973 / 1.357 Mbit/s (download / upload), all hashes correct.
Three HTTPS HTML requests returned 200 in 0.53–1.05 seconds with certificate
verification. AWS CPU was about 33% of one core at the captured load sample.
The 1/2/4/8/16/32 ramp, unsuccessful early trials and exact executable hashes
are retained in the [evidence](../evidence/transport-reliability-2026-10-11.json).

Mail.ru: the original v4.2.0 control reproduced close 1005 every approximately
62 seconds on each peer and a 2.539 Mbit/s short upload. Its short download was
28.797 Mbit/s; 64 MiB rate-limited transfers passed both hashes despite reconnects.
The final candidate passed uncapped 256 MiB in each direction at 21.787 / 6.092
Mbit/s (98.569 / 352.508 seconds), with no established-socket closures during
those files. Initial authorization required two retries; do not interpret the
nonzero reconnect counter as two drops during the file test. After a deliberate
TLS-socket interruption, the document connection recovered in 1.458 seconds and
the same inner TCP download completed 32 MiB with the correct hash. The fault
time was inside that transfer. Two test-container TLS sockets were closed.

An intermediate candidate's first large transfer failed because identical public
URLs had different internal editor keys. A forced reconnect matched the keys and
restored 16 MiB transfers at 27.701 / 42.763 Mbit/s. The final runtime adds metadata
refresh every 30 seconds and rejoins only on key change. A mock editor verifies
automatic room rollover; the live diagnosis used manual reconnection. Short-rate
bursts are not sustained capacity, and stable 18–29 Mbit/s upload remains unproven.

Validation of the final runtime source passed on GitHub: all Go tests, vet,
race tests, Volga opt-in/root and separate-module checks, 115 Python integration
tests, Linux builds and container smoke tests. Additional Linux checks on the
test host passed. See the [report](../TRANSPORT-RELIABILITY-2026-10-11.md) for
reproduction commands and the scope of each measurement.

## Production decision and handoff

Do not merge, release or replace installed executables in this session. The
candidate is opt-in for Legacy and requires matching settings on both peers:
`--yandex-safe-mode=true --yandex-lanes=32 --compression=true`. Existing generated
units and router UI do not enable this profile automatically. Mail.ru retains
the existing data wire format.

Rollback checkpoint: the unchanged installed v4.2.0 executables and existing
unit/configuration files. Restoration verified: AWS Mail.ru/Yandex exits active,
AWS Volga inactive; Mini PC2's three installed clients inactive. Both installed
hashes matched the checkpoint. No test units, owned container or cleanup timer
remained. Both temporary directories, cookie copies and private logs were removed.
An initial cleanup helper assumed Docker on Mini PC2; it was corrected for native
client units and cleanup/verification then passed on both hosts.
No upstream review cursor changes. No production Master Handover is superseded.
The earlier local concept-only document is superseded by this tested candidate.

Unresolved: no overnight or simultaneous-direction full-rate soak; no guarantee
for arbitrary long carrier outages or exit-process restarts; no UDP delivery
guarantee for Legacy; no full rendered-browser workload; provider limits beyond
32 participants per endpoint are unmeasured. The scope of the provider's quotas
must not be inferred from its total corporate user population.

CURRENT ACTION = review PR #39, especially Legacy's matching opt-in configuration
and Mail.ru key-refresh behavior; make a separate coordinated merge/release/rollout
decision. No merge, release, router change or installed-binary replacement occurred.
