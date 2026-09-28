# Volga integration and rollout

Volga **v4.1.0-rc2** is published as a prerelease and installed on the existing
AWS, PC1 and PC2. Stable remains **v4.0.6**. Release source:
`bef15276c23d4cf324044aedc8dabcde1f0a137b`.

Integration was developed in `codex/volga-integration`, based on local
`claude/volga-integration@bb5c293`, and merged through PRs #19, #20 and #21.
Legacy Go source remains unchanged; reproducible builds with `-buildvcs=false`
match the baseline. No installed Legacy executable was replaced.

## Accepted topology and final state

- One Volga container on the existing AWS, alongside Legacy.
- One shared document pair and matching transport configuration, used by PC1 or
  PC2 **one at a time**. Stop the old client before starting the other, even if
  idle. Extra document links are not configured.
- Existing recovery keys and Disk token are reused; no new recovery pairing.
- Both mini-PCs end with Legacy selected and Volga stopped. Their original
  router traffic modes remain unchanged. AWS Volga stays running for a client.
- Actual Volga limits: 256 MiB, no swap, 50% of one CPU, 128 tasks, 64 streams.

## Completed checks

Separate verified binaries/images, protocol manifest, bounded runtimes,
transactional support updates, watchdog and rollback are implemented. Both real
customized panels retain their grouped configuration cards and expose protocol
selection, Volga config import/export, updates and browser-cookie recovery.

Short live checks covered 64 simultaneous SOCKS transfers per client with all
SHA-256 values matching, private-egress denial, admission limits, half-close and
abort recovery. Actual installed RC2 checks then covered both panels, encrypted
Disk delivery for both documents from each client, signed server status,
Legacy/Volga selection with exit-IP health checks, HTTPS hashes and rollback.

PC1's RC1 install correctly rolled back on its optional delegating runtime helper.
RC2 preserves that helper and patches the actual controllers. All 42 Python
integration tests, Linux Go race/vet and separate container checks passed.

Original Legacy binaries, service state/PIDs, AWS Legacy container, keys, router
modes and IP rules were verified unchanged. Temporary staging and unused release
caches were removed; installed runtime and rollback checkpoints remain.

## Release boundary

RC2 remains a **prerelease**, not a stable promotion. One final HTTPS probe on PC2
failed and succeeded on the same probe's retry without a configuration change;
the first curl cause was not retained. A manual recovery poll also needed a
retry. These limitations are recorded in the report; there is no zero-error or
long-term reliability claim. Existing LAN traffic modes were not switched for
the tests, so this is not a new whole-LAN routing test.

The user's performance freeze remains in force. No further speed tuning,
throughput experiments or long soak were performed. The frozen profile blob is
`09daf0229d395ad4b39dea82446a33c9d1fc82b4`.

See [installed rollout report](VOLGA-ROLLOUT-2026-09-28.md),
[sanitized deployment evidence](VOLGA-ROLLOUT-2026-09-28.json),
[64-connection AWS evidence](VOLGA-AWS-FUNCTIONAL-2026-09-28.md),
[lab evidence](VOLGA-LAB-ROUTER-2026-09-28.md) and
[operation instructions](../integration/README.md).
