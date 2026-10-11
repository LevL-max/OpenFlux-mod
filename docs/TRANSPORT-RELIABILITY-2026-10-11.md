# Transport reliability candidate, 2026-10-11

Base: OpenFlux-mod v4.2.0, `8a5e21fd82a062bb9047423ae7d76550af08ac71`.
Branch: `codex/transport-reliability-v420`. This is a review candidate, not a release.

## Implemented behavior

Mail.ru now waits for the document's `auth/result=1`, acknowledges `authChanges`
and the co-editing `waitAuth` transition, and derives its read deadline from the
Engine.IO heartbeat parameters. A failed write closes that socket so the reader
can reconnect. An older socket cannot mark a replacement disconnected.

The same queue survives reconnects. It owns copies of outgoing packets, is bounded
by both packet count and 8 MiB, and expires packets after three seconds. Expired
packets are recovered by the existing TCP retransmission mechanism; this is not
an application-level replay protocol. `Stop` cancels bootstrap HTTP requests,
WebSocket dialing, an in-progress authentication, timers and the writer. Readiness,
queue drops, write failures and reconnects appear in `[M-PERF]` status lines.
Document-changing fake editor activity remains off by default.

The live A/B transition exposed a second failure: the same public URL can give
the peers different internal editor keys. Both sockets remain authorized but are
in separate rooms, so no cursor data crosses. Closing only the old-key test socket
made it fetch the current key and immediately restored verified traffic.
Mail.ru now refreshes document metadata every 30 seconds and reconnects only if
the key changed. An unchanged key or a metadata request failure preserves the
healthy socket; a stale response cannot close a replacement session. `Stop`
cancels the refresh request as well. This adds at most two metadata requests per
minute per running connected endpoint, separate from cursor traffic.

Legacy has an opt-in `--yandex-safe-mode=true` profile. Enable it on **both peers**
with matching `--compression`. It forces single-packet batching, overriding
`--batch-packets`, because the four-packet live trial stalled during upload.
It limits complete serialized
cursor messages to 2600 bytes and spaces all application WebSocket writes,
including heartbeat/control replies, by at least 100 ms. Below compression, an
`OFL1` fragment envelope splits large packets into at most 1800-byte
binary messages. This is necessary because Linux raw sockets can deliver packets
larger than the nominal 1500-byte MTU. Reassembly is bounded to 64 partial packets,
128 KiB per packet and a 30-second expiry; it handles out-of-order and duplicate
fragments. It does not add reliable delivery to the cursor carrier. The receiver
now reads every cursor entry in a server message and skips only the keepalive
entry, instead of dropping the rest of that message.

The new Legacy wire format is not compatible with an unmodified v4.2.0 peer.
The flag defaults to false to avoid silently changing existing installations.
Mail.ru's cursor wire format remains unchanged. Volga's implementation and frozen
performance profile are unchanged.

Experimental `--yandex-lanes=N` (2 through 32) adds independent Legacy participants on the
same document. It requires safe mode and matching settings on both peers. An
`OFP1` envelope carries a random sender epoch and sequence number: receivers drop
their own broadcasts and duplicates seen on other connections. Replay memory
is bounded to eight epochs with 4096 sequence slots each. Sends alternate between
ready participants and can use the remaining participant during a reconnect.
This provides neither packet acknowledgments nor lossless replay; TCP still
handles packet loss. All participants share a serialized cookie store so they
cannot race its temporary-file/rename transaction. The default is one participant.
The extra 20-byte stripe envelope still fits below the 2600-byte serialized cap.
`[YDOCS-POOL] lanes_ready` reports how many authenticated participants are usable.
The count is a bounded experimental setting, not a claim that the provider allows
arbitrarily many participants. Broadcast traffic grows faster than useful payload;
choose a count from file-integrity, throughput and resource measurements.

## Why adding connections is a separate question

The v4.2.0 Volga runtime already has four lanes over two documents. It uses an HTTP
relay plus a push connection, a reliable record layer, a shared POST budget,
session identifiers, sequence numbers and acknowledgments. See
[Volga](../volga/README.md), [frozen profile](FROZEN-PERFORMANCE-PROFILE.json) and
`transport/yandex/volga_v6_*`. Panel V8 is the router UI revision that added Mail.ru;
it is not a faster Legacy transport.

A pool of OnlyOffice cursor WebSockets does not inherit Volga's throughput.
The participant/document/account/IP scope of current cursor limits is not assumed.
`cmd/legacy-lane-probe` tests one versus two active senders on the same document,
in each direction, with eight 1800-byte data messages/s per sender. Two receiver
sessions deduplicate broadcasts and validate SHA-256, so duplicates cannot inflate
the rate. Both sessions are open in each phase; the comparison changes the number
of active senders. This is a carrier probe, not proof of a bonded TCP connection.

At a nominal 3 KB JSON limit, Base64 alone leaves about 2.2 KB of binary payload.
Fifteen messages/s therefore permits only about 0.26 Mbit/s before other overhead,
delivery loss, control frames and TCP acknowledgments. The conservative profile's
1800-byte and ten-message limits cap its raw payload rate at 0.144 Mbit/s per
sender. These are calculations from the stated limits, not measured service SLAs.

## Live verification

Tests use Mini PC2 and the existing AWS, one active mini-PC at a time. Candidates
run from private temporary directories, with a separate loopback SOCKS port and
an isolated exit container. Existing installed executables and router routes are
not replaced. Corresponding AWS exit services are paused during each experiment
and protected by a restoration timer. Private document URLs, cookies and keys
are not part of this branch.

Each HTTP transfer has one TCP connection, no retry/resume, deterministic
pseudorandom data and an end-to-end SHA-256 check. Upload throughput is valid only
after the origin confirms the byte count and digest. A timeout after `send()` is
not evidence that those bytes arrived. Short tests measure rate; rate-limited long
tests exercise connection lifetime and must not be presented as maximum speed.

### Legacy: measured application throughput on one document

The setting is participants **per endpoint**: `--yandex-lanes=32` opens 32 on
Mini PC2 and 32 on AWS, all using the same document URL. The number of independent
HTTP transfers is a separate setting. No extra documents are needed.

| Participants per endpoint | Download, Mbit/s | Upload, Mbit/s | File per direction |
| ---: | ---: | ---: | ---: |
| 1 | 0.063 | 0.055 | 1 MiB |
| 2 | 0.112 | 0.107 | 1 MiB |
| 4 | 0.241 | 0.211 | 1 MiB |
| 8 | 0.704 | 0.265 | 1 MiB |
| 16 | 1.171 | 0.414 | 1 MiB |
| 32 | 1.070 | 0.368 | 1 MiB |
| 32, warmed up | 1.426 | 0.405 | 8 MiB |
| 32, four concurrent TCP files | 1.973 aggregate | 1.357 aggregate | 4 × 2 MiB |

Every table entry passed end-to-end SHA-256. The short runs were sequential,
not repeated randomized trials; warm-up and carrier conditions affect the
comparison. The 16-to-32 result does not establish a hard provider limit.
The 16/32-participant tests used runtime commit `af3677e` (SHA-256 `19817520...`).
The later Mail.ru key-refresh change does not alter Legacy. Earlier runs used
the intermediate hashes listed in the evidence.

The parallel run passed all eight file hashes: four downloads followed by four
uploads. It did not send both directions simultaneously. Aggregate rates use the
wall time for the complete group, not a sum of each stream's individual rate.

With 32 participants per endpoint, `curl --socks5-hostname` fetched the HTTPS HTML
of example.com, www.wikipedia.org and www.python.org with status 200 and normal
certificate verification. Total times were 1.018, 1.054 and 0.526 seconds.
This is a page-body smoke test, not a rendered browser test including images,
scripts and interactive applications. It supports trying this profile for light
browsing; video and heavy sites were not tested.

During load, the AWS test container used approximately 16% of one CPU with 16
participants and 33% with 32. Those are point samples, not average/peak guarantees.
At the latter sample its memory was about 15 MiB and Mini PC2's about 34 MiB.
Duplicate document broadcasts consume network and decoding work on each receiver;
physical transport counters therefore greatly exceed unique application bytes.
Thousands of users across a service do not establish the capacity of one document
with every participant continuously transmitting. This experiment bounds the
setting at 32 and does not attempt unbounded connection creation.

The four-packet batching trial stalled on upload and was stopped. Safe mode now
forces one packet per batch. The initial 45-second single-lane upload timeout
also failed; its bytes accepted by the local socket are not delivered throughput.
Both failed attempts are retained in the evidence rather than counted as passes.

### Mail.ru: baseline and final candidate

| Build / test | Download, Mbit/s | Upload, Mbit/s | Integrity |
| --- | ---: | ---: | --- |
| Installed v4.2.0, 16 MiB per direction | 28.797 | 2.539 | Both pass |
| Installed v4.2.0, 64 MiB capped at 512 KiB/s | 4.239 | 4.110 | Both pass despite reconnects |
| Intermediate candidate after manual key recovery, 16 MiB | 27.701 | 42.763 | Both pass |
| Final candidate with key refresh, uncapped 256 MiB | 21.787 | 6.092 | Both pass |

The final 256 MiB download took 98.569 seconds and upload 352.508 seconds, on
one TCP connection per file without retry/resume. No established Mail.ru socket
closed during those transfers. Initial connection attempts retried twice before
authorization; the counter therefore was not zero even though the established
connections remained stable. The baseline repeated close 1005 at roughly
62-second intervals on both peers. Its file hashes still passed, so the evidence
does not imply that every old-version reconnect destroys the inner TCP session.

The earlier 256 MiB attempt on the intermediate candidate failed with zero bytes
received because the peers had different editor keys despite identical public
URLs. Matching the keys restored transfer, and the final candidate adds periodic
key refresh. The failed attempt remains in the evidence. The automatic key-change
path is covered by a mock editor that changes rooms and verifies reauthorization;
the live key-mismatch diagnosis used a manually forced socket reconnect.

These are sequential, single-site observations, not randomized A/B trials.
Short and long file sizes differ, so 42.763 Mbit/s must not be presented as
sustained upload capacity or compared directly with the 256 MiB result.
The long upload is usable but remains substantially slower than the download;
this branch does not claim to restore a stable 18–29 Mbit/s upload rate.

The final recovery test closed two TLS sockets in the isolated exit container,
including the document WebSocket. The established document socket
reconnected in 1.458 seconds. The original TCP download completed all 32 MiB in
62.554 seconds with the expected SHA-256, with no HTTP retry/resume. The recorded
fault timestamp falls inside the transfer. This tests a brief external socket
failure, not an exit-process restart or an arbitrarily long network outage.

After testing, AWS's original Mail.ru and Yandex exit services were active again,
its Volga exit remained inactive, and all three installed Mini PC2 clients were
inactive as initially observed. Both installed executable hashes were unchanged.
No temporary test units/container/timers remained; both temporary directories,
including copied cookies and private logs, were removed. Mini PC1 was not changed.

Final measured results and machine restoration are recorded in the accompanying
[delta](deltas/OpenFlux_Delta_2026-10-11_transport-reliability.md) and
[JSON evidence](evidence/transport-reliability-2026-10-11.json).
A close reported as 1005 means the Close frame carried
no status code; it does not identify the provider's reason or prove a fixed TTL.
See [RFC 6455](https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1).

## Reproduce and hand off

The final Mail.ru runtime source is commit `72672c2ace08037c4a86bda0c00924db490ecfbd`.
Subsequent evidence/probe documentation changes do not change that executable.
The Linux amd64 candidate built without stripping has SHA-256
`9bb08cb236703e632f631da1598f78f2fa309b4f14ead6aff4653e289dc632d4`.
Legacy's live matrix used the otherwise identical Legacy code at `af3677e`, with
executable SHA-256 `19817520cf612eaaa17ad72290844a6baf7739eb4dd76aed0c3b9065cbe12117`.
Release/CI builds using different linker flags have different hashes.

For the measured Legacy pool, add the following to the usual runtime invocation
on **both** Mini PC2 and AWS, keeping their existing client/exit roles, URL and
cookie-store configuration:

```sh
--transport=yandex --yandex-safe-mode=true --yandex-lanes=32 --compression=true
```

Use one active mini-PC. `--yandex-lanes=16` is a lower-resource trial option;
its completed measurements are the short runs above. The router UI and generated
units do not automatically enable these experimental flags. A permanent rollout
must preserve them in both launch configurations; merging this branch alone does
not enable the pool. Keep the current executables/configuration as rollback
checkpoints and change both Legacy endpoints together.

Build the normal executable with Go 1.26.8:

```sh
GOTOOLCHAIN=go1.26.8 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -buildvcs=false -o openflux-candidate .
go test ./...
go test -race ./transport/mailru ./transport/yandex ./transport
go vet ./...
go test -tags volga ./transport/yandex
(cd volga && go test -tags volga ./... && go vet -tags volga ./...)
PYTHONPATH=integration python3 -m unittest discover -s integration/tests -q
```

Linux is required for the integration suite and cookie-file permission checks.
Two updater test fixtures now point `UNIT_DIR` at their temporary directory so
an already-installed Mail.ru systemd unit cannot contaminate the test scenario.

Start a temporary origin bound to a private address reachable from the isolated
exit, then probe the separate SOCKS listener. Do not expose the origin publicly.

```sh
python3 scripts/transport_probe.py origin --host PRIVATE_ORIGIN_IP
python3 scripts/transport_probe.py client --host PRIVATE_ORIGIN_IP --mib 16
python3 scripts/transport_probe.py client --host PRIVATE_ORIGIN_IP --mib 64 --rate-kib 512
# Legacy needs a timeout longer than a complete 1 MiB transfer at its low rate.
python3 scripts/transport_probe.py client --host PRIVATE_ORIGIN_IP --mib 1 --timeout 600
# Four simultaneous files through an already-running Legacy participant pool:
python3 scripts/transport_probe.py client --host PRIVATE_ORIGIN_IP --mib 2 --connections 4 --timeout 120
```

For the Mail.ru recovery case, transfer a 32 MiB download with `--rate-kib 512`
and `--direction download`. During that transfer, record a Unix timestamp and
close the TLS socket in the **isolated test exit container's** network namespace
(`nsenter --target TEST_EXIT_PID --net ss -K dport = 443`). Record the number of
sockets closed. The probe must complete on its original TCP connection with the
correct digest, and the fault timestamp must fall between its start and finish.
The experiment does not restart the exit process or its network namespace.

For an isolated lane experiment, build `./cmd/legacy-lane-probe`, stop the other
Legacy participants, and use the same future Unix `--start` on AWS (`--role 1`)
and Mini PC2 (`--role 2`). Pass the existing document with `--url` and a copied
cookie store with `--cookies`. A four-phase run takes about 200 seconds after its
start. It creates no document edits and never prints credentials.

For Claude/reviewer: inspect the measured results first, retain the distinction
between a carrier experiment and a functioning TCP tunnel, and review the opt-in
Legacy compatibility requirement. Merge/release/deployment is a separate action.
Before a release, extend full-rate simultaneous-direction and multi-stream load
testing, and test a longer carrier outage. This branch does not claim unlimited
uptime, a fix for every provider-side disconnect, or production rollout.
