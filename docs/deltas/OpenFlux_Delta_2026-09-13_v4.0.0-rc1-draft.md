# OpenFlux Delta - 2026-09-13 - v4.0.0-rc1 draft created

## Date/time

2026-09-13

## Machines affected

None. GitHub repository and registry only.

## Reason

Complete the first private downstream release pipeline with a containerized exit-node artifact while keeping the currently deployed Yandex v4 production unchanged.

## Repository changes

- PR #2 merged into `main`
- release infrastructure source commit: `f6b64cb20157baac8e6d2c3f566719ecbba982ca`
- added `Dockerfile.release`
- added `.dockerignore`
- added container CI
- added draft private release workflow
- added `release/VERSION = v4.0.0-rc1`
- added release model documentation

## Release result

Draft prerelease `v4.0.0-rc1` created successfully.

Native binary SHA256:

`20a881921fc9c5ad6dca0b7e0a8f70869ae4ce4a07cc8a2876f950fd21c1bd06`

Container archive SHA256:

`72fb6a3b403a775b8326d2c22e25fbf14ba46c997bc8e0a1480666abcb207e0a`

GHCR image:

`ghcr.io/levl-max/openflux-mod:v4.0.0-rc1`

Registry digest:

`sha256:cdc1023c6c94b7c985e9de46c561bdfce0c496c12cfdf5cb6fa999ca118ba0ea`

## Validation actually passed

- existing Go CI passed on PR and `main`
- `go test ./...` passed
- static Linux amd64 release binary build passed
- Docker runtime image build passed
- container `--help` entrypoint smoke test passed
- GHCR authentication and push passed
- immutable version/SHA tags published
- draft prerelease asset upload passed

## Production decision

No production deployment. No service restart. No AWS, Mini-PC1 or Mini-PC2 change.

Current production remains the historical binary SHA256:

`6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

## Functional decision

Keep the current private Yandex v4 feature set. Do not add Volga, CUPS or other experimental transports merely because they exist upstream. Review/backport only when there is a concrete reliability, compatibility or operational benefit.

## Rollback checkpoint

Not applicable to production because nothing was deployed. Repository release infrastructure can be reverted independently if needed.

## Upstream review cursor

`6443d42ac3fe643e702c8774bc4508e2c5ec222c`

## Superseded artifacts

None in production.

## Unresolved items

- review draft release and metadata
- isolated live runtime test of RC container before any production change
- after isolated approval, adapt AWS updater to consume exact approved private GitHub release/container digest while keeping checkpoint, health-gate and rollback logic
- adapt Mini-PC distribution afterward using the approved downstream native binary asset
- publish/promote the draft RC only after review; draft creation itself is not approval

`CURRENT ACTION = REVIEW AND ISOLATED-TEST V4.0.0-RC1 - PRODUCTION NO CHANGE`
