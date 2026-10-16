# Mail.ru file API feasibility check, 2026-10-16

> Latest result: the complete raw-IP hybrid candidate failed the live tunnel
> acceptance test. The earlier file benchmarks below do not establish usable
> browser throughput. Do not promote v4.3.0-rc1; see the final section.

## Scope and result

Tested script commit: `0e5aa05ed3574a6ee5f2e9d5c50a6ff696729639`.
Two existing browser sessions for the same account exchanged random files via
the Cloud file API. Each received payload matched its SHA-256 and exact length.
This is evidence of sufficient short-run bulk throughput for a 2,000,000 B/s
target. It is not a working OpenFlux transport or a sustained throughput result.

No installed programs, service configuration, router mode, or production node
were changed. The test client stayed in AWG mode with OpenFlux clients and bridge
inactive. These were ordinary node HTTPS requests, not LAN traffic through
OpenFlux. Reachability under a restricted network was not tested.

## Procedure

The node-local script reads its existing browser profile into memory, bootstraps
CSRF, and obtains upload/download endpoints from `/api/v2/dispatcher`.
For each object: generate fresh random bytes, upload, register with `file/add`,
read metadata from the receiving node, download, verify, and remove the object.
Files have a unique random experiment prefix; no existing user file is modified.
No file is published. Account identities and cookies are not emitted.

One 64 KiB and one 16 MiB file were sent in each direction, sequentially, with
35-second operation deadlines and no automatic retry. A private SSH coordinator
sent names/checksums and commands only; payload bytes traveled through Mail.ru.
The script was committed and pushed before execution.

## Measurements

MB/s below means decimal megabytes per second. File sizes use binary units.
Elapsed time includes upload, registration, receiving metadata lookup, download,
hash verification and coordinator overhead, but excludes subsequent deletion.

| Direction | Payload | Elapsed | Effective MB/s | SHA-256 |
| --- | ---: | ---: | ---: | --- |
| Server to test client | 64 KiB | 1.766 s | 0.037 | Match |
| Test client to server | 64 KiB | 1.796 s | 0.036 | Match |
| Server to test client | 16 MiB | 2.015 s | 8.326 | Match |
| Test client to server | 16 MiB | 2.844 s | 5.899 | Match |

The large server-to-client transfer comprised 1.120 s upload, 0.248 s
registration, 0.146 s receiving metadata lookup, and 0.339 s download.
The reverse comprised 0.634 s upload, 0.681 s registration, 0.244 s lookup,
and 1.092 s download. Separate-phase rates must not be presented as tunnel speed.

## Limitations and interpretation

- All objects were visible on the receiver's first metadata request after the
  coordinator signalled successful registration. Visibility delay was not
  measured independently from that request's duration.
- SSH provided the test's control signalling. A standalone file transport still
  needs notification or polling, framing, acknowledgements and backpressure.
  Its extra overhead is absent from these measurements.
- Small-file delivery took about 1.8 seconds in each direction. Two dependent
  exchanges can therefore cost several seconds before any website latency.
  Bulk throughput alone does not meet the browser-responsiveness requirement.
- No simultaneous bidirectional load, long-run quota, streaming read before
  upload completion, outage recovery, or actual browser session was tested.
- The result does not establish the scope/cause of document-channel error 4004.

The first harness attempt uploaded 64 KiB but registration returned HTTP 403.
Adding browser CSRF/origin headers to API requests corrected this harness issue.
The failed registration's path was confirmed absent (404), followed by a
successful removal request. The successful run removed all four objects;
both workers reported zero pending objects and zero cleanup failures.
Unregistered uploader data, if retained internally by the provider, is outside
the account's file namespace and its retention was not inspected.

No release was produced. The next engineering question is low-latency control
and pipelined file blocks, rather than more document-channel timing changes.

## Follow-up: 1, 4 and 6 file lanes

Tested commit: `18d96b63f75eb289a35e4060a2d31dcafb246272`.
Probe source SHA-256:
`84e6b51d638889e48a91e9726e53f2646f1dfa5f2eccf3d1f56731abdc8a070d`.

The follow-up enabled thread-local persistent HTTPS connections and independent
receiver discovery. Before upload starts, the receiver knows only the experiment
prefix, sequence numbers, block count and expected size. It checks file metadata
and retries only a not-yet-created file (404), at 200 ms intervals after each
response, with a 25-second discovery budget. It does not retry 429 or other
provider failures. No per-file readiness or content travels through SSH.
Final hashes from both ends are compared by the coordinator after the batch.

Each lane uploads or receives its assigned sequence in order. Sending and receiving
overlap, but each file must finish uploading and registering before downloading.
There is no shared file overwrite, full tunnel implementation, or application
acknowledgement protocol. The experiment is bounded to 12 distinct objects per
bulk batch. Deletion follows completed verification and is outside the timing.

Each bulk run sent the same 12 fresh random blocks of 2 MiB: 24 MiB total.
One run per setting and direction was performed, in order 1, 4, 6.

| Concurrent file lanes | Server to client | Elapsed | Client to server | Elapsed |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 2.39 MB/s | 10.515 s | 3.49 MB/s | 7.219 s |
| 4 | 5.55 MB/s | 4.531 s | 7.32 MB/s | 3.437 s |
| 6 | 6.82 MB/s | 3.688 s | 6.71 MB/s | 3.750 s |

Four lanes improved observed throughput in both directions; six improved the
download direction further but did not improve the reverse direction in this
sample. These short, unrandomized runs do not establish an optimal lane count
or isolate concurrency from changing provider conditions.

Six separate 64 KiB transfers, three in each direction, measured latency:

| Direction | Samples | Median |
| --- | --- | ---: |
| Server to client | 1.328, 1.516, 2.328 s | 1.516 s |
| Client to server | 2.406, 1.672, 1.360 s | 1.672 s |

The persistent-connection client did not solve interactive latency. Small-block
tests used one lane and a fresh batch worker; latency under simultaneous bulk
traffic was not measured. Compared with the earlier test, discovery is now part
of the path and block sizes differ, so its 16 MiB rates are not a direct baseline.

All 78 files (144.375 MiB total unique payload) passed SHA-256/length checks.
Receivers made 124 metadata requests, including expected not-yet-ready replies.
No completed matrix batch reported an error. Every test object was removed and
both workers reported zero pending objects / cleanup failures. Installed services
were unchanged: the server's normal Mail.ru service remained active; test-client
OpenFlux services and bridge remained inactive. The client remained in its
pre-existing AWG mode; restricted-network reachability is still unverified.

Before the matrix, two 64 KiB setup attempts stopped on the downloader redirect
handling (HTTP 301, then a rejected destination domain). A separate 64 KiB probe
confirmed the HTTPS redirect from `*.cloud.mail.ru` to `*.datacloudmail.ru`.
The reader now permits that observed download domain, limits redirects to three,
rebuilds destination cookies and strips cross-host CSRF headers. All setup probe
objects were also removed. These attempts were harness corrections, not capacity
results. The additional download domain matters for future reachability tests.

The evidence supports a bounded prototype with 4-6 bulk file lanes. Fast handling
of small requests/replies still needs a separate design and validation. No release
or router-mode change is justified by this benchmark alone.

## Hybrid proof: document chat control and Cloud file payloads

Tested commit: `14433761244bb457df2b07b02752a88ccf4d8600`.
The isolated Python helper is `scripts/mailru_hybrid_probe.py`.

The existing installed Mail.ru transport carried one persistent TCP control
connection through its SOCKS endpoint. Its destination was an ephemeral test
listener bound only to the host's private Docker bridge, with an experiment
nonce handshake. No public listening address or firewall change was used.
The server's normal Mail.ru service continued running. The test client's existing
Mail.ru service was temporarily started under a four-minute rollback timer;
router mode stayed AWG and the OpenFlux router bridge stayed inactive.

The first control test targeted loopback inside the exit namespace and timed out.
Both document sessions were ready; that attempt does not establish a chat
failure. After correcting the probe target to the routable private bridge,
the control connection and the whole hybrid exchange completed.

Procedure on one control connection:

1. Exchange 12 tiny ping/pong messages at 500 ms intervals.
2. Request a download through the control connection.
3. Server uploads eight fresh random 2 MiB files through four parallel lanes.
4. For each registered file, server sends its name, size and SHA-256 through
   document chat. The client starts its download on receiving this notification.
5. Client verifies every file, acknowledges completion through chat, and server
   deletes the eight objects and acknowledges cleanup.
6. Exchange 12 more ping/pong messages, then close the connection.

SSH only launched the two test processes and collected measurements. File
readiness, hashes, requests and completion acknowledgements traveled through the
existing Mail.ru document chat tunnel. There was no per-file SSH signalling.

| Measurement | Result |
| --- | ---: |
| Control RTT median before payload | 123.7 ms |
| Control RTT maximum before payload | 330.8 ms |
| Download, 16 MiB end-to-end | 4.386 s |
| Effective download throughput | 3.825 MB/s |
| Control RTT median after payload | 125.0 ms |
| Control RTT maximum after payload | 253.9 ms |
| File hashes / sizes verified | 8 / 8 |
| Outstanding test objects after cleanup | 0 |

The download clock starts before its request is sent through chat and ends after
all eight files are received and verified. Cleanup follows and is not included.
The existing server reconnect counter stayed at five; the test client stayed at
zero, with no reported document drops during the observation window. At the end,
the test client was stopped, its rollback timer cancelled, and AWG mode confirmed.
The server's normal service remained active. The production client was untouched.

This establishes a working short hybrid exchange above the 2 MB/s bulk target,
with low-latency control on either side of the transfer. It does not demonstrate
interactive requests during bulk load, bidirectional payloads, long-session
stability, restart recovery, or browser traffic through a complete hybrid tunnel.
The 24 RTT samples contain only tiny synthetic messages, not arbitrary website
requests. The document chat's earlier sustained-load failures remain relevant;
the small-message budget and recovery behavior still need an explicit design.

No production transport implementation or release was produced by this proof.

## Complete raw-IP hybrid candidate: acceptance failed

Source: `f41d20fb1881223a6b11edb5a6945ae9db24616a` on
`codex/mailru-hybrid`. CI passed Go tests, race tests, Linux integration tests,
builds and container smoke tests. The exact CI binary was launched temporarily
on the test client and server, with rollback checkpoints. Installed files and
router mode were preserved. The production client was untouched.

The candidate carries TCP setup, pure ACKs and the first 4 KiB per flow in chat.
Other raw IP packets are collected for up to 80 ms into files of at most 512 KiB,
with four upload/download workers. File notices and acknowledgements use chat.

An initial implementation failed uploads because host-only cookies were not
available on the upload subdomain and successful HTTP 201 responses were
rejected. A bounded comparison observed HTTP 403 without authentication and
HTTP 201 with it. Both defects were fixed in Git, with regression tests for
upload registration and cookie scoping; a new CI binary was then tested.

| Live check | Result |
| --- | --- |
| HTTPS, 32 KiB | HTTP 200, complete, 1.235 s |
| Public bulk-test endpoint | HTTP 403; not a throughput measurement |
| Private server, 16 MiB random download through hybrid SOCKS | Timed out at 40 s; only 199,824 bytes received, about 0.005 MB/s |
| Concurrent ordinary HTTPS page | HTTP 200, complete, 1.327 s |
| Registered / received file blocks | 33 / 33 |
| Document reconnects during the observation | 0 |

The private bulk source bound only to the server's Docker bridge, not a public
interface. Random payload and readiness traveled through the tested tunnel;
SSH only launched helpers and collected results. The incomplete bulk payload
failed the expected full-file hash check. No LAN browsing or router-mode
acceptance test was attempted after this failure.

During bulk traffic, registration and deletion began returning non-JSON API
responses. A subsequent single bootstrap check returned HTTP 200 HTML containing
`robot` and `too many`, without the usual CSRF data. The exact rate threshold,
scope and expiry of this provider restriction were not established. This must
not be reported as an ordinary cookie expiry or a numeric Cloud bandwidth cap.

The evidence is consistent with excessive API operations caused by many small
TCP-driven blocks. Unlike the previous prefilled 2 MiB file experiment, raw TCP
depends on acknowledgements across the file path and does not automatically
fill large blocks. Increasing the worker count alone is not justified.

The next design needs a connection/stream relay that separates Internet TCP
flow control from reliable, ordered, bounded file delivery, aggregates bulk
data into large objects, and stops API traffic on provider rejection. This is
an architectural proposal, not a validated fix or a speed guarantee.

Temporary transports and rollback timers were stopped. The test client remained
in AWG with OpenFlux clients and bridge inactive. Five remaining test objects
were removed through the test-client session; the dedicated Cloud folder was
verified empty. The previous server binary was restored, but its Mail.ru login
was still blocked by the provider at the first post-test check. The candidate
must remain unpublished; restoring an installed version is not proof of
provider-side recovery.
