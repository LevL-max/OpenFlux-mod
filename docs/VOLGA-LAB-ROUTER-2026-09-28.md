# Router runtime: public egress and bounded concurrency

Base: `v0.1.3-packing@ab7a7f319cb4e810f6929f55ded979327c161f18`.
Tested version: `0.1.3-router1-experimental`. This is a lab change, not a
production release. [Machine-readable evidence](ROUTER-RUNTIME-2026-09-28.json).

## Changes

- `egress_policy: public` permits public TCP destinations, denying private,
  loopback, link-local, metadata, multicast, special-use/transition addresses and
  every current interface subnet. `denied_cidrs` adds deployment-specific denies
  and overrides exceptions. Internal globally routed networks hidden behind a
  container namespace must be supplied explicitly.
- DNS answers are validated together before any connect. Mixed public/private
  answers fail closed; the dialer uses the validated numeric IPs without a second
  lookup. Two bounded address-family racers provide a 250 ms IPv4/IPv6 fallback.
- Exact literal-IP `allowed_targets` exceptions remain available for the test
  origin. Omitted policy retains mandatory allowlist mode, including its existing
  hostname entries. An unavailable local-interface inventory denies access.
- `max_streams` accepts 1–64 and defaults to 64. Admission is enforced at SOCKS,
  endpoint and remote server; slots are released after cleanup. A tracked-stream
  fuse and bounded accept backlog limit half-closed excess streams from a
  noncooperating authenticated peer.

Special-use exclusions were checked against the primary
[IANA IPv4 registry](https://www.iana.org/assignments/iana-ipv4-special-registry/)
and [IANA IPv6 registry](https://www.iana.org/assignments/iana-ipv6-special-registry/).
The policy intentionally excludes some special anycast ranges too.

The unchanged 2 MiB Yamux credit allows 128 MiB of unread payload across 64 active
streams, plus approximately 4 MiB bridge buffers. Closing streams, Go, TLS and
carrier overhead are additional. Stream admission bounds concurrency; it is not
an RSS cap. Service/container limits are still required.

## Validation

| Check | Result |
|---|---|
| Lab `go test -race ./...` | PASS, including address policy, rebinding/mixed DNS, both blackholed-family fallbacks, 64-stream admission, server-independent limit and slot release |
| Lab `go vet ./...` | PASS |
| Unchanged transport/yandex race tests and vet | PASS on Windows with only the POSIX-permissions test excluded |
| `TestLoadBrowserCookiesPersistsAndReloads` | PASS separately on Linux VPS; Windows reports mode 0666 instead of 0600 |
| Frozen performance file | Unchanged Git blob `09daf0229d395ad4b39dea82446a33c9d1fc82b4` |
| Nested transport snapshot | All 118 files byte-identical to base; no Legacy/master/production modifications |

Go 1.26.4; Linux amd64 builds use CGO=0 and
`-buildvcs=false -trimpath -ldflags='-s -w -buildid='`.
Runtime SHA-256: `237983a4f4ebbcd192d6af33ee867f91f2caa185c2d1d5bda69645bc298e3810`.
Helper SHA-256: `3b81154e90b78b14c73a5a3eaf3af1593c520e0c0e86b8a206dc3b9c0e5dc40f`.

## Short live functional check

September 28, final transfer events 09:23:57–09:24:04 UTC, two isolated VPS units.
Four lanes; frozen shared 480 POST/s, 1 MiB window, 5600-byte records, 1 ms
coalescing, 64 send workers, `per_lane_budget=false`. No speed score or tuning.

| Check | Result |
|---|---|
| 64 simultaneously held SOCKS connections | PASS |
| 65th connection rejected while all slots held | PASS |
| 32 downloads + 32 uploads, 128 KiB each | 64/64 SHA-256 match; 8 MiB verified |
| Unlisted public `https://example.com/` | HTTP 200, TLS certificate verified, 559 bytes |
| Loopback, metadata, RFC1918, mapped IPv4 loopback | All explicitly rejected with SOCKS status 2 |
| TCP half-close | PASS |
| New connection and integrity after aborted transfer | PASS |
| Sampled POST errors / HTTP 429 / WS reconnects / handoffs | 0 / 0 / 0 / 0 in final run |

Repairs remained active (final snapshots: server 29, client 38). This is normal
record repair accounting, not an integrity mismatch. The lifecycle helper also
performs its existing 2 MiB recovery download, separate from the 8 MiB matrix.

Two earlier functional failures were fixed before acceptance:

1. All 64 hashes passed, but public HTTPS was denied because the old test unit's
   address-family allowlist omitted AF_NETLINK. An isolated comparison confirmed
   interface enumeration failed without it and succeeded with it. Deployment
   units must permit AF_NETLINK for reading interfaces; no network-administration
   capability is needed. The policy still fails closed.
2. After that unit correction, hashes passed but HTTPS timed out: direct IPv4
   HTTPS worked, while the VPS IPv6 route timed out. Sequential numeric dialing
   exhausted the setup budget on IPv6. Bounded parallel family fallback fixed it;
   regression tests cover both directions and cancellation of the losing dial.

One intervening idle preflight observed a carrier handoff and was stopped by the
strict monitor before launching the probe. It is retained as a limitation, not
counted as a passing transfer run. The final run had no handoff. The initial
Linux permission-test invocation also hit `/run` noexec; execution from the owned
`/opt` test directory passed. No production settings were changed for these fixes.

## Resources and cleanup

Each runtime: CPUQuota=50%, MemoryMax=256M, MemorySwapMax=0, GOMEMLIMIT=160MiB,
GOMAXPROCS=1. Origin and probe had separate bounded units. Sampling every 0.2 s.

| Runtime measurement | Server | Client |
|---|---:|---:|
| Process peak RSS (`VmHWM`), MiB | 71.33 | 72.63 |
| Largest sampled cgroup memory, MiB | 69.25 | 70.89 |
| Sample interval length, seconds | 20.10 | 7.04 |
| Process CPU seconds within interval | 1.73 | 1.54 |
| Mean CPU over interval, % of one core | 8.61 | 21.87 |
| Peak sampled CPU, % of one core | 54.77 | 54.77 |
| `nr_throttled`, unit lifetime | 17 | 17 |
| `nr_throttled`, observed interval delta | 16 | 17 |
| `throttled_usec`, unit lifetime | 718647 | 432460 |
| OOM / memory-limit hits | 0 / 0 | 0 / 0 |

Server sampling continued after completion, so its mean includes more idle time
and is not comparable to the client's active-interval mean. CPU tick granularity
and quota periods permit a sampled peak slightly above 50%. Kernel `memory.peak`
was unavailable; cgroup values are sampled maxima, while VmHWM is a process high
water mark. These short checks do not establish worst-case home-network memory
requirements or long-duration stability. Retain hard limits during integration.

Both VPS test units, timer, directories, binaries and private credentials were
removed; SSH closed. Existing service PIDs/start times, containers, TCP listeners,
routes, rules and static firewall matched the pre-test snapshots. Dynamic UDP
ports and fail2ban entries changed independently during the session.

The speed series remains closed. This acceptance covers the two runtime changes
and ordinary concurrent TCP/SOCKS behavior, not a six-hour soak, cookie-expiry
exercise, transparent router deployment or a production release.
