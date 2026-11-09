# 016: Wake streams on committed changes

Status: proposed; implementation and measurements are under review.

## Problem

An idle Execution stream polls every 100 ms, validating its lease and then reading
events in separate transactions. Console streams also poll. Every domain call
checks expiry, and event consumers can rewrite an unchanged cursor. The resulting
work scales with the number of idle sockets and competes with hooks for the
store's transaction mutex.

## Decision

1. A hub shares maintenance and committed-change notifications across its live
   streams. Its worker starts with the first stream and stops after the last one.
   Idle sockets do not poll the store. A burst is coalesced within a bounded
   interval measured from its first change, so sustained traffic cannot keep
   postponing delivery.
2. Time-driven transitions use one maintenance sweep, at most once a second.
   A no-op sweep does not wake observers. Execution and claim expiry, missed
   receipts and absolute console-session expiry still produce bounded attention
   or revocation when no client writes.
3. Each observation reads state in one transaction. Execution event reads check
   the requested generation in that same snapshot. Transport pings do not trigger
   store reads. A full event page drains immediately until a short page is found.
4. Subscribe before reading state. Commits precede notifications, and a change
   between an observation and its wait cannot disappear. Notifications are hints;
   durable event cursors, replay and reset snapshots remain authoritative.
5. Ticket rotation has an explicit in-memory revocation signal. It does not rely
   on a database poll. Console frame admission remains serialized with account
   revocation. An independent observer can cancel a stalled network writer; an
   admitted frame is queued only after its transaction has committed.
6. Expiry queries use live-row indexes and newly ended or lost executions rather
   than repeatedly scanning historical ownership. Requeue remains in the same
   transaction as the transition. An unchanged persisted cursor is not rewritten.

## Limits

The hub can still perform one maintenance transaction per second while streams
exist; zero per-socket polling does not mean zero hub transactions. Time-driven
revocation can be delayed by that sweep interval and bounded coalescing. A frame
admitted before revocation can already be in transit. Frames remain content-free.
Scheduling and store contention can add latency; this is not a hard real-time
guarantee. The store retains its single transaction mutex and existing authorization
rules. A real loss can still visit acknowledged deliveries assigned to that
Execution, and a claim release retains its existing durable-Agent fanout. The
change removes repeated historical scans during steady idle operation, rather
than promising every non-idle transition is proportional only to unacked mail.

## Validation

Deterministic tests count real transaction deltas with in-memory socket transports
and virtual time. They cover idle streams, coalesced bursts, backlog draining,
rotation, expiry, missed receipts, reset/reconnect, and stalled-write revocation.
Existing section 5 invariants and fault-injection sweeps remain in force.

Synthetic fixtures matching the reported production scale and ten times its
history measure CPU time, transaction rate and registration RPC latency for 1, 5 and 20 watch
sockets plus two console sockets. The before and after environments, measured
durations and unmeasured cases are recorded in `docs/research/hub-idle-cpu.md`.
No production account data is used for those measurements.
