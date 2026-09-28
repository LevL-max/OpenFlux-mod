# Volga integration and rollout

Branch: `codex/volga-integration`, from local `claude/volga-integration@bb5c293`.
Production baseline: v4.0.6 / `47e9cb7`. Proposed RC: **v4.1.0-rc1**.
Main changes only through a PR.

## Implemented

1. V6 behind the `volga` build tag; the Legacy executable remains byte-identical.
2. Public egress and 64-stream admission from lab `c630641933c19fdf250c9c13ffaef6fee6e80be3`.
3. Separate release binary/container, SHA256SUMS and protocol manifest with pinned image digest.
4. Separate runtime/config/update state, bounded resources, health checks and rollback watchdog.
5. Protocol card selection, config import/export and separate Volga updates. Both real grouped panels patched offline and checked for idempotence, Python and JavaScript syntax.
6. Independent server container; client unit uses the existing SOCKS listener and router bridge. Router helpers follow `active_transport`.
7. Same Disk token/pairing keys, separate encrypted document inboxes and signed Volga status. Startup waits for new cookies after CAPTCHA; browser profile changes rebuild the session within the process.

8. PR #19 CI passed: full Linux race/vet, 38 integration tests, reproducible
   binaries and both container smokes. CI binaries match the local SHA-256 values.
   [Validated code eab50c6](https://github.com/LevL-max/OpenFlux-mod/actions/runs/36415351502).

## Remaining gates

9. After review/merge, release workflow creates a **draft prerelease**. Verify the exact RC assets before enabling a live installation.
10. Short functional RC check on server and PC2: SOCKS/HTTPS and SHA-256, cookie recovery, protocol switch and rollback. Restore the prior router mode, review before stable/PC1 rollout.

The earlier six-hour throughput soak is superseded by the user's instruction to
freeze performance and stop speed experiments. No new window/rate/worker tuning.

An ordinary update retains Legacy. Volga needs its own config, credentials,
installation and explicit selection. Server transports run independently;
client transports share port 11080 and cannot run together. Default Volga limits:
256 MiB, zero swap, 50% of one CPU, 128 tasks, 64 streams.

PC1/PC2 have disabled Legacy and bridge units: the router controller owns start/
stop. The bridge has no Requires/Wants dependency on Legacy. Offline checks have
not changed services, panels or routes. The lab test is complete; it is not proof
that the final RC deployment/update path has passed a live test.

See [integration instructions](../integration/README.md),
[lab evidence](VOLGA-LAB-ROUTER-2026-09-28.md) and
[integration verification](VOLGA-INTEGRATION-2026-09-28.md).
