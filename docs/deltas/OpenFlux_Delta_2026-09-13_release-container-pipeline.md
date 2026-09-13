# OpenFlux Delta - 2026-09-13 - GitHub release container pipeline

## Date/time

2026-09-13

## Machines affected

None. Repository-only work.

## Reason

Prepare the private GitHub repository to produce an immutable downstream release candidate with a containerized exit-node artifact while preserving the current stable Yandex v4 production line.

## Files/scripts changed

Planned repository additions on release preparation branch:

- `Dockerfile.release`
- `.dockerignore`
- `.github/workflows/container-ci.yml`
- `.github/workflows/release.yml`
- `release/VERSION`
- `docs/RELEASE_MODEL.md`
- this Delta

## Old production commit/SHA

- canonical production source ancestor: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- historical production binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

## New production commit/SHA

None. Production is unchanged.

## Exact production decision

Keep current Yandex v4 production untouched. Do not add Volga, CUPS or other experimental transports. Prepare `v4.0.0-rc1` as a GitHub draft prerelease only.

## Validation required

- `go test ./...`
- static Linux amd64 release build
- Docker runtime image build
- container entrypoint smoke test
- immutable SHA256 metadata
- no secrets/runtime Yandex session data baked into the image
- no automatic production deployment

## Rollback checkpoint

Not applicable to production. Repository branch can be discarded if CI fails.

## Upstream review cursor

`6443d42ac3fe643e702c8774bc4508e2c5ec222c`

## Superseded artifacts

None in production. Historical AWS-direct Mini-PC update-channel experiments remain ABANDONED / DO NOT USE.

## Unresolved items

- validate the release container in GitHub CI
- confirm GHCR publishing and draft release creation after merge
- isolated runtime test of the GitHub release artifact before any production deployment
- later adapt AWS updater to consume approved private GitHub releases
- later adapt Mini-PC distribution to the same approved release channel

`CURRENT ACTION = PREPARE AND VALIDATE V4.0.0-RC1 CONTAINER RELEASE PIPELINE - NO PRODUCTION CHANGE`
