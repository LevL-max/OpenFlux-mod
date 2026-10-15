# Mail.ru chat carrier backport

Base: downstream v4.2.3, `e6b934038287256da3dbbe6d28ca26898919c259`.
Adapted from upstream `4ee9614c802e92a57a8be7300ed3d25f94f2477f`
(chat channel) and the batching rationale in
`fd5b7c3ad7f186826dadccba82bd7bdc2ca9cb38` (Mail.ru message-count budget).

## Behavior

Mail.ru's editor rejects synthetic cursor/saveChanges writes. Send `OFX1`-tagged
Base64 in chat `message` events and accept only live chat broadcasts. Ignore
our own echoes, human chat, malformed payloads, cursor events and auth history.
Stop sending cursor keepalives and reject `--mailru-editor-activity=true` before
opening a connection. Engine.IO ping/pong provides the heartbeat.

Keep downstream authorization checks, generation-specific socket teardown,
single reconnect scheduling, document-key refresh, status reporting and bounded
queues. A successful authChanges acknowledgement releases chat writes; an older
editor that omits authChanges gets a three-second grace period after successful
editor authorization. Retain queued data for at most 15 seconds / 8 MiB: the
previous three-second age limit expired packets during bootstrap plus that grace
period. This does not guarantee packet delivery; TCP handles loss and duplicates.

Only Mail.ru defaults change: 512 packets, 500,000 payload bytes and a 5 ms batch
flush delay. Explicit CLI packet/delay settings still win. The existing OFB1/LZ4
codec is retained. The byte target is not an absolute frame-size limit; the last
packet, OFB1 framing and Base64 add overhead. The integration profile supplies
these settings and rejects older binaries that lack `--mailru-batch-bytes`.

## Compatibility and rollout

- Chat peers cannot communicate with v4.2.3-or-earlier cursor peers.
- Update the server and client as a coordinated pair. Stage both artifacts before
  selecting Mail.ru; keep Legacy/Volga available during the transition.
- Both peers must agree on whether batching and compression are enabled. This
  fork's OFB1/LZ4 codec is not upstream's Session/zstd wire format.
- Use a dedicated document: tagged data is visible in its chat.
- Roll back both peers together. A rollback to the cursor carrier may still fail
  against editor servers that reject cursors; use Legacy/Volga in that case.
- No release number, deployment, production service or remote main branch is
  changed by preparing this branch.

## Validation

On 2026-10-15, using a user-provided disposable public document (URL deliberately
omitted), two local peers connected independently to the actual Mail.ru service:

- Tagged-packet probe: 1,500, 32,768 and 500,000 bytes in both directions, followed
  by a forced WebSocket reconnect and sixteen alternating 128,000-byte transfers.
  Every expected payload arrived intact; observed reconnect counts were 3 / 2.
  The probe retries at the application level and is not a throughput benchmark.
- Full TCP probe: gVisor TCP through the actual OFB1 -> LZ4 -> Mail.ru chat path,
  524,288 random bytes in each direction, exact byte equality. A WebSocket was
  forcibly closed halfway through; the same TCP connection completed without
  application retries. Transfer phase: 19.099 seconds including recovery;
  reconnect counts: 1 / 0; no queue drops or batch decode errors observed.
- Unit and race tests cover readiness/ack failure, invalid frames, self-echo
  filtering, queue expiry, stale generations, editor authorization and key refresh.
- A local WebSocket relay runs the same TCP probe in ordinary CI, without network
  service credentials. It requires authChangesAck and rejects non-chat data writes.

The full Linux suite (including Unix file permissions, Python integration and
container smoke tests) runs in the PR's existing CI. Windows cannot run the Linux
Python integration modules (`fcntl`/`pwd`), and the existing Yandex cookie-mode
test expects Unix permissions; these are not changes to those tests.

To repeat the optional service checks, set `OPENFLUX_MAILRU_TEST_DOCUMENT` locally
to a disposable document and run:

```sh
go test -run '^TestLiveChat' -count=1 -v -timeout 8m ./transport/mailru
```

Without that environment variable both live tests skip. Do not publish the URL
or add it to CI. These short service tests confirm function and reconnect, not
long-term availability or a guaranteed data rate. Mail.ru can still disconnect
anonymous chat sessions.
