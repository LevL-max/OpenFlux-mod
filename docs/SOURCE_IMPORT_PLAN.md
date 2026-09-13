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

- `refs/heads/openflux-yandex` -> `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- `refs/tags/yandex-working-v1`
- `refs/tags/yandex-working-v2`
- `refs/tags/yandex-working-v3`
- `refs/tags/yandex-working-v4` -> `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- historical upstream `main` base `461905369bd8f44ad2aacff240540d6a01d38c4d`
- reviewed upstream refs captured at export time

## Import rule

The AWS Git tree wins over reconstructed source from handovers, logs or chat excerpts.

The exact Git history/tags should be preserved rather than replaced by a source-only snapshot, because historical commit IDs are already referenced by production VERSION files, handovers and rollback records.

The temporary GitHub `bootstrap` history is repository scaffolding only. It must not replace the canonical OpenFlux history.

## Desired steady-state repository layout

- `main` - our imported private downstream trunk, initially equivalent to `yandex-working-v4`
- `upstream-main` - public upstream tracking state after bootstrap
- short-lived integration/backport branches
- historical private tags retained
- documentation/CI committed on top of the imported private history after import

## Validation after import

1. GitHub `main` tree equals the exact v4 source tree.
2. Historical private tags remain reachable.
3. The imported v4 commit ID remains `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e` before repository-only documentation commits are added on top.
4. `go test ./...` passes in GitHub Actions.
5. Linux amd64 binary is built in GitHub Actions.
6. Build metadata records GitHub commit and Go version.
7. Compare the GitHub-built binary against historical production SHA. A mismatch is not automatically a source mismatch because toolchain/environment/flags may differ, but it must be understood before production deployment.
8. No production deployment occurs during repository bootstrap.

## Upstream model

Public upstream remains:

`p1neappleXpress/OpenFlux`

Private GitHub becomes the downstream source of truth only after successful import and CI validation.

Upstream changes remain review-only until explicitly integrated. No automatic sync-to-production.
