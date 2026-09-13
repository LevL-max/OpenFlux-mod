# OpenFlux Project History Ledger

**Ledger date:** 2026-09-12  
**Purpose:** Durable project state and chronology independent of ChatGPT chat retention/compaction.  
**Rule:** Chat history is not authoritative. This ledger + the current Master Handover are.

---

## 1. Precedence / source-of-truth rules

When sources conflict, use this order:

1. **Latest explicitly approved Master Handover / later approved Delta**
2. **Current production VERSION / SHA / checkpoints on the machines**
3. **Component runbooks that are not superseded**
4. **Historical scripts and intermediate handovers**
5. **Chat recollection / summaries**

Current top-level authoritative document:

`OpenFlux_Master_Handover_2026-09-12_FINAL(1).md`

SHA256:

`32f365620f2f8cd3a34ed72cb1829ea9867e85f5486df614bf7dbaa63e2b68dc`

Important: `OpenFlux_AWS_v4_Daily_Review_Master_Handover_2026-09-12(1).md` is an **earlier snapshot from the same work period**. Its AWS/updater material remains useful, but its statement that Mini-PC dynamic migration was not yet completed is superseded by the later FINAL Master Handover.

---

## 2. Current approved production state

### Private OpenFlux v4

- commit: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- tag: `yandex-working-v4`
- binary SHA256: `6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`
- public Yandex document URL: `https://disk.yandex.ru/i/65fb1Od_I1ysSA`
- profile: `yandex-batch4-1ms-v1`
- transport: `yandex`
- batching: `4 packets / 1 ms`
- known bad: `4 packets / 2 ms`
- AWS exit IP: `3.8.0.35`

All three production participants now use the dynamic public-URL bootstrap model:

- AWS exit node
- Mini-PC1
- Mini-PC2

The old static Yandex JSON is **not a runtime dependency**.

Each process independently fetches current bootstrap/session information and may receive different temporary JWT/session/user values. That is expected.

---

## 3. What v4 fixed / preserves

v4 preserves the private performance path and adds the production hardening that was selectively backported from upstream:

- dynamic Yandex public URL bootstrap
- signed Yandex `editorConfig.user.id`
- dynamic OnlyOffice build detection
- current OnlyOffice authentication flow
- `lcid=25`
- private batching / queue / TCP buffer tuning
- SafeGo / panic containment
- SOCKS bounds/recover hardening
- TCP dial timeout
- bounded WebSocket handshake
- WebSocket diagnostics
- reconnect exponential backoff + jitter
- reconnect reset after a healthy session
- HTTP redirect cap and fetch timeout
- safer Yandex config parsing

Do not revert to locally generated Yandex user IDs, static OnlyOffice build strings, or static session JSON.

---

## 4. AWS production / updater

AWS updater:

- path: `/usr/local/sbin/openflux-update`
- version: `2.0.0-rc6`
- SHA256: `3c6092d6888879cf96e450574eb557f8c8cf1cc48d5e5145e64b7ed2728a7f50`

Approved flow:

`status -> check -> assess -> explicit stage -> explicit deploy`

No automatic production deployment from upstream.

Validated updater properties:

- current v4 stages reproducibly byte-for-byte
- identical binary -> `DEPLOY=NOOP_ALREADY_CURRENT`
- NOOP does not restart production
- `stage main` stops on manual merge conflict
- invalid rollback target fails closed
- deploy has checkpoint + health gate + automatic rollback
- management SSH path guard must not be bypassed casually

Important AWS checkpoints:

- `/opt/openflux/checkpoints/aws-v4-baseline-20260912T005544Z`
- `/opt/openflux/checkpoints/pre-v4-deploy-20260912T005235Z`
- `/opt/openflux/checkpoints/updater-rc6-install-20260912T011518Z`
- `/opt/openflux/checkpoints/updater-pre-v2-20260912T002759Z`

Daily upstream review cursor:

`6443d42ac3fe643e702c8774bc4508e2c5ec222c`

Historical merge base:

`461905369bd8f44ad2aacff240540d6a01d38c4d`

These two SHAs have different purposes and must not be confused.

---

## 5. Mini-PC current architecture

Five modes remain:

- DIRECT
- AWG
- TUN
- MIHOMO
- OPENFLUX

OpenFlux path:

`native client -> SOCKS 127.0.0.1:11080 -> sing-box-openflux -> oflux0`

Routing:

- OpenFlux mark `0x4`
- OpenFlux table `130`
- AWG mark `0x1`
- AWG table `100`
- host default route remains WAN

Client is intentionally on-demand and normally disabled/inactive outside OPENFLUX mode.

Mini-PC2 validated timeout policy:

- client readiness: `45s`
- activation hard timeout: `75s`
- rollback hard timeout: `40s`
- fallback: `AWG`

Do not “fix” the client by enabling it permanently.

---

## 6. Chronology of the abandoned Mini-PC update-channel experiment

This is important because several scripts exist and can otherwise be mistaken for current production tooling.

### v1.0.0

`openflux-minipc-client-v4-master-v1.0.0(1).sh`

SHA256: `3824f8863b1f9a953e20fef15078231bcb2eb6bacbdb25f6754e8e7efe1d05f7`

Initial design:

`Mini-PC -> SSH/SCP -> AWS production binary`

It attempted to make AWS the approved artifact source directly over SSH.

**Status: SUPERSEDED / DO NOT USE.**

### v1.0.1

`openflux-minipc-client-v4-master-v1.0.1(1).sh`

SHA256: `421b903ff50f0d68ee41fb374bb43db7122d18612750cc24a29f8581235af32a`

Added stricter AWS host-key verification and SSH invocation handling.

**Status: SUPERSEDED / DO NOT USE.**

### v1.0.2

`openflux-minipc-client-v4-master-v1.0.2(1).sh`

SHA256: `abaa68b4671ead34d0191ca1ea48f9d0093ec3db28392cf80e9ff863a522dee2`

Transitional dedicated-key/artifact-gateway work. This file is not a valid final design and contains signs of an incomplete merge of two approaches (including duplicate `remote_metadata()` logic).

**Status: ABANDONED / DO NOT USE.**

### Artifact gateway

`openflux-aws-artifact-gateway-v1.0.0(1).sh`

SHA256: `4c4555c265ea2fec345dce0dc740082b1eca1d5bea166c196d4e0687ffba3a52`

Implemented a forced-command, read-only SSH gateway exposing only VERSION/binary/health.

It was a security-conscious experiment, but the entire direct Mini-PC -> AWS update/control dependency was later rejected.

**Status: ABANDONED / DO NOT DEPLOY.**

### v1.0.3

`openflux-minipc-client-v4-master-v1.0.3(1).sh`

SHA256: `0806cad32e80ceb069ce63be9e5d9dd6a1b0a6b5d7b95e9239162c3fde6de55e`

Completed the dedicated key + restricted AWS gateway concept.

**Status: ABANDONED / DO NOT USE.**

### Why the whole approach was rejected

Final architecture decision:

- no direct Mini-PC -> AWS update/control SSH dependency
- no permanent artifact gateway requirement
- no Mini-PC independent upstream following
- avoid DPI/path uncertainty and unnecessary key/control-plane coupling

The failed/intermediate work was rolled back before the final migration.

---

## 7. Final approved Mini-PC deployment model

The accepted design replaced the SSH artifact channel:

`upstream GitHub -> AWS review/assess/stage -> explicit approved AWS production -> self-contained client installer -> manually/management-path copy installer -> local SHA-verified Mini-PC migration`

Approved generated installer:

`openflux-minipc-approved.sh`

Recorded installer SHA256:

`48b668cca97bdfff5f7a48d0ce997c749ffca9ce93db4668c5d9bb49ae085d33`

Embedded approved binary SHA256:

`6f242d3c7ef9184811e58df5893e0ad0095da6aeb7354ba02df5da99fa569715`

Both Mini-PC1 and Mini-PC2 were successfully migrated using this final model.

---

## 8. Earlier AWS daily handover - scope and supersession

`OpenFlux_AWS_v4_Daily_Review_Master_Handover_2026-09-12(1).md`

SHA256:

`2395a7f195aa169d7d01ccc72adc4eac75bb60e284a29e0cf60f0f70a2b8d0a6`

This document is still valuable for:

- AWS v4 production baseline
- updater design and safety
- upstream review cursor
- merge-tree compatibility strategy
- upstream classification
- AWS daily health model
- known conflict files

But its Mini-PC status sections were written before final Mini-PC dynamic migration.

Therefore:

**AWS/updater sections: RETAIN**  
**Mini-PC “not yet migrated” statements: SUPERSEDED by FINAL Master Handover**

---

## 9. Current experimental transport watch: CUPS

Source reviewed:

`cupsonline.go`

SHA256: `cd687e589946ae125f0edb1e4b82a26c80b299ffaa3f8f8e461b67106173820c`

Current classification:

**WATCH / EXPERIMENTAL - NOT PRODUCTION**

Observed design:

- CUPS / interview live-coding rooms as the carrier
- multiple rooms / WebSocket channels
- EXIT creates rooms
- client receives a compact base64 room list
- each side authorizes against room URLs
- flow pinning distributes TCP flows across channels
- batching currently configured around 64 packets / 32 KiB / 2 ms in the reviewed file
- current source calls `SetConnected(true)` before proven WS readiness
- flow map requires lifecycle/GC review
- reconnect/token refresh behavior needs production-grade validation
- throughput, room lifetime, reconnect, restart persistence, auth/token lifetime and 24h+ stability must be proven before adoption

Decision:

**Do not replace Yandex v4 now. Monitor upstream/repository development and only run an isolated A/B test when there is a clear reason.**

---

## 10. Current unresolved / future work

These are not reasons to disturb current production:

1. **Azure VPN through OpenFlux**
   - laptop runs Azure VPN behind Mini-PC
   - same profile works through other router modes/tunnels
   - fails through current OpenFlux
   - investigate only when deliberately scheduled
   - do not weaken generic UDP policy without evidence

2. **Long-duration dynamic Yandex validation**
   - v4 removes the operational dependency on a ~24h static JSON/token
   - continue observing 24-30h+ behavior
   - if Yandex closes a session, v4 should dynamically refetch bootstrap and reconnect

3. **Alternative transports**
   - Volga/vyandex: WATCH
   - CUPS: WATCH
   - production Yandex v4: KEEP

---

## 11. Durable history protocol for future sessions

From now on, every meaningful OpenFlux change should produce a short **Delta** file before the chat ends.

Naming:

`OpenFlux_Delta_YYYY-MM-DD_<topic>.md`

Minimum contents:

- date/time
- machine(s) affected
- reason for change
- files/scripts changed
- old SHA/commit
- new SHA/commit
- exact production decision
- tests actually passed
- rollback checkpoint
- upstream review cursor if changed
- superseded artifacts
- unresolved items
- `CURRENT ACTION = ...`

Rules:

- Never use chat memory as the sole record.
- Never overwrite history silently.
- A later Delta can supersede an older Delta, but must say so explicitly.
- After several Deltas, merge them into a new Master Handover.
- Historical scripts remain listed as historical; they are not silently deleted from the record.
- If a future ChatGPT session lacks context, provide the latest Master Handover plus all later Delta files.
- If the Master and a historical script disagree, the Master wins unless a later Delta explicitly changes it.

---

## 12. Current action

`CURRENT ACTION = NO CHANGE`

Production is working.

Do not redesign, retune, migrate transport, or reopen solved investigations unless a real failure or a deliberately approved experiment justifies it.
