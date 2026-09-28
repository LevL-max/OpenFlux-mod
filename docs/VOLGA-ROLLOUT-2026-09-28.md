# Volga RC2: installed rollout results

[v4.1.0-rc2](https://github.com/LevL-max/OpenFlux-mod/releases/tag/v4.1.0-rc2)
is published as a prerelease and installed on the existing AWS and both mini-PCs.
Stable remains **v4.0.6**. Release source:
`bef15276c23d4cf324044aedc8dabcde1f0a137b`.

| Node | Installed Volga | Final selection / runtime | RAM / CPU limit |
|---|---|---|---|
| AWS | v4.1.0-rc2 | One active Volga container alongside unchanged Legacy | 256 MiB / 50% of one CPU |
| PC1 | v4.1.0-rc2 | Legacy selected; Volga stopped | 256 MiB / 50% of one CPU |
| PC2 | v4.1.0-rc2 | Legacy selected; Volga stopped | 256 MiB / 50% of one CPU |

All Volga runtimes have zero swap and a 128-task limit. Actual Docker/systemd
settings were read back, including the server's bridge network, read-only root,
dropped capabilities and no published ports. One document pair and matching
transport key are shared between PC1 and PC2 **for sequential use only**.
Existing RSA/Ed25519 recovery keys and Disk authorization were retained.

## Checks completed

| Check | PC1 | PC2 |
|---|---|---|
| Customized grouped panel retained | PASS | PASS |
| Volga config download/upload roundtrip | PASS | PASS |
| Install/update requested through real panel API | PASS (RC2) | PASS (RC1 and RC2) |
| Legacy -> Volga -> Legacy with SOCKS/exit-IP health checks | PASS | PASS |
| RC2 HTTPS body, TLS verified | 559 bytes | 559 bytes on retry |
| RC2 HTTPS SHA-256 | MATCH | MATCH |
| Two encrypted document packets sent through panel/Disk | PASS | PASS |
| Signed AWS status verified through Disk | connected | connected |

HTTPS SHA-256 on both clients:
`ff67a9d764d6a2367a187734e697f6a53217db9a21c101d410a113ca871a299d`.
Installed Volga binary SHA-256 on all three nodes:
`e0f794e37006675b68cbba03cf80074816200176a09f6f3b538c02044374bda3`.
The downloaded release bundle and executable hashes matched the exact source
and local build before publication/installation.

AWS received four distinct signed/encrypted recovery packets (two from each
client) and applied both document inboxes. Repeated polling did not reapply the
same packet IDs. One manual PC2 recovery poll returned an error; a subsequent
poll verified the connected server status. No new key set was generated.

The first final RC2 HTTPS probe on PC2 failed after a successful exit-IP health
check. The initial harness did not retain curl's exit code/error output, so its
cause is undetermined. Repeating the same 20-second probe without configuration
changes succeeded and matched SHA-256. This is **not a zero-error rollout** and
does not justify promoting RC2 to stable. No long reliability claim is made.

The earlier [64-connection functional check](VOLGA-AWS-FUNCTIONAL-2026-09-28.md)
passed 32 uploads and 32 downloads per client with all SHA-256 values matching,
65th-stream rejection, public HTTPS, private-address denial, half-close and abort
recovery. RC2 changes only the integration adapter and version string; Go
transport source and the frozen profile are unchanged. No further throughput
experiments or long soak were run.

## Rollback and the PC1 compatibility fix

The server CLI and PC2 panel completed installation, explicit rollback and
reinstallation. PC1's first RC1 installation automatically rolled back because
its optional `router-runtime-ensure` helper delegates to the main controllers
without containing a fixed client-unit reference. Protected state after that
failure matched the original state. RC2 leaves that dispatcher unchanged and
patches its actual controllers. The updated adapter was also checked against
copies of all actual PC1 helper files before the successful RC2 installation.

PC1 received the verified RC2 support bundle before retrying its incomplete
initial installation. AWS and PC2 upgraded through their Volga updaters.
RC1's published files remain unchanged and its notes direct users to RC2.

Updates now install support modules and panel changes transactionally. Recovery
uses a complete copy of the previous support code, and the panel restarts only
after the transaction returns. Injected partial-copy and failed-health cases are
covered by the 42-test Python suite. Full Go race/vet and container checks also
passed in [RC2 CI](https://github.com/LevL-max/OpenFlux-mod/actions/runs/36426444230)
and the [exact release build](https://github.com/LevL-max/OpenFlux-mod/actions/runs/36426784386).

## Preserved state and cleanup

Final checks matched the original Legacy executable hashes, Legacy service
states/PIDs, AWS Legacy container identity/PID/start time, recovery keys, router
modes and IP rules. Existing LAN traffic modes were not switched during this
rollout; live transport checks used the local SOCKS service. These results do
not claim a new whole-LAN routing or throughput test.

Temporary remote staging directories and unused downloaded candidate caches
were removed. Installed runtime files and rollback checkpoints remain. AWS
retains its stopped previous Volga container for rollback; only one Volga
instance is running. No earlier lab VPS was used for deployment.

Legacy Go source remains unchanged. Reproducible builds with `-buildvcs=false`
match the baseline. Published Legacy assets have different hashes because the
release builds record their new Git revision; the installed Legacy executables
were not replaced during this rollout.

## Operation

On the chosen mini-PC, open the OpenFlux protocol card, choose **Volga**, and
press **Apply protocol**. Connection and AWS exit-IP checks must pass. Then use
the existing OpenFlux router-mode control. Stop OpenFlux/Volga on the old mini-PC
before starting it on the other; an idle running client still owns the session.

For expired cookies, select each document in the Volga card, paste its browser
Copy as cURL, save client cookies and send server cookies via Disk. Repeat for
the other document. The existing recovery timer applies signed packages and
the runtime reloads credentials. Config import/export and Volga update/rollback
controls are separate from Legacy. See [instructions](../integration/README.md)
and [sanitized evidence](VOLGA-ROLLOUT-2026-09-28.json).
