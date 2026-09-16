# Volga V6 design - one logical reliable stream over recyclable Yandex carriers

Status: design baseline for the next implementation. No Production deployment and no live benchmark changes are part of this document.

## Problem statement

The current Volga experiments have established two separate facts:

1. The Yandex Volga carrier is capable of high short-term goodput. Existing experiments reached roughly 30-34 Mbps on the current Linux client/exit topology.
2. HTTP 200/204 from the Volga relay is not an end-to-end delivery acknowledgement. Under sustained load, logical batches can be accepted by the relay but fail to appear on the peer websocket within the observation window. The resulting holes can trigger TCP collapse and, in V5, retransmission amplification.

V5.5 also showed that creating a fresh Yandex authorization/session shortly before handoff is not sufficient when the logical reliability state is still owned independently by each carrier. The next design must therefore separate logical reliability lifetime from physical Yandex session lifetime.

## V6 goal

Provide one continuous OpenFlux reliability domain that survives Yandex carrier replacement.

Logical state must remain stable across a carrier change:

- logical session ID
- next DATA sequence
- replay buffer
- cumulative ACK/SACK state
- duplicate suppression
- receiver resume state
- congestion/recovery state

Physical state is disposable and may be replaced at any time:

- Yandex document URL
- authorization data
- request-path
- cookies
- frontier
- HTTP client
- websocket
- physical generation number

A logical DATA batch first sent through carrier generation N must be replayable through generation N+1 using the same OpenFlux logical sequence but a fresh Yandex wire operation identity.

## Non-goals

V6 must not change the Mini-PC router mode implementation, AWS service topology, Mihomo, AWG, OpenFlux dynamic bootstrap, editorConfig.user.id handling, OnlyOffice build detection, production nftables, or other transports.

V6 is not a tuning exercise for workers, HTTP shards, batch timeout or retry constants. Those may remain runtime knobs, but the architecture must be correct before another parameter sweep.

## Architecture

The target data path is:

```text
OpenFlux packet stream
        |
        v
packet coalescing / compression
        |
        v
ReliableSession
  sessionID
  nextSeq
  replay
  ACK/SACK
  recovery controller
        |
        v
CarrierManager
  active generation
  prewarming generation
  draining generation(s)
        |
        v
VolgaCarrier generation N
  authorize
  request-path
  cookies
  frontier
  HTTP relay
  websocket
        |
        v
Yandex Volga
```

The inverse receive path is:

```text
Yandex websocket from any live generation
        |
        v
VolgaCarrier decoder
        |
        v
ReliableReceiver
  session validation
  sequence dedupe
  ACK/SACK update
        |
        v
OpenFlux packet delivery
```

## Required Go ownership boundaries

### ReliableSession

Owns all sender-side logical reliability state.

Suggested responsibilities:

- allocate monotonic logical DATA sequence numbers
- create DATA metadata
- retain unacknowledged logical batches
- process cumulative ACK and SACK information
- produce paced repair work
- expose replay depth, oldest-unacked age and ACK progress
- preserve state while CarrierManager replaces physical carriers

Suggested shape:

```go
type ReliableSession struct {
    sessionID uint64
    nextSeq   atomic.Uint64

    mu      sync.Mutex
    replay  map[uint64]*ReplayEntry
    ackBase uint64

    controller RecoveryController
    sender     WireSender
}
```

`WireSender` is an interface implemented by CarrierManager. ReliableSession must never hold a direct pointer to a specific Yandex relay client.

### ReliableReceiver

Owns peer logical session and sequence state independently of a websocket object.

Suggested responsibilities:

- accept DATA from any current or draining physical generation
- reject stale logical sessions
- deduplicate logical sequences across carrier generations
- advance cumulative base
- build bounded SACK ranges
- repeat ACK state when necessary
- retain receiver state if the active websocket changes

The receiver must not be keyed by `*wsListener`. V5 stored receive state per websocket instance, which couples reliability lifetime to carrier lifetime. V6 must store it on the outer logical transport.

### VolgaCarrier

Represents one disposable physical Yandex generation.

Suggested responsibilities:

- perform fresh `authorize()`
- own its HTTP relay client and physical operation counters
- own document frontier state
- own exactly one websocket lifecycle
- encode one logical DATA/ACK record into a fresh Yandex relay operation
- emit decoded logical records to the outer receiver
- report physical health and readiness

It must not own replay, ACK state, logical sequence allocation or retry scheduling.

Suggested state:

```go
type VolgaCarrier struct {
    generation uint64
    docURL     string

    auth  *volgaAuth
    relay *relayWire
    ws    *wsListener

    ready atomic.Bool
    state atomic.Uint32
}
```

### CarrierManager

Owns carrier lifecycle and handoff.

Suggested states:

- `Starting` - initial fresh carrier is authorizing and connecting
- `Active` - carrier accepts new DATA and repairs
- `Prewarming` - replacement carrier is authorizing and opening websocket
- `Switching` - active generation pointer changes atomically
- `Draining` - previous carrier remains receive-only for late websocket delivery
- `Retired` - old carrier is closed and removed

CarrierManager implements `WireSender`:

```go
type WireSender interface {
    SendLogical(kind FrameKind, logicalSession, seq uint64, payload [][]byte) error
}
```

For every call, CarrierManager selects the current active physical generation at send time. Retransmission therefore naturally moves to the replacement carrier without moving the replay entry itself.

## Handoff semantics

A handoff must be make-before-break.

1. Generation N remains Active.
2. CarrierManager creates generation N+1 with a full fresh Yandex authorization.
3. N+1 must complete authorization, relay creation and websocket readiness.
4. Only after readiness does the manager atomically set N+1 as Active.
5. New DATA, ACK control frames and retransmissions immediately use N+1.
6. Generation N becomes Draining. It may still deliver late websocket records into the shared ReliableReceiver.
7. N must not originate new logical traffic while Draining.
8. After a bounded drain interval, or after there is no useful late activity, N is retired.

Important invariant: carrier retirement must never delete logical replay state.

## Carrier replacement trigger

V6 must not rely only on a fixed 15/20/25 second rotation.

The primary health signal is end-to-end logical progress, not HTTP relay success.

Minimum signals:

- `ackBase` movement
- `ackedPerSecond`
- replay depth
- oldest unacknowledged age
- count of timeout repairs
- HTTP request latency
- websocket reconnect/error state

A physical carrier is considered degraded when relay POSTs continue but logical ACK progress stalls beyond a defined threshold while outstanding DATA exists.

Suggested first policy for testing:

```text
if outstanding > 0
and ackBase has not advanced for ProgressStall
and oldestUnackedAge >= ProgressStall
then prewarm replacement carrier
```

A maximum carrier age may later be added only if clean experiments prove a repeatable session-age degradation pattern.

## Congestion and recovery control

V5.1 reduced retransmission storms but still allowed new DATA to continue at full rate while replay debt was increasing. V6 must bound total carrier pressure.

Required controls:

- maximum in-flight logical batches, analogous to a simple congestion window
- separate retry token bucket or retry budget
- oldest-hole-first repair
- one fast repair for clear SACK-visible holes
- exponential timeout repair thereafter
- reduce or pause admission of new DATA when replay debt/oldest-unacked age exceeds thresholds
- resume normal admission only when cumulative ACK progress recovers

The first implementation should favour stability over maximum peak throughput. The target is sustained >=20 Mbps, not the highest single-transfer peak.

## ACK behaviour

ACK must be treated as idempotent state, not a one-shot event.

Receiver should periodically resend the latest cumulative ACK/SACK while recent DATA or unresolved gaps exist. A lost ACK must therefore be repairable by another small ACK frame instead of forcing large DATA retransmission solely because one control operation was not observed.

ACK frames use the same current active physical carrier as DATA and retransmissions, but they remain part of one logical reliability session.

## Operation-rate reduction

Existing telemetry suggests the system can generate hundreds of Yandex relay operations per second before collapse. The next implementation must explicitly measure and reduce `operations/sec`.

The current upstream OpenFlux `BatchedTransport` is useful as a design reference because it coalesces tunnel packets before the concrete transport. V6 should not copy its defaults blindly because Volga has a much smaller practical relay-body ceiling in our tests.

Required behaviour:

- coalesce small tunnel packets before logical sequencing
- retain the current Volga HTTP body guard
- form fewer, fuller logical DATA batches without crossing the safe relay-body limit
- record logical batches/sec and physical relay POSTs/sec

Initial objective: materially reduce physical relay operations at the same goodput before increasing worker concurrency.

## Fresh document discriminator

Existing test documents have accumulated a large collaboration history from repeated benchmarks. Before deciding whether degradation is session-specific or document/history-specific, perform one controlled comparison after V6 simulator correctness is complete:

- existing document + fresh physical authorization
- completely new document + fresh physical authorization

Keep all logical transport settings identical. This experiment is not a V6 acceptance test. It exists only to distinguish session fatigue from document/history state.

## Deterministic fake carrier

No new live Yandex test should be the first validation of V6 reliability.

Implement an in-memory fake `WireSender`/carrier that can deterministically inject:

- DATA drop after apparent send success
- ACK drop
- reorder
- duplicate delivery
- delayed delivery
- carrier switch with outstanding replay
- late DATA from the draining generation
- receiver restart/resume
- asymmetric loss
- prolonged delivery-progress stall

Required tests:

1. Same logical seq is replayed through a newer carrier generation.
2. Retiring a carrier does not remove replay entries.
3. Late delivery from an old generation is deduplicated.
4. Lost ACK is recovered by repeated ACK state without an unbounded DATA storm.
5. Retry rate remains within the configured recovery budget.
6. New DATA admission slows/stops when replay debt crosses the recovery threshold.
7. ACK progress reopens the send window.
8. Receiver restart uses replay-floor/resume semantics without pinning base at zero.
9. Carrier switch itself does not reset logical sender or receiver session IDs.
10. Repeated carrier changes preserve one continuous logical stream.

## Telemetry required before live testing

Every V6 build used for Yandex testing must expose one compact line containing at least:

```text
logical-session
next-seq
ack-base
replay-depth
oldest-unacked-ms
new-data-rate
retry-rate
ack-rate
active-generation
active-doc
carrier-age-ms
physical-post-rate
http-p50/p95 or aggregate latency
ws-state
handoff-count
handoff-reason
```

A carrier handoff log must include old/new generation, reason, authorization-to-ws readiness time and outstanding replay depth at the moment of switch.

## Implementation sequence

### Phase 1 - simulator and logical reliability extraction

- Move sender reliability state out of `relayClient` into `ReliableSession`.
- Move receiver reliability state out of `wsListener` into `ReliableReceiver`.
- Define `WireSender` interface.
- Implement deterministic fake sender/receiver tests.
- No dual Yandex carrier logic yet.

Exit criterion: all deterministic loss/reorder/ACK-loss tests pass with bounded retries.

### Phase 2 - single physical VolgaCarrier

- Adapt current known-good V5.2 wire format to the new logical objects.
- One VolgaCarrier only.
- Confirm simulator and unit tests remain green.
- No live sustained benchmark yet.

Exit criterion: source architecture no longer keys replay or receive state by relay/ws object identity.

### Phase 3 - CarrierManager handoff

- Add fresh replacement carrier.
- Add readiness gating.
- Switch current active generation atomically.
- Keep previous generation receive-only during drain.
- Route new DATA, ACK and repair through current generation.

Exit criterion: fake carrier test can switch repeatedly with outstanding replay and no logical stream reset.

### Phase 4 - progress-based recycle and operation pacing

- Add ACK progress detector.
- Add recovery/send window limits.
- Add retry token budget.
- Add operation-rate telemetry.
- Add optional preventive max carrier age only behind a runtime flag.

Exit criterion: deterministic stall produces one controlled carrier recycle and bounded recovery traffic.

### Phase 5 - controlled live discriminator

Use a disposable benchmark environment only.

First compare existing vs fresh Yandex documents using identical parameters. Do not tune workers or retry timers during this test.

### Phase 6 - preliminary live acceptance

Same process and logical session, no restart between transfers:

- 6 x 50 MiB
- each transfer max 30 seconds
- every transfer must complete

If any transfer times out or collapses, the series fails and we inspect telemetry before changing parameters.

### Phase 7 - final acceptance

- 6 sequential full 100 MiB downloads
- same process/session
- no restart between tests
- each test max 45 seconds
- expected size 104857600 bytes each
- practical target >=20 Mbps sustained

Then run deterministic transport-loss validation using the test-only fake/loss path, not production nftables or router fault injection.

## Stop criteria

Volga should remain experimental if a clean V6 implementation with bounded operation rate and recyclable physical carriers still shows repeated accepted-but-undelivered logical operations severe enough to fail the sustained acceptance series.

At that point the evidence would support a carrier limitation rather than another local reliability implementation bug.

## Design decisions frozen for the first V6 implementation

- One logical reliability session across physical carrier generations.
- Physical Yandex authorization is disposable.
- Replay ownership is not tied to relay client lifetime.
- Receive dedupe/ACK state is not tied to websocket lifetime.
- Retries may change physical generation but never logical sequence.
- Draining carriers are receive-only.
- ACK state is repeatable/idempotent.
- Recovery traffic is rate-bounded.
- Live testing starts only after deterministic simulator coverage.
- No Production deployment during V6 development.
