# Volga V6 protocol invariants

These invariants are stricter than the V5/V5.2 implementation and are intended to prevent another architecture regression while physical Yandex carrier recycling is added.

## Logical lifetime is independent of physical lifetime

A physical Yandex authorization/request-path/websocket generation may be replaced without changing:

- OpenFlux logical session ID
- next logical DATA sequence
- replay ownership
- cumulative ACK state
- receiver dedupe state
- congestion/recovery state

A replay of logical `seq=N` through a new physical generation keeps the same logical session and sequence, but the concrete Yandex adapter must generate fresh Yandex operation/local/bundle identities.

## Only cumulative ACK frees replay storage

This is an intentional change from V5/V5.2.

A SACK range proves that the current receiver instance has observed a sequence above the cumulative base. It does **not** prove that the sender may permanently forget the payload.

Example:

```text
receiver: base=1, SACK={3}
sender replay before ACK: 1,2,3
```

After this ACK the V6 sender may free only sequence 1. Sequence 3 stays in replay but is marked SACKed and suppressed from ordinary repair.

Reason: if the receiver restarts before sequence 2 arrives, its in-memory SACK knowledge for sequence 3 disappears. If the sender had already deleted sequence 3, replay-floor resume could establish `base=1`, repair sequence 2, and then become permanently pinned at `base=2` because sequence 3 no longer exists anywhere recoverable.

Therefore:

```text
cumulative ACK <= base  -> free payload
SACK > base             -> retain payload, suppress repair
later ACK loses SACK    -> retained payload becomes repairable again
```

This is covered by `TestVolgaV6SACKDoesNotDestroyRestartRecovery`.

## HTTP success is never a logical ACK

HTTP 200/204 from the Yandex relay means only that the relay request was accepted. Replay is released only by a peer logical ACK received through the V6 protocol.

Physical POST success must not update cumulative logical ACK progress.

## ACK is repeatable state

The latest cumulative ACK/SACK snapshot is idempotent state and may be sent repeatedly. A receiver does not need another duplicate DATA frame before it can repeat a previously lost ACK.

This prevents a lost small control operation from forcing an unnecessary DATA retransmission storm.

## Draining carriers are receive-only

After make-before-break handoff:

- new DATA uses the new Active carrier
- repairs use the new Active carrier
- ACK control frames use the new Active carrier
- the previous carrier remains only to receive late websocket delivery
- retirement of the previous physical carrier never deletes logical replay

## Handoff must be serialized

Only one physical replacement may progress through authorize/readiness/active-pointer switch at a time. Concurrent handoffs must not race.

Physical generation numbers are monotonic and are not reused after a failed authorization/start attempt.

## Recovery pressure is bounded

A logical hole does not grant permission for unlimited repair traffic.

A repair is emitted only when both conditions are true:

1. the logical retry timer says the sequence is due, and
2. the transport-wide retry budget has a token.

New DATA admission is reduced as replay debt grows, before the static replay capacity is reached.

## Carrier recycle is based on end-to-end progress

The primary degradation signal is stalled cumulative logical ACK progress while DATA is outstanding and oldest-unacknowledged age is growing.

HTTP 204 rate alone is not a health signal. A fixed carrier age may be added later as a preventive policy only if controlled tests prove a repeatable age-dependent degradation.

## No live Yandex validation before simulator correctness

The offline simulator must cover, at minimum:

- apparent send success with DATA drop
- ACK drop and repeated ACK recovery
- reorder
- duplicate delivery
- receiver restart/replay-floor recovery
- SACK retention across receiver restart
- physical handoff with outstanding replay
- replay of the same logical sequence on a new generation
- late duplicate from a draining generation
- failed replacement keeping the old carrier active
- bounded repair burst/rate
- new DATA backpressure under replay debt
- repeated physical generations preserving one logical session

Only after these invariants are green should the concrete Yandex Volga adapter be connected to V6.
