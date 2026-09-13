# OpenFlux Delta - 2026-09-13 - DevOps model

## Scope

Repository architecture only. No production host was changed.

## Decision

`LevL-max/OpenFlux-mod` will become the private downstream source of truth after exact canonical v4 history import and validation.

Public `p1neappleXpress/OpenFlux` remains the upstream project to monitor for reusable fixes and features.

## Approved development flow

```text
public upstream
  -> review / compare
  -> selected integration into private repo
  -> PR / CI / tests
  -> explicit private release
  -> AWS deployment
  -> approved Mini-PC distribution
```

No direct deployment from public upstream.

No machine independently follows upstream.

## Release/install direction

Future production binaries should be GitHub-built and attached to explicit private releases with SHA256 and build metadata.

AWS updater will later be adapted to consume an explicit approved GitHub release while preserving stage, health gate and rollback behavior.

Mini-PCs will consume only private approved releases, either through read-only GitHub access or via a self-contained installer created from that release. Direct Mini-PC -> AWS update/control dependency remains rejected.

## Canonical bundle verification

Uploaded canonical bundle SHA256:

`c42fe84bc5a9a77b510e88685f3c24957f4ab1bc8b41d363586c45412ab3be20`

Bundle verification result:

- complete history: YES
- private v4 HEAD: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- tag `yandex-working-v4`: points to the same commit
- historical private tags v1/v2/v3/v4 present
- public upstream historical refs present

## Production impact

None.

`CURRENT ACTION = IMPORT EXACT GIT HISTORY, THEN VALIDATE CI - NO PRODUCTION CHANGE`
