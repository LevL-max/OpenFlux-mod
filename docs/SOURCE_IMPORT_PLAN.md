# Canonical source import plan

## Goal

Make `LevL-max/OpenFlux-mod` the authoritative source repository without reconstructing private v4 from chat memory.

## Authoritative pre-GitHub source

AWS canonical tree:

`/opt/openflux/src/openflux-yandex`

Verified production state before import:

- HEAD: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- tag: `yandex-working-v4`
- working tree: clean at export time
- production binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

## Canonical bundle

Exported file:

`openflux-v4-canonical.bundle`

Verified SHA256:

`c42fe84bc5a9a77b510e88685f3c24957f4ab1bc8b41d363586c45412ab3be20`

`git bundle verify` reports a complete history.

Verified important refs include:

- private v4 commit `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- tags `yandex-working-v1` through `yandex-working-v4`
- historical upstream base `461905369bd8f44ad2aacff240540d6a01d38c4d`
- reviewed upstream cursor `6443d42ac3fe643e702c8774bc4508e2c5ec222c`

## Import rule

The AWS Git tree wins over reconstructed source from handovers, logs or chat excerpts.

The exact Git history/tags must be preserved because historical commit IDs are already referenced by production VERSION files, handovers and rollback records.

The temporary GitHub bootstrap history is repository scaffolding only and must not replace canonical OpenFlux history.

## Steady-state repository layout

- `main` - our private downstream trunk, descended from the exact imported v4 history
- `canonical-v4` - immutable bootstrap reference to the imported production v4 commit
- `upstream-main` - public upstream tracking branch
- short-lived `feature/`, `backport/` and `test/` branches
- historical private tags retained

## Validation

1. Canonical v4 source exists on GitHub at the original commit ID.
2. Historical private tags remain reachable.
3. `upstream-main` tracks the reviewed public upstream state independently.
4. `go test ./...` passes in GitHub Actions.
5. Linux amd64 artifact is built in GitHub Actions.
6. Build metadata records source commit and Go version.
7. An exact-source reproduction job checks out `22f29ab...` and builds it separately from repository documentation commits.
8. Historical production binary SHA remains recorded. A build SHA mismatch must be understood before replacing production, but does not invalidate the exact source import.
9. No production deployment occurs during repository bootstrap.

## Upstream model

Public upstream remains `p1neappleXpress/OpenFlux`.

Private GitHub is the downstream development source of truth after successful import and CI validation. Upstream changes remain review-only until explicitly integrated. No automatic sync-to-production.
