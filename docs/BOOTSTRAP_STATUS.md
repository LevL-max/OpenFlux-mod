# Repository bootstrap status

Status: **COMPLETE - GITHUB MAIN IS AUTHORITATIVE DEVELOPMENT SOURCE**

Completed:

- private repository verified
- exact canonical AWS Git bundle exported and SHA256 verified
- original v4 commit imported without rewriting commit ID
- `canonical-v4` -> `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- historical private tags `yandex-working-v1` through `yandex-working-v4` imported
- `yandex-working-v4` verified to point to `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- `upstream-main` created and verified at reviewed upstream cursor `6443d42ac3fe643e702c8774bc4508e2c5ec222c`
- downstream documentation and CI committed directly on top of canonical v4 ancestry
- imported-history branch promoted to repository `main`
- downstream DevOps model documented
- upstream review/integration policy documented
- durable history policy and project ledger added
- repository secret ignore rules hardened
- `go test ./...` passed in GitHub Actions
- Linux amd64 build passed and artifact uploaded
- exact-source v4 reproduction workflow passed while checking out `22f29ab...` directly

Build validation notes:

- integration build SHA256 recorded during bootstrap: `a2c24c4a1a06512264c11cadf7b4bee2da49c0041c6a7b595a460091063622ac`
- exact-source default build SHA256: `a9502869305c34f3f7a71bccc2cd991f582aa7aad8dacfcfc39fae3a5c01d1e9`
- exact-source `-trimpath` build SHA256: `6526d2de95b12f08114632ec124b89227b59508930f097941ec034e0dfef27e4`
- historical production binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

The source import is exact, but neither tested GitHub build variant reproduces the historical production binary bit-for-bit. This is now explicitly a build-recipe/tooling investigation and does not invalidate the imported source history. The current production binary remains unchanged and authoritative until the private GitHub release recipe is finalized and validated.

Steady-state repository roles:

- `main` - authoritative private downstream development trunk
- `canonical-v4` - immutable imported production-v4 source reference
- `upstream-main` - reviewed public upstream tracking branch
- `feature/*`, `backport/*`, `test/*` - short-lived work branches

Next delivery work:

- finalize private GitHub release workflow and immutable asset naming
- determine the canonical future build recipe/flags
- create the first approved private GitHub release
- adapt AWS updater to consume only an explicitly approved private GitHub release while preserving stage/health/rollback
- define read-only private release access for Mini-PC distribution
- remove the temporary bootstrap write deploy key

Production action during repository bootstrap: **NO CHANGE**
