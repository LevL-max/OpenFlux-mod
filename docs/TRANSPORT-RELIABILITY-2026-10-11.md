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

Experimental `--yandex-lanes=2..8` adds independent Legacy participants on the
same document. It requires safe mode and matching settings on both peers. An
`OFP1` envelope carries a random sender epoch and sequence number: receivers drop
their own broadcasts and duplicates seen on the other connection. Replay memory
is bounded to eight epochs with 4096 sequence slots each. Sends alternate between
ready participants and can use the remaining participant during a reconnect.
This provides neither packet acknowledgments nor lossless replay; TCP still
handles packet loss. All participants share a serialized cookie store so they
cannot race its temporary-file/rename transaction. The default is one participant.
The extra 20-byte stripe envelope still fits below the 2600-byte serialized cap.

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

Final measured results and machine restoration are recorded in the accompanying
delta and JSON evidence. A close reported as 1005 means the Close frame carried
no status code; it does not identify the provider's reason or prove a fixed TTL.
See [RFC 6455](https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1).

## Reproduce and hand off

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
```

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
