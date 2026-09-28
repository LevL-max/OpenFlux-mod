# Volga integration verification — 2026-09-28

Branch `codex/volga-integration` starts at local Claude commit `bb5c293`, based
on production v4.0.6 (`47e9cb7`). Proposed version: `v4.1.0-rc1`.
Lab source: `c630641933c19fdf250c9c13ffaef6fee6e80be3`.

## Scope and compatibility

Volga is a separate opt-in binary, service/container and configuration. Ordinary
updates retain Legacy. Public egress and bounded 64-stream admission are imported
from the completed lab; the measured performance profile is unchanged. Only
Volga-tagged Go authentication code changes in the root transport package.

The existing updater receives the same six integration module filenames, so
v4.0.x archive validation still accepts the new bundle. Volga has separate release
state and checksums; the container image is pinned by digest and local image ID.
The server loads the verified release archive without registry credentials. A failed health
check restores the binary, unit/container, node selection, config, recovery timer
and patched router helpers. A watchdog recovers interrupted operations. An
unmanaged container with the same name is rejected; a failed container rename
cannot delete the original container during rollback.

Client selection uses the existing loopback SOCKS port and bridge. Switching
checks session readiness and HTTPS/exit IP, preserves the saved router mode and
restores Legacy after failure. Server transports remain independent. Defaults
bound Volga to 256 MiB, no swap, 50% of one CPU and 128 tasks; CLI limits and
`max_streams` remain configurable without changing throughput parameters.

Existing Disk token and pairing keys are reused. Envelopes and signed status are
bound to the Volga protocol; each document has its own inbox to prevent overwrite.
Missing/invalid packets back off independently. Startup CAPTCHA waits locally
for changed credentials; server Set-Cookie from an earlier lane cannot cause a
retry loop against another blocked lane. Browser profile updates rebuild the
session in-process. Interactive CAPTCHA verification still requires the browser.

## Verification

| Check | Result |
|---|---|
| Root Go race tests and vet | PASS on Windows; one POSIX-permission assertion excluded |
| Tagged transport race tests and vet | PASS with the same Windows-only exclusion |
| Nested Volga race tests and vet | PASS, including startup wait, resume, cancellation and self-cookie-write regression |
| Linux integration tests | PASS, 38 in GitHub CI; preceding 37-test set also passed on isolated PC2 |
| PC1/PC2 actual panel Python syntax and idempotence | PASS |
| PC1/PC2 actual panel JavaScript syntax | PASS, four/two script blocks respectively |
| Existing grouped config renderer | Preserved; only Volga config entry and private-file badge added |
| Both routers' patched shell helpers | PASS, `bash -n` on Linux |
| Linux executable smoke | `openflux-volga v4.1.0-rc1`, exit 0 |
| Legacy reproducible binary comparison | Identical to v4.0.6 |
| Frozen profile | Identical Git blob `09daf0229d395ad4b39dea82446a33c9d1fc82b4` |

The Windows exclusion is `TestLoadBrowserCookiesPersistsAndReloads`: Windows
reports mode 0666 instead of POSIX 0600. The test is not excluded from Linux CI;
it also passed separately on Linux during the lab task.

Go 1.26.4, Linux amd64, CGO=0. Reproducible flags:
`-trimpath -buildvcs=false -ldflags='-s -w -buildid='`.
Volga additionally uses `-tags volga` and `-X main.version=v4.1.0-rc1`.

- Legacy SHA-256: `c905b9d0ef5901f4c4dfd574caa3f54c20f91763f6f9044edbabcb8f51b334e5`.
- Volga SHA-256: `116e9b2b17edc3510b3db3278eec0ed769b70fed852c666a835cc446587dede4`.
- [Panel fingerprints and machine-readable evidence](VOLGA-INTEGRATION-2026-09-28.json).

Linux tests ran as unprivileged `ubuntu` in a unique `/tmp/openflux-offline-tests-*`
directory on PC2. Host config reads were isolated and service/network mutations
mocked. A pre-existing rollback test was corrected to redirect all support paths
into its fixture: the first unprivileged run correctly denied a write to the
real panel. Temporary files were removed. No actual panel, service or route was
modified. Read-only audit confirmed Legacy and bridge units are disabled and
controller-managed on both routers; bridge dependencies do not start Legacy.

Final code `eab50c6` passed full Linux tests (including the POSIX permission test),
race, vet and both container smokes in
[CI run 36415351502](https://github.com/LevL-max/OpenFlux-mod/actions/runs/36415351502).
All 38 integration tests passed in 5.196 seconds. The CI Volga container was saved,
loaded from its archive, checked by image ID and started under resource limits.
Both downloaded CI executables match the local SHA-256 values above exactly.
[PR #19](https://github.com/LevL-max/OpenFlux-mod/pull/19) tracks review and final
head checks; subsequent documentation-only commits do not change these binaries.

## Live evidence and remaining gate

The [completed lab check](VOLGA-LAB-ROUTER-2026-09-28.md) held 64 SOCKS connections,
verified all 64 transfers by SHA-256, denied internal destinations and passed
half-close/recovery. Its isolated VPS units and files were removed.

That lab binary is distinct from this integration candidate. Actual RC update,
cookie refresh and Legacy ↔ Volga switching on PC2/AWS have **not yet** been
validated live. Main and deployed installations remain unchanged. PR CI must
validate this exact source; after review/merge, the release workflow creates a
draft RC. A short functional RC test remains before stable rollout. No new speed
experiments or long throughput soak are required.

## Existing AWS and two-client deployment gate

A subsequent read-only SSH audit confirmed the actual deployment topology:
AWS `3.8.0.35`, PC1 `192.168.1.132`, PC2 `192.168.1.74`. Both mini-PC updater
settings specify that AWS exit IP, and the document identity hashes on all
three nodes match. The earlier lab VPS pair is not a deployment target.

AWS has two logical CPUs and 1,998,610,432 bytes of RAM; 1,308,508,160 bytes were
available at the sample. This is an inventory observation, not a load/capacity
test. `openflux-yandex-exit.service`, its Legacy container and
`openflux-recovery-inbox.timer` are active. Existing recovery keys and Disk token
are present. Volga config, updater state and container are absent. The Legacy
container PID stayed 131725 across both read-only samples. No AWS files,
containers, units or network settings were changed.

**Release blocker for the requested two-client topology:**
`volga/internal/tunnel/endpoint.go` owns one record connection and one yamux
session. In `volga/internal/recordconn/conn.go`, an authenticated hello from a
different peer after readiness causes `ErrPeerRestart`. The managed adapter also
uses one fixed Volga config/state/container per server. Therefore the green
single-pair tests do not establish simultaneous support for PC1 and PC2. Sharing
one current Volga config between both clients can disrupt the first session.

Resolve this through isolated per-client instances on the same AWS or explicit
multi-client server support, with corresponding update/recovery tests and a
short concurrent functional check before release. Existing recovery keys can
still be reused; transport session isolation is a separate concern. No further
throughput tuning is needed or authorized by this finding.
