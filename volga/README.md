# OpenFlux Volga SOCKS lab

Standalone experimental client/server, version `0.1.3-review1-experimental`.
It is **not** an OpenFlux production release. The v4.0.5 CLI, configuration,
Legacy transport and its cookie implementation are unchanged.

The byte-stream carrier follows the September 26 feasibility test: four
independently authorized Volga sessions, a shared 360 POST/s ceiling, 5000-byte
V6 batches, HTTP/1.1, minimal envelopes and a 512 KiB stream receive window.
This build adds authenticated/encrypted records, bounded reordering and credits,
Yamux multiplexing, SOCKS5 CONNECT, TCP half-close and session restart handling.

## Configuration

Use a separate private JSON file on each machine. Both sides need the same
random 32-byte `shared_key`, encoded as 64 hex characters. The example placeholder
is intentionally rejected. Never reuse a production configuration file.

```json
{
  "protocol": "volga-stream-v1",
  "role": "client",
  "documents": ["DOCUMENT_A", "DOCUMENT_B", "DOCUMENT_A", "DOCUMENT_B"],
  "cookie_store": "/private/volga/cookies.json",
  "browser_profile": "/private/volga/browser.json",
  "shared_key": "REPLACE_WITH_64_RANDOM_HEX_CHARACTERS",
  "listen": "127.0.0.1:1088",
  "idle_seconds": 30,
  "allowed_targets": [],
  "posts_per_second": 0,
  "per_lane_budget": false,
  "record_chunk_bytes": 0,
  "record_window_bytes": 0,
  "flush_millis": 0,
  "send_workers": 0
}
```

`posts_per_second` (optional, default 360) and `per_lane_budget` (optional,
default false) are throughput A/B knobs; see "Relay pacing" below. Absent or
zero keeps the validated shared-gate behaviour.

`record_chunk_bytes` (default 5600), `record_window_bytes` (default 512 KiB),
`flush_millis` (default 1 ms; a negative value disables coalescing) and
`send_workers` (default 64) are stream-shape knobs. They exist so a controlled
run can separate a provider limit from a local bottleneck: the sender credit
window, the record size and the send concurrency are all things that can cap
throughput below the relay's own limit. A receiver accepts any peer window up to
8 MiB, so the two sides need not match; each sender self-limits to its own
window. Absent or zero keeps the validated defaults.

On the server set `role` to `server` and supply the exact permitted `host:port`
values in `allowed_targets`. An explicit allow-list is mandatory in this lab.
The test helper uses `127.0.0.1:18765` for HTTP and `127.0.0.1:18766` for half-close.
Client SOCKS binds only to loopback. BIND and UDP ASSOCIATE are not implemented.
At most 16 concurrent streams are admitted. Setup is bounded to 10 seconds,
configurable activity timeout is 5–60 seconds, and Yamux half-close retention
is bounded to 60 seconds.

Version 0.1.1 gives each Yamux stream a 2 MiB receive window, while retaining
the 512 KiB carrier window. Yamux sends credit after half of its stream window
has been consumed; matching the two windows at 512 KiB caused avoidable stalls
in the paced local comparison. Payload storage across 16 streams is bounded
at 32 MiB per peer.

## Relay pacing and the throughput ceiling

The carrier is POST-rate bound: on an active one-way transfer the sender runs
close to the shared 360 POST/s ceiling, and each POST carries exactly one
`recordconn` record. Throughput is therefore (posts/s) x (useful bytes/post).

Version 0.1.2 raises the record `Chunk` from 4700 to 5600 bytes. One full
record still fits one relay POST: the minimal relay body grows from ~6476 to
~7676 bytes, safely under the 8000-byte limit and the observed ~8294-byte HTTP
413 point. This adds ~19% useful bytes per POST with no extra requests and no
change to the POST rate, so it does not raise HTTP 429 risk.

Two optional knobs let the next controlled run test the POST budget itself:

- `posts_per_second` sets the pacing rate (default 360).
- `per_lane_budget` (default false) gives every document lane its own pacing
  gate and its own HTTP 429 cooldown instead of one shared gate. The aggregate
  rate then becomes `posts_per_second x lanes`.

The shared 360/s gate is the known-safe configuration validated live with zero
HTTP 429. `per_lane_budget` is the lever for pushing aggregate throughput past
that ceiling toward 20-50 Mbps, but it **must be validated live for HTTP 429**
before any wider use: watch the `http_statuses` field of the periodic status
event, which aggregates the 429 count across all lanes. Do not raise the
aggregate rate without that measurement.

The cookie store uses the v4.0.5 document-URL-to-cookie-map format. The optional
browser profile has `headers` and `editors` maps. Only approved browser headers
accompany editor requests; account cookies are not forwarded to the Volga host.
Each physical lane obtains fresh bootstrap/session authorization. A login or
CAPTCHA rejection requires updated browser cookies; this build does not solve
interactive CAPTCHA challenges. Keep these files out of source control and
restrict their permissions (0700 directory, 0600 files on Linux).

```sh
openflux-volga-lab -config /private/volga/client.json
```

After a session failure, existing TCP connections close and subsequent SOCKS
requests use a fresh authenticated epoch. Interrupted application transactions
are not resumed transparently. Previously used peer epochs are rejected; after
64 session restarts the process stops with an explicit error.

## Build and local validation

Requires Go 1.26.4 and the sibling `../openflux-volga-405` source directory.
Yamux is pinned to v0.1.2 in this separate module; release go.mod/go.sum stay
unchanged. From this directory:

```sh
go test -race -timeout=90s ./...
go vet ./...
go build -trimpath ./cmd/openflux-volga-lab
go build -trimpath ./cmd/volga-lab-check
```

Local tests cover reordered/duplicated record delivery, bounded unread-peer
backpressure, authentication, cancellation and deadline changes, parallel HTTP
integrity, fragmented SOCKS negotiation, rejected targets/methods, half-close,
idle cleanup and fresh connections after a Yamux session failure. Local test
timings do not measure Yandex relay performance.

## Bounded live check

The helper origin listens only on loopback. Start it only on an isolated test
server, then run the helper through the test client's SOCKS listener:

```sh
volga-lab-check -mode=origin
volga-lab-check -mode=bench -proxy=127.0.0.1:1088
```

The benchmark checks a 50 MiB download, a 50 MiB upload and two simultaneous
25 MiB downloads. Every completed transfer is SHA-256 verified. Each direction
and the parallel aggregate must exceed 10 Mbps. Two concurrent streams share
the channel; each stream's speed must not be confused with the aggregate.
It also checks a response after TCP half-close and a fresh connection after an
aborted download. `-mode=lifecycle` runs only the latter short checks.

`-mode=endurance` performs exactly six alternating 50 MiB downloads/uploads
(300 MiB combined, 150 MiB per direction). It retains one HTTP/SOCKS connection,
records connection reuse and cumulative verified bytes after each round, and
stops on a transfer below 10 Mbps, an integrity error or a replaced connection.
It does not restart the Volga peers. Run it with an already running test origin:

```sh
volga-lab-check -mode=endurance -proxy=127.0.0.1:1088
```

Do not install this as a production service yet. A short successful test does
not establish long-duration stability or automatic recovery from every possible
provider authorization failure.

## Review and live evidence

See [v0.1.3-review1 review](../docs/REVIEW-v0.1.3-review1.md) for the coalescing failure-path fixes, regression tests, and two short live benchmarks. The default shared budget remains 360 POST/s; no production release is implied.
