# OpenFlux Delta - 2026-09-13 - GitHub bootstrap

## Scope

Repository/bootstrap only. Production hosts were not changed.

## Change

Created private GitHub repository baseline under `LevL-max/OpenFlux-mod` and prepared a `bootstrap` branch for migration of the canonical AWS v4 source tree.

Added:

- project production baseline
- durable history policy
- project history ledger
- source import plan
- safe AWS bundle export instructions
- bootstrap status
- GitHub Actions CI skeleton for `go test ./...` and Linux amd64 build
- repository hygiene / secret ignore rules

## Production baseline before change

- AWS private commit: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- tag: `yandex-working-v4`
- binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

## Production impact

None.

No AWS service restart, router change, transport change, token/config change or Mini-PC change was performed.

## Next step

Import the exact canonical Git tree from `/opt/openflux/src/openflux-yandex` rather than reconstructing source from chat history.

## Current action

`CURRENT ACTION = SOURCE IMPORT ONLY - NO PRODUCTION CHANGE`
