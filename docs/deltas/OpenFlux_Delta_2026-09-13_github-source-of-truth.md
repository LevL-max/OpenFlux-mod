# OpenFlux Delta - 2026-09-13 - GitHub source-of-truth migration

## Scope

GitHub repository and CI only. Production hosts were not changed.

## Reason

Move OpenFlux development from machine-local source trees to a private downstream GitHub repository while preserving the exact historical v4 ancestry and keeping public upstream as review input only.

## Canonical source imported

- AWS source tree: `/opt/openflux/src/openflux-yandex`
- canonical bundle SHA256: `c42fe84bc5a9a77b510e88685f3c24957f4ab1bc8b41d363586c45412ab3be20`
- imported production commit: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- imported production tag: `yandex-working-v4`
- imported historical private tags: `yandex-working-v1` through `yandex-working-v4`
- GitHub immutable bootstrap branch: `canonical-v4`

## Upstream tracking

- branch: `upstream-main`
- reviewed cursor imported: `6443d42ac3fe643e702c8774bc4508e2c5ec222c`
- public upstream remains `p1neappleXpress/OpenFlux`
- upstream must never deploy directly to production

## GitHub validation

- `go test ./...`: PASS
- Linux amd64 build: PASS
- artifact upload: PASS
- exact-source reproduction workflow checks out `22f29ab...`: PASS

Recorded build SHA256 values:

- integration build: `a2c24c4a1a06512264c11cadf7b4bee2da49c0041c6a7b595a460091063622ac`
- exact-source v4 reproduction build: `a9502869305c34f3f7a71bccc2cd991f582aa7aad8dacfcfc39fae3a5c01d1e9`
- historical production binary: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

The imported source history is exact. The production binary is not yet reproduced bit-for-bit by the GitHub build recipe, so the old production binary remains authoritative until the build recipe/release path is finalized and tested.

## Production impact

None.

No AWS service restart, Docker restart, router mode change, Yandex config change or Mini-PC change occurred.

## Rollback

Not applicable to production. GitHub bootstrap references remain preserved in `canonical-v4`, historical tags and the earlier bootstrap branches.

## Next

1. Promote validated imported-history branch to GitHub `main`.
2. Finalize private release workflow.
3. Adapt AWS updater to consume an explicit approved GitHub release artifact while preserving stage/health/rollback.
4. Define read-only private release access for Mini-PC distribution.
5. Remove temporary bootstrap write deploy key.

## Current action

`CURRENT ACTION = GITHUB REPOSITORY PROMOTION ONLY - NO PRODUCTION CHANGE`
