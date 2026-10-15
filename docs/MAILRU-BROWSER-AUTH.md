# Mail.ru browser authentication

A Mail.ru public document with editing enabled may still require a login.
OpenFlux can import a successful browser editor request. Anonymous documents
continue to work without an import.

## Router Panel v10

Open the configured public document in the browser, sign in, and open the editor.
In developer tools → Network, reload and select the public document page,
then choose **Copy as cURL (bash)**. OpenFlux obtains the editor CSRF token
automatically with the same cookie jar, including cookies returned by that page.
A copied POST to `https://cloud.mail.ru/api/v4/r7/edit`, including its JSON body,
also works.
Paste it into **Mail.ru browser cookies** and choose its document.

- **Save on this mini-PC only** stores the browser profile locally.
- **Send to server only** uploads an encrypted package through **Yandex Disk**.
  It does not import that session on the mini-PC.
- **Download encrypted server file** creates the same package for manual upload
  to the paired server's recovery folder.

Use separate browser sessions for the mini-PC and server. Do not post cURL text
in an issue or commit it to the repository. The panel clears the input after a
successful operation and never includes cookie values in a response or error.
The Configuration card includes **Mail.ru browser cookies** after the first import.

The panel shows the local and signed server state separately. An HTTP 401/403
from the editor API becomes `AUTH_BLOCKED`; automatic requests pause until a
different profile is imported. The last local cookie refusal stays visible after
the client stops. Saving new cookies marks them as awaiting the next check.

## Native CLI and recovery

First-time server setup keeps an authenticated document configured while waiting
for its cookie import. Existing server configuration changes and native release
health checks still fail and roll back if Mail.ru cannot authenticate.

```
sudo openfluxctl cookies import --transport mailru --document-url PUBLIC_LINK --file /private/request.txt
sudo openfluxctl cookies send --transport mailru --document-url PUBLIC_LINK --file /private/server-request.txt
sudo openfluxctl cookies package --transport mailru --document-url PUBLIC_LINK --file /private/server-request.txt --output /private/mailru-recovery.json
```

Existing RSA server keys, Ed25519 client keys and the Disk OAuth token are reused.
Mail.ru uses its own authenticated envelope schema and target, and an inbox named
`<recovery-base>-mailru-<document-sha256-prefix>.json`. Packages for other protocols
or documents, expired packages, untrusted signatures and replayed IDs are rejected.
The existing recovery timer checks a blocked server about every two minutes, or
every fifteen minutes otherwise. A rejected Disk token stops requests until it
is replaced. Signed status retains its existing `-mailru.json.status.json` path.

The imported profile is `/var/lib/openflux-mailru-auth/browser.json` (0600 in a
0700 directory). Client ownership follows the configured runtime account. The
server container mounts the **directory** read-only so atomic imports become
visible without recreating the container. `--mailru-browser-profile` watches
changes and reconnects automatically. Cookie updates received from the service
stay in the running HTTP cookie jar; an expired browser login needs a new import.

Only the configured public page and fixed Mail.ru editor endpoint receive browser credentials. Imported
cURL is parsed, never executed. Redirects are not followed. Cookies and sensitive
headers are not forwarded to the editor WebSocket host. Request error messages
do not include token-bearing query strings.

## Compatibility and verification

Update both ends before using authenticated Mail.ru. Authenticated chat messages
use an `OFX2` process marker so two browser sessions belonging to the same account
can exchange traffic without mistaking each other for an echo. Anonymous `OFX1`
traffic remains compatible with the previous chat transport. Cursor-era Mail.ru
peers remain incompatible with chat traffic.

Panel versions 1–9 migrate to v10. The integration stays within existing archive
members, preserving the native updater's bundle allowlist and rollback flow.
Legacy and Volga credential stores are independent of the Mail.ru profile.

Tests cover profile validation, credential isolation, refusal/reload, encrypted
recovery, replay protection, retained status, panel migration, and real TCP through
a local authenticated chat relay with identical provider account IDs and a forced
reconnect. A live service check still requires fresh browser imports on both ends.
