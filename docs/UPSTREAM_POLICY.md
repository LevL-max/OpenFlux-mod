# Upstream review and integration policy

Public upstream: `p1neappleXpress/OpenFlux`
Private downstream: `LevL-max/OpenFlux-mod`

## Principle

The private repository is a maintained downstream fork, not an automatic mirror.

Upstream is monitored continuously for fixes, security/reliability improvements, protocol changes, and new transports that may be useful. New upstream code is never assumed safe or desirable merely because it was merged upstream.

## Review cursor

Keep two concepts separate:

- `upstream_base` - historical common base used for merge semantics.
- `review_cursor` - most recent upstream commit already reviewed for new changes.

Current historical values at GitHub bootstrap:

- upstream base: `461905369bd8f44ad2aacff240540d6a01d38c4d`
- reviewed through: `6443d42ac3fe643e702c8774bc4508e2c5ec222c`

## Classification

New upstream changes are classified as one of:

- `ACTION / HIGH VALUE` - strong candidate for immediate integration/testing.
- `REVIEW / POTENTIALLY USEFUL` - relevant but needs analysis.
- `WATCH` - interesting but not mature enough for integration.
- `IGNORE FOR PRODUCTION` - irrelevant to our production architecture.
- `ALREADY BACKPORTED` - semantics already present downstream.

## Integration

Never merge upstream directly into production `main` without review.

Preferred flow:

1. advance/update `upstream-main` to the reviewed upstream commit;
2. compare it against downstream history;
3. create `backport/<topic>` or `feature/<topic>`;
4. integrate only selected changes;
5. run CI and isolated functional tests;
6. merge to downstream `main` only when approved;
7. create an explicit downstream release;
8. deploy through staging/health/rollback gates.

When a full upstream merge has conflicts, inspect the final-tree semantics rather than assuming patch applicability means compatibility.

## New transports

Experimental transports such as Volga/vyandex or CUPS-derived work stay isolated from stable production until they have demonstrated restart/reconnect behavior, readiness semantics, sustained throughput, and operational reliability.

## Production rule

`upstream-main` is never a deployment source. Only explicit downstream releases from `main` may become production candidates.
