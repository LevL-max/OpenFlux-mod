# Volga integration notes

Branch: `feature/volga-integration`

Upstream source reviewed against `p1neappleXpress/OpenFlux` main at `3fefc533a8115d1048c323b1ed455fc7e603ceb5`.

## Selected upstream scope

- `transport/yandex/vyandex.go` from upstream Volga implementation. The file has not changed upstream since commit `2af94abbf17d811eee13d4f78e5dc95eb9fa1dac`.
- `vyandex` selector wiring equivalent to upstream commit `707602826920836be4d2a30c5958400535b80123`.

## Intentionally not imported

- upstream Yandex reconnect timing changes from `4513a14e704b0e5697be4966e4898ac5dcc6eb16`
- AES-256-GCM transport wrapper
- CUPS transport
- upstream proxy/raw exit-mode refactor
- upstream Docker/compose packaging
- any production deployment changes

## Downstream compatibility choices

- Existing legacy `yandex` behavior and private RC2 runtime profile remain unchanged.
- Existing legacy Yandex external batching remains scoped to `transport=yandex`.
- `transport=vyandex` uses Volga's own upstream internal batching defaults: 20 packets / 2 ms.
- Existing downstream compression flag applies to both `yandex` and `vyandex`.
- Existing downstream gVisor TCP buffer flags continue to apply at the tunnel layer.

## Validation gates

1. `go test ./...`
2. Linux amd64 build
3. container build/smoke test
4. isolated live Volga auth on a non-production Yandex test document
5. isolated remote client -> Volga -> staging exit -> Internet
6. controlled reconnect/recovery test
7. throughput/error counters review
8. no production deployment until separately approved
