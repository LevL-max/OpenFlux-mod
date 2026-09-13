# Canonical source import plan

## Goal

Make `LevL-max/OpenFlux-mod` the authoritative source repository without reconstructing private v4 from chat memory.

## Authoritative pre-GitHub source

AWS canonical tree:

`/opt/openflux/src/openflux-yandex`

Expected production state before import:

- HEAD: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- tag: `yandex-working-v4`
- clean working tree
- production binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

## Import rule

The AWS Git tree wins over reconstructed source from handovers, logs or chat excerpts.

Before GitHub becomes source of truth, import the exact AWS tree and preserve the existing downstream history/tags where possible.

## Validation after import

1. GitHub source contains the exact v4 tree.
2. `go test ./...` passes in GitHub Actions.
3. Linux amd64 binary is built in GitHub Actions.
4. Build metadata records GitHub commit and Go version.
5. Compare the GitHub-built binary against the historical production SHA. A SHA mismatch is not automatically a source mismatch because build environment/flags may differ, but it must be understood before changing production.
6. No production deployment occurs during repository bootstrap.

## Upstream model

Public upstream remains:

`p1neappleXpress/OpenFlux`

Private GitHub becomes the downstream source of truth after successful import.

Upstream changes remain review-only until explicitly integrated. No automatic sync-to-production.
