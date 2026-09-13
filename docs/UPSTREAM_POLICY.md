# Upstream tracking and integration policy

Public upstream: `p1neappleXpress/OpenFlux`

Private downstream: `LevL-max/OpenFlux-mod`

## Rule

Upstream is an input to engineering review, not a deployment feed.

We track upstream changes continuously, but only selected useful changes enter the private fork.

## Two different upstream references

Do not confuse:

- historical merge base: `461905369bd8f44ad2aacff240540d6a01d38c4d`
- last reviewed upstream cursor: `6443d42ac3fe643e702c8774bc4508e2c5ec222c`

The historical base is used for three-way integration semantics.

The review cursor is used to determine what is genuinely new since the previous review.

## Review procedure

1. Fetch current public upstream metadata.
2. Compare current upstream head to the stored review cursor.
3. If unchanged, report `NO CHANGE`.
4. Review only newly appeared commits.
5. Classify each relevant change.
6. Check whether private `main` already contains equivalent semantics.
7. If useful, integrate on a dedicated branch.
8. Run CI and isolated transport tests.
9. Merge only after explicit review.
10. Release only from our private repository.
11. Advance the review cursor after review is complete, not merely after fetch.

## Integration rule

For non-trivial updates, evaluate final-tree compatibility rather than replaying historical downstream patches blindly.

The existing AWS work established `git merge-tree` as the preferred compatibility model.

Never auto-resolve conflicts by always preferring upstream or always preferring downstream.

## Current watch list

High-value upstream areas:

- Yandex bootstrap/authentication
- OnlyOffice protocol/build changes
- signed identity/session handling
- WebSocket and reconnect reliability
- HTTP safety
- panic/crash hardening
- SOCKS correctness
- Linux tunnel/network path
- batching/queue/compression/TCP buffering
- CPU/memory improvements relevant to our AWS host
- dependency/security fixes

Alternative transports such as `vyandex`/Volga or CUPS remain experiments until they outperform or solve a real production problem.

## Deployment boundary

Even when an upstream change is selected, the deployment artifact must be built and released from `LevL-max/OpenFlux-mod`.

Production should never install an arbitrary binary directly from `p1neappleXpress/OpenFlux`.
