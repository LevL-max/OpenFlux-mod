# OpenFlux updater, authentication and recovery

This directory is versioned with OpenFlux. The release includes `openflux-integration-linux.tar.gz`
and `openflux-recovery-windows.zip`, verified by the same `SHA256SUMS` as the binary.
No cookies, account tokens, private keys or deployment addresses are included.

## Existing Router Panel installations

After verifying and extracting the release archive, run:

```sh
sudo python3 integration/install.py --role client --expected-exit-ip YOUR_SERVER_IP
```

The installer supports the existing Router Panel / router-updater layout and refuses unknown
source layouts before writing. It preserves device-specific runtime hooks, routing modes and
cookie stores. It creates a complete support checkpoint in `/var/backups/openflux-integration`.
It does not install a router OS or provision a new tunnel.

Use the normal **Check → Download → Install** controls. Versions are compared numerically,
including rc10 after rc9 and stable after RC. `openflux_prereleases` in
`/etc/router/updater/settings.json` selects the release channel. Nothing is pinned to RC2/RC3.
The updater verifies GitHub asset digests and the release manifest, then saves binary, runner,
helper and integration modules together. A failed authentication/SOCKS/exit check restores the
checkpoint. Normal Install rejects downgrades; use **Rollback** for a local checkpoint.
New integration modules in later releases are installed along with the binary. A new transport
still needs transport-specific setup; publishing its binary does not automatically configure it.

The OpenFlux card shows Connected, Connecting, AUTH_BLOCKED or Authentication failed and
retains the last authentication error. Status polling continues with the panel closed.
Expand **Update Yandex cookies**, paste **Copy as cURL (bash)** from the public document's
request in browser DevTools, then save. The command is parsed as data and never executed.
Cookies are stored per document with mode 0600; a blocked connection resumes automatically.

## Server recovery

Install Python 3 and `python3-cryptography` on the Docker host. The default deployment uses
container `openflux-yandex-exit` and store `/var/lib/openflux-yandex/yandex-cookies.json`.
Configure the document and its initial cookie store during normal server provisioning.

Create an OAuth app with **Yandex Disk read-only** permission. The API grants account-wide
read access, although this receiver reads only the configured file. Do not use a browser's
public-folder session as the only recovery route; it can require CAPTCHA.
Pair with the Windows tool's public signing key:

```sh
sudo python3 integration/install.py --role server \
  --disk-token-file /protected/disk-token \
  --sender-public-key /protected/sender-public.pem \
  --disk-path 'disk:/OpenFlux Recovery/aws-recovery.json'
```

Keep the token file private. Copy `/etc/openflux-recovery/server-public.pem` to the Windows
tool. The RSA private key stays on the server. Subsequent support updates use just
`sudo python3 integration/install.py --role server`; existing pairing and configuration remain.

The Windows tool accepts fresh browser cURL and either imports over SSH or creates
`aws-recovery.json`. Upload this file to **OpenFlux Recovery** in your own Yandex Disk and
replace the previous file. The receiver polls every minute. Packets are encrypted for the
paired server, signed, expire after one hour and are applied once. Only cookies can be changed.
Neither a restart nor inbound SSH access is needed for this route.

`sudo openflux-auth status` reports transport and recovery-channel status without cookie values.
Disk authorization failures and CAPTCHA are visible separately; failures back off to 15 minutes.
OAuth/API access can still be revoked or blocked: it is an independent recovery channel, not a
guarantee against all provider restrictions. Renew the read-only token if DISK_AUTH_REQUIRED appears.

## Windows tool

Requires Python 3.10+; install dependencies from `desktop/requirements.txt`.
Run `desktop/setup_windows.py --help` for pairing and host configuration. It stores the signing
key with Windows DPAPI and pins SSH host keys from the supplied known_hosts file. Mini-PC
passwords are entered only when used and are not saved. The UI listens only on localhost.

## Tests

```sh
PYTHONPATH=integration python3 -m unittest discover -s integration/tests -v
go test ./...
```
