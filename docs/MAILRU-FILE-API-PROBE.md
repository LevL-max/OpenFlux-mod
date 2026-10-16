# Mail.ru file API feasibility check, 2026-10-16

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
