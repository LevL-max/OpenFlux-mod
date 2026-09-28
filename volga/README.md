# Volga runtime

Separate module from lab `c630641933c19fdf250c9c13ffaef6fee6e80be3`.
Build/test here with `go build -tags volga ./cmd/openflux-volga-lab`,
`go test -race -tags volga ./...` and `go vet -tags volga ./...`.
The historical command directory name is retained; the release executable is
`openflux-volga-linux-amd64`, reporting `openflux-volga <release version>`.
Without the build tag this transport is excluded from the Legacy executable.

The [frozen profile](../docs/FROZEN-PERFORMANCE-PROFILE.json) uses four lanes over
two documents, shared 480 POST/s, 1 MiB record window, 5600-byte records, 1 ms
coalescing and 64 workers. No performance tuning is part of this integration.
See [configuration/deployment](../integration/README.md).

The server validates every DNS answer, dials numeric addresses, and denies
local/private/special-use addresses and local interface subnets. Managed
container setup adds host interface subnets to `denied_cidrs`; add any other
internal globally routed networks explicitly. Literal-IP test-origin exceptions
remain available in the raw lab; the production adapter rejects them.

SOCKS supports CONNECT, half-close, bounded idle cleanup and at most 64 streams.
UDP ASSOCIATE/BIND are unsupported. 64 Yamux streams can retain about 128 MiB
of unread payload plus runtime/buffer overhead. Managed defaults are 256 MiB and
50% of one CPU; lower `max_streams` if necessary.

Both sides need matching documents and a random 32-byte shared key. Credentials
are separate from Legacy. Startup CAPTCHA/login rejection waits for a local
credential change; active sessions reload cookies during authorization. Browser
profile changes rebuild the session without restarting the process. Interactive
browser verification still requires the user to complete it and import cookies.

The helper in `cmd/volga-lab-check` supports short functional integrity/concurrency
checks. [Lab results](../docs/VOLGA-LAB-ROUTER-2026-09-28.md) do not substitute for
validation of the final RC deployment.
