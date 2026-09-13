# Durable history policy

Chat history is not the source of truth for this project.

## Precedence

When information conflicts, use this order:

1. latest approved Master Handover or later approved Delta
2. current production VERSION/SHA/checkpoints on the machines
3. current repository source and release metadata
4. component runbooks that are not superseded
5. historical scripts and intermediate handovers
6. chat recollection or summaries

## Change record

Every meaningful production change should create a short Delta document under `docs/deltas/` before the work session is considered complete.

Suggested name:

`OpenFlux_Delta_YYYY-MM-DD_<topic>.md`

Minimum contents:

- date/time
- machine(s) affected
- reason for change
- files/scripts changed
- old commit/SHA
- new commit/SHA
- exact production decision
- validation actually passed
- rollback checkpoint
- upstream review cursor if changed
- superseded artifacts
- unresolved items
- `CURRENT ACTION = ...`

After several Deltas, consolidate them into a new Master Handover.

Historical scripts must remain explicitly classified as CURRENT, SUPERSEDED, ABANDONED or DO NOT USE. Do not silently resurrect an older implementation because it happens to still exist in a chat or file archive.
