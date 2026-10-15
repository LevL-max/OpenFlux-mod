# Mail.ru disconnect diagnosis

`[M-SESSION]` is one JSON summary for each opened WebSocket, emitted on exit
from that socket before reconnect backoff. It uses the existing status-event
logging switch. No debug mode is needed. This is instrumentation, not a claim
that Mail.ru reliability or throughput has been fixed.

The summary records only numeric metadata and fixed local categories. It never
records document URLs, cookies, tokens, account IDs, chat contents, arbitrary
server event names, WebSocket close text or raw exception text. Socket IDs are
local monotonic counters and restart with the process. There is no per-packet
log or retained packet history. Existing `[M-PERF]` records still provide queue
drops, queue bytes, aggregate write errors and reconnect counts.

## Reading a summary

| Fields | Interpretation |
| --- | --- |
| `id`, `phase`, `age_ms`, `ready_ms` | Socket identity, last connection phase, lifetime and time since editor authorization. An age of -1 means the event was never observed. |
| `cause`, `error`, `close_code` | First observed failure or local close action. Later errors caused by closing the socket do not overwrite it. `read` / `websocket_close` / `1005` means an empty close frame; it does not identify the server's reason. `1006` can mean an abnormal transport loss and is not proof of a server-sent close frame. |
| `ping_interval_ms`, `ping_timeout_ms`, `heartbeat_ms` | Server-advertised heartbeat intervals and the client's resulting read-deadline duration. |
| `pings`, `pongs`, `last_ping_ago_ms`, `max_pong_ms` | Engine.IO heartbeat observations and maximum pong write time, including time waiting for the write lock. A successful write is not proof that the server received the pong. |
| `rx_messages`, `rx_bytes`, `tx_messages`, `tx_bytes` | WebSocket application-message totals, including handshake/control traffic. Not TCP goodput, compressed wire bytes or link-speed measurements. |
| `last_tx_bytes`, `max_tx_bytes`, `failed_tx_bytes` | Last/largest successful application message and the failed write size, if any. Sizes include the Socket.IO wrapper and Base64 expansion. |
| `max_write_ms`, `max_write_wait_ms`, `last_rx_ago_ms` | Maximum socket-write time, write-lock wait and age of last received application message. |
| `last_control`, `terminal_control` | Recognized Engine.IO close or Socket.IO disconnect/connect-error packets. The last terminal packet is preserved even if a ping follows it. Observation does not change reconnect behavior. |
| `editor_event`, `editor_code` | Last recognized warning/error/drop/disconnectReason event and optional integer code. Text is discarded. Oversized control frames and chat/history messages are not parsed for this field. |

`stop`, `profile_changed`, `document_key_changed` and `browser_auth` identify
locally initiated closes. They must not be counted as unexplained peer failures.
The first recorded cause is an observation, not proof of root causality: a read
and write can fail concurrently. Short-lived sessions are logged individually;
the older human-readable drop notice remains throttled to once a minute.

## Bounded service experiment

Prepare changes through Git and CI, then use a release and the native updater.
Do not copy patched binaries onto running nodes. A production router is outside
this experiment. A test-router deployment or mode change requires authorization
if the operator has instructed that node to remain unchanged.

Use a disposable document and independent browser sessions for the two endpoints.
Import credentials through the existing panel. Do not enable raw payload logging
or include credentials or private node/document addresses in a public report.

1. Record the release and current mode. Observe both endpoints idle for five
   minutes, with no browser editing that document and no benchmark traffic.
2. If idle is stable, exchange small messages at low frequency. Change only one
   dimension at a time: first payload size, then message frequency. Stop on
   repeated disconnects, authentication refusal or explicit service throttling;
   increasing concurrency is not a remedy for a service limit.
3. Compare `[M-SESSION]` and `[M-PERF]` records on both endpoints with synchronized
   timestamps. Separate local stops/key refreshes from unexplained closes. Check
   heartbeat responsiveness, write stalls, message sizes and queue expiry.
4. Only after a stable transport trial, test actual LAN traffic and mode/protocol
   changes on the authorized test router. Local SOCKS probes alone cannot certify
   this path. Record delivered bytes/time, failures and reconnects throughout a
   sustained run, not just a single passing request.

An empty close with responsive heartbeats and small messages still does not prove
rate limiting. If no cause is visible after this bounded comparison, retain the
transport as experimental rather than guessing at cookies, quotas or timeouts.

Protocol references: [Engine.IO heartbeat and packet types](https://socket.io/docs/v4/engine-io-protocol/)
and [Socket.IO disconnect/connect-error packets](https://socket.io/docs/v4/socket-io-protocol/).
