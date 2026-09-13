# OpenFlux private fork - production baseline

This repository is private. Do not make it public without an explicit decision.

## Current production baseline

The approved production v4 source was imported from the canonical AWS Git tree at `/opt/openflux/src/openflux-yandex`.

- production commit: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- production tag: `yandex-working-v4`
- production binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`
- historical upstream base: `461905369bd8f44ad2aacff240540d6a01d38c4d`
- reviewed-through upstream cursor: `6443d42ac3fe643e702c8774bc4508e2c5ec222c`
- profile: `yandex-batch4-1ms-v1`
- batching: `4 packets / 1 ms`
- known-bad setting: `4 packets / 2 ms`
- AWS exit IP: `3.8.0.35`

## Dynamic Yandex model

Production no longer uses static Yandex session JSON at runtime. AWS, Mini-PC1 and Mini-PC2 independently fetch fresh bootstrap/session data from the same public Yandex document URL:

`https://disk.yandex.ru/i/65fb1Od_I1ysSA`

The live URL is stored in:

- AWS: `/etc/openflux/yandex.env`
- Mini-PC clients: `/etc/openflux-client/yandex.env`

Legacy `yandex.json` files may still exist as rollback/history material but are not runtime dependencies.

## v4 behavior that must be preserved

- signed Yandex `editorConfig.user.id` identity
- dynamic OnlyOffice build detection
- current OnlyOffice authentication flow
- `lcid=25`
- private batching and TCP buffer tuning
- panic containment / SafeGo hardening
- SOCKS bounds and recovery hardening
- bounded TCP/WebSocket/HTTP timeouts
- WebSocket diagnostics
- reconnect exponential backoff with jitter
- safe Yandex config parsing

## Production update policy

Do not automatically deploy upstream changes. The approved lifecycle remains:

`status -> check -> assess -> explicit stage -> explicit deploy`

GitHub is becoming the private downstream source/build/release control plane. Production deployment remains explicit and rollback-safe.
