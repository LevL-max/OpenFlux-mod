# Install and update OpenFlux

The release installs **OpenFlux itself**, on a client or an exit server. It does not install
a router OS, reproduce a particular Mini-PC configuration, or change existing router modes.
No deployment addresses, SSH credentials, cookies or OAuth tokens are included.

Supported release platform: **Linux amd64, Python 3.10+, systemd**. A new client runs a native
SOCKS service. A new server uses Docker with its own network namespace. Existing native or
Docker deployments can be adopted without restarting them. Future transports are delivered
in normal releases; their settings remain an explicit configuration choice.

## 1. Obtain the installer

On the target machine, download `openflux-node.py` and `SHA256SUMS` from the same GitHub release:

```sh
curl -fLO https://github.com/LevL-max/OpenFlux-mod/releases/latest/download/openflux-node.py
curl -fLO https://github.com/LevL-max/OpenFlux-mod/releases/latest/download/SHA256SUMS
grep '  openflux-node.py$' SHA256SUMS | sha256sum --check -
```

Proceed only after the checksum succeeds. For an exact version, replace `latest/download`
with `download/vMAJOR.MINOR.PATCH` in both URLs. Release assets are verified again by the
installer using GitHub asset digests and the manifest before installation.

On Debian/Ubuntu install the prerequisites, if missing:

```sh
sudo apt-get update
sudo apt-get install python3 python3-cryptography curl
# Server only; an existing Docker installation can be used:
sudo apt-get install docker.io
sudo systemctl enable --now docker
```

## 2. Choose client or server

Client:

```sh
sudo python3 openflux-node.py install --role client \
  --document-url 'YOUR_YANDEX_DOCUMENT_URL' --socks 127.0.0.1:1080
```

Server:

```sh
sudo python3 openflux-node.py install --role server \
  --document-url 'YOUR_YANDEX_DOCUMENT_URL'
```

Both ends must use the intended document/transport settings. Import browser cookies if
required, then start:

```sh
sudo openfluxctl cookies import --file browser-curl.txt
sudo openfluxctl start
sudo openfluxctl status
```

`browser-curl.txt` contains **Copy as cURL (bash)** from the configured document's browser
request. It is parsed as data and never executed. `openfluxctl menu` also accepts pasted
multiline cURL directly, ending with `END` on its own line. No Windows application is involved.

The client listens on loopback by default. The server's RST suppression stays inside its
Docker network namespace. The installer does not change host router routes or firewall rules.

## 3. Normal operation and updates

```sh
sudo openfluxctl menu
sudo openfluxctl check
sudo openfluxctl update
sudo openfluxctl rollback
```

The menu offers status, release checks, update, rollback, cookie import, Disk setup and restart.
`update` downloads and verifies the newest release in the chosen channel, checkpoints the
binary and support tools, restarts OpenFlux, checks health and restores the previous release
on failure. An independent systemd timer restores the checkpoint if the update is interrupted.
The previous running/stopped state is preserved. Updates are explicitly requested; polling
authentication or Disk does not install a software release.

```sh
sudo openfluxctl configure --channel stable
# To include release candidates:
sudo openfluxctl configure --channel prerelease
```

Versions are compared numerically, including RC versions. Nothing is pinned to RC2, RC3 or
a particular stable version. An integration-only release with an unchanged Go binary also
counts as an update. A future transport's options can be supplied as a JSON argument array:

```sh
sudo openfluxctl configure --transport TRANSPORT --args-file arguments.json
sudo openfluxctl restart
```

The new release must actually support that transport. The updater does not invent tokens or
enable new protocols automatically. Native services and new Docker installations use the
configured arguments. Adopted installations retain their original service/container arguments;
change those using their established deployment configuration when switching transport.

## 4. Optional Yandex Disk backup channel — any installation

On an existing Router Panel, the integration installer also extends **Configuration
Files** with individual Download / Upload entries for the node profile, updater
settings, Disk OAuth token, server public key, client private/public signing keys
and client cookie store. Existing bridge, document URL and transport entries have
short descriptions. Only installed files are listed. Token downloads use the name
`disk-token.txt`; key files use PEM.

Uploads validate the format, reject stale revisions and save backups under
`/var/backups/router-configs`. Document URL changes synchronize the runner env and
node profile; release-channel changes synchronize the profile and router updater.
Managed executable/service paths and settings of unrelated components cannot be
changed through the OpenFlux profile upload. Importing a client private key derives
its matching public key; the server must trust that public key. A public-key-only
upload must match the existing private key. Tokens, private keys and cookies are
secret files, downloaded only through the panel's existing LAN access policy.

Uploads do not restart services. Document/transport/bridge changes need an explicit
OpenFlux restart. Recovery settings are read by subsequent operations, and a blocked
client reloads changed cookies automatically. No server SSH configuration is added.
For an existing older panel, run the integration installer once (`install.py
--role client`) to add the UI hooks; future support updates preserve these hooks.

This feature is available to a newly installed client/server, not just a Router Panel extension.
Use it when inbound SSH to the server is unavailable. Both nodes still need outbound access
to the Yandex Disk API. The document's browser cookies and the Disk OAuth token are separate
credentials: renewing one does not renew the other.

1. Create/select a Yandex OAuth application for Disk and authorize your own account.
2. For automatic upload **and server status**, grant Disk read and write access. Read-only
   access supports server downloads but cannot upload cookies or publish its status.
3. Create a folder such as `OpenFlux Recovery` in your Disk. Choose a shared file path,
   for example `disk:/OpenFlux Recovery/server-recovery.json`.
4. Put the OAuth token in a root-readable file on each node that will use the API. Token
   values must not be placed in shell arguments or committed to GitHub.

Official API and OAuth entry point: https://yandex.com/dev/disk/rest/
Provider authorization may require a login/CAPTCHA. Token creation is an explicit setup step;
the installer does not manufacture credentials or silently request account access.

Generate the client signing key:

```sh
sudo openfluxctl recovery --disk-path 'disk:/OpenFlux Recovery/server-recovery.json'
```

Copy the resulting `/etc/openflux-recovery/sender-public.pem` to the server (public key only):

```sh
sudo openfluxctl recovery --sender-public-key /path/to/sender-public.pem \
  --disk-token-file /protected/disk-token \
  --disk-path 'disk:/OpenFlux Recovery/server-recovery.json'
```

Copy the server's `/etc/openflux-recovery/server-public.pem` back to the client:

```sh
sudo openfluxctl recovery --server-public-key /path/to/server-public.pem \
  --disk-token-file /protected/disk-token \
  --disk-path 'disk:/OpenFlux Recovery/server-recovery.json'
```

These are installation parameters, not built-in machine names. Verify public-key fingerprints
through your trusted installation session. Private keys stay on their own nodes. Repeat the
server's `--sender-public-key` step to trust another client; existing trusted clients remain.

Automatic upload from the client:

```sh
sudo openfluxctl cookies send --file browser-curl.txt
```

Or create a file on the client and upload it manually through the Disk website:

```sh
sudo openfluxctl cookies package --file browser-curl.txt --output server-recovery.json
```

Use the exact configured filename and replace the previous file. Packets are signed, encrypted
for the paired server, valid for one hour and applied once. They contain cookies, not commands.
The server polls approximately once a minute. A blocked connection reloads fresh cookies
without restarting the OpenFlux service. CAPTCHA may still require fresh browser cookies.

The server also signs its status and publishes `<cookie-file>.status.json`. The client verifies
that signature using the paired server key. Status older than three minutes is shown as stale,
never as a fresh successful connection. A missing Disk response is a channel failure, not proof
that client or server authentication failed. Revoked/expired OAuth tokens require reauthorization.

## 5. Existing Mini-PC Router Panel installations

Keep the current Router Updater and routing modes. After verifying and extracting
`openflux-integration-linux.tar.gz`, install the optional compatibility hooks:

```sh
sudo python3 integration/install.py --role client --expected-exit-ip YOUR_EXIT_IP
```

This refuses unknown Router Panel/core layouts before writing and preserves host-specific
hooks. New standalone OpenFlux clients do not require Router Panel.

The existing **Check → Download → Install** buttons continue to update OpenFlux. The card
shows client authentication and the last client failure. When Disk pairing is configured, it
also shows the separately verified server authentication, last server failure and response time.
The cookie form can save local cookies, download the encrypted server recovery file, or upload
it directly through the Disk API. The API upload button needs a configured write-capable token.

To attach the standalone CLI to an existing installation, use `adopt` with its actual paths:

```sh
sudo python3 openflux-node.py adopt --role client --backend systemd \
  --binary /path/to/existing/openflux --library /path/to/integration/modules \
  --service YOUR_CLIENT.service --cookie-store /path/to/cookies.json \
  --document-url 'YOUR_DOCUMENT_URL' --router-updater
```

For an existing server use `--role server --backend systemd --service YOUR_SERVER.service`
when systemd starts the process or Docker container. Use `--backend docker --container NAME`
only for a persistent container managed directly by Docker. A service-owned `docker run --rm`
container must be controlled through its systemd service, which recreates it on start.
Docker adoption requires the configured host binary mounted at `/usr/local/bin/openflux` in the
container. Adoption records the verified installed release and adds CLI support; it does not
restart the runtime. The `--router-updater` option delegates software updates to the existing
client updater so there are not two competing update histories.

## Files and checks

- `/etc/openflux/node.json`: node configuration; no deployment addresses are compiled into code.
- `/etc/openflux-recovery/`: private credentials and pairing keys, restricted to root.
- `/var/lib/openflux-updater/`: staged releases, installed version and rollback checkpoints.
- `/var/lib/openflux-auth/status.json`: current authentication and retained last failure.
- `/var/lib/openflux-recovery/`: inbox result and the client's last verified server status.
- `openflux-auth-status.timer`: local status; `openflux-recovery-inbox.timer`: optional Disk channel.

Server health checks verify Yandex authentication. Client checks also exercise HTTPS through
the SOCKS listener. Other transports require their own authentication events; until available,
the generic server check only verifies that the runtime stays running. This is reported as a
limitation, not proof of end-to-end connectivity for a future protocol.

Run the focused tests with `PYTHONPATH=integration python3 -m unittest discover -s integration/tests -v`.
