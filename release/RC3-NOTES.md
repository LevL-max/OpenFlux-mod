# OpenFlux-mod v4.0.0-rc3

Release candidate focused on Yandex SmartCaptcha recovery and persistent browser-auth state.

## What changed

- Added automatic first-tier `showcaptchafast` PoW handling.
- Added persistent per-document Yandex cookie storage via `--yandex-cookie-store`.
- Kept `--yandex-cookie-file` as an optional one-time browser cookie seed.
- Cookie state is saved atomically with mode `0600`.
- Added explicit detection of second-tier SmartCaptcha / login walls.
- Added `AUTH_BLOCKED`: once second-tier SmartCaptcha is detected, OpenFlux stops retrying Yandex instead of polling every 30 seconds.
- Added runtime cookie-store change detection. When fresh browser cookies are imported, OpenFlux reloads the store and reconnects without a service restart.
- Added `openflux-yandex-cookie-import` helper for importing Chrome/Chromium “Copy as cURL (bash)” cookies without printing cookie values.
- Kept the proven private v4 behavior unchanged: dynamic public Yandex bootstrap, dynamic OnlyOffice build detection, modern Engine.IO/Socket.IO authentication, Yandex `editorConfig.user.id`, 4-packet / 1 ms batching, compression, TCP buffers, reconnect/backoff+jitter and explicit WebSocket close hardening.

## Validation

- GitHub CI: PASS
- Container CI: PASS
- Exact release binary SHA256: `12f5911ee8904987c5f64592f63c689bf6a3073f40d7dc8d736ba8e3c6db1913`
- AWS isolated preflight: first-tier PoW -> second-tier SmartCaptcha -> `AUTH_BLOCKED` -> cookie-store replacement -> runtime reload -> OnlyOffice authentication: PASS
- AWS production rollout of the exact RC3 binary: PASS
- AWS production service remained connected after rollout without additional reconnect growth.
- Mini-PC rollout is intentionally pending until this prerelease is published and consumed through the normal updater path.

## Release assets

- `openflux-linux-amd64`
- `openflux-container-linux-amd64.tar.gz`
- `openflux-yandex-cookie-import`
- `SHA256SUMS`
- `BUILD-METADATA`

This is a prerelease. It is intended for controlled rollout and validation before promotion to a stable release.
