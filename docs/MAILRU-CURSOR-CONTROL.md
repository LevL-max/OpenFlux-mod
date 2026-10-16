# Historical Mail.ru cursor control

This branch adds `--mailru-cursor-probe=true` for a bounded comparison with
the current chat carrier. Both test endpoints must select it. It uses the
historical `type: cursor` / `length;Base64` wire representation and keeps current
browser authentication, heartbeat, document-key refresh and bounded queues.
Synthetic document edits remain disabled. Chat remains the default; deployment
templates and installed release settings are unchanged.

Do not combine this control with experimental chat pacing or session renewal.
Use an exact committed CI artifact in temporary runtimes with automatic cleanup.
First verify a small HTTPS transfer. Only if it succeeds, perform one bounded
32 MiB download, recording complete bytes, elapsed time, application throughput,
session failures and queue drops. Stop after failure; do not tune settings in a
loop. The target is at least 2,000,000 application bytes/second. A successful
control is not a router/LAN readiness or long-term stability claim.
