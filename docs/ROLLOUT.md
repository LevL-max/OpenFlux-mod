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

9. Resolve simultaneous PC1/PC2 operation before calling the integration release-ready. The current Volga endpoint holds one peer/session, and the adapter manages one Volga instance per server. A second client using the same config can interrupt the first. The 64-stream limit applies within one client session, not to 64 independent mini-PCs.
10. After the topology fix, review/merge and create a **draft prerelease**. Verify the exact RC assets before enabling a live installation.
11. Short functional RC check on the existing AWS and PC2: SOCKS/HTTPS and SHA-256, cookie recovery, protocol switch and rollback. Then validate both mini-PCs concurrently using the supported topology. Restore prior router modes and review before stable rollout.

## Confirmed deployment target

The sole server is the existing AWS `3.8.0.35`, with PC1 `192.168.1.132` and
PC2 `192.168.1.74` as clients. The earlier isolated lab VPS pair is not a rollout
target. Read-only SSH inventory on 2026-09-28 confirmed the AWS Legacy container
and recovery timer are active, and both mini-PC settings expect this AWS exit IP.
No new server or new recovery key set is required.

One possible topology is separate bounded Volga instances for PC1 and PC2 on the
same AWS, alongside Legacy. This needs instance-aware installation, updates,
recovery/status isolation and a document/key allocation; it is not implemented
by the current single-instance adapter. An alternative is multi-client session
support in the server. Neither option has passed a concurrent two-client check.
Do not deploy identical single-session Volga configs to both mini-PCs.

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
