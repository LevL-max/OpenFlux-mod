# Repository bootstrap status

Status: **VALIDATING - SOURCE IMPORT COMPLETE**

Completed:

- private repository verified
- exact canonical AWS Git bundle exported and SHA256 verified
- original v4 commit imported without rewriting commit ID
- `canonical-v4` -> `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- historical private tags `yandex-working-v1` through `yandex-working-v4` imported
- `yandex-working-v4` verified to point to `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- `upstream-main` created and verified at reviewed upstream cursor `6443d42ac3fe643e702c8774bc4508e2c5ec222c`
- integration branch created directly on top of canonical v4 ancestry
- downstream DevOps model documented
- upstream review/integration policy documented
- durable history policy added
- GitHub Actions CI added
- `go test ./...` passed on imported downstream source
- Linux amd64 build passed and artifact uploaded
- exact v4 reproduction workflow added; it checks out production source commit `22f29ab...` directly
- exact v4 reproduction test/build passed

Build validation notes:

- normal integration build SHA256: `a2c24c4a1a06512264c11cadf7b4bee2da49c0041c6a7b595a460091063622ac`
- exact-source v4 reproduction build SHA256: `a9502869305c34f3f7a71bccc2cd991f582aa7aad8dacfcfc39fae3a5c01d1e9`
- historical production binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

The source import is exact, but the historical production binary is not yet bit-for-bit reproduced by the current GitHub build recipe. This is a build-recipe/tooling question, not a source-history mismatch. Production is not being replaced during bootstrap.

Pending:

- consolidate remaining durable project documentation onto the imported-history branch
- promote the validated imported-history integration branch to repository `main`
- create first approved private GitHub release only after release workflow is finalized
- adapt AWS updater to consume an explicit approved private GitHub release artifact with existing stage/health/rollback safety
- define read-only private GitHub release access for Mini-PC distribution
- remove temporary write deploy key after bootstrap push work is finished

Production action during bootstrap: **NO CHANGE**
