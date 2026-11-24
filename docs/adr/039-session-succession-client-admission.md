# ADR 039: Admit session successors only from exact local proof

Status: Proposed

Amends [ADR 038](038-process-activation-and-exact-proof-succession.md) for the stage 1
session-ID shim and registering hooks. It keeps [ADR 035](035-post-window-shim-succession.md)'s
Choice A, the stage 1a hub transaction and the reverted ADR 037 schema refusal. Process-bound
succession, local start intents and synthetic mailbox cutover remain sequenced with
[ADR 036](036-single-session-clear-supersession.md). This proposal applies D3, D6, D8, D9,
D13 and section 5 of the [architecture](../architecture.md).

## Context

The hub can commit a child even when the shim receives no reply. A later bind can discover that
child, but discovery supplies neither the proof to adopt it nor a reason to give up on the saved
predecessor. If pruning removes that predecessor, the hub can still name the child. The client
needs the hub to distinguish this child from a healthy legacy binding whose local proof is also
absent. A hook has a related distinction to make after registration. A late end marker can veto use
of the reply, while a failed write of optional evidence can leave its current claim check valid.

## Decision

Treat binding answers as candidates until admitted. The hub identifies the kind and lineage of the
candidate. The client supplies exact proof from committed local evidence and checks the local end
floor under the sessions lock. An eligible shim with a retained predecessor can ask the hub to
create or replay even when bind found a live but unproved child. Hook reply admission and optional
proof persistence have separate outcomes.

### Local evidence

- The existing integer generation file remains readable by older binaries and supplies a D9 floor.
  An exact proof record at `<hash>.execution` contains a positive generation and its Execution ID
  together. It records whether a hook admitted the pair itself or a shim admitted it through
  succession. The latter also records the predecessor pair, so a fresh shim can apply the
  predecessor's end veto before heartbeating a recovering child that bind cannot see. Publish
  the record by one protected atomic replacement. A newer integer, on its own, supplies no exact
  proof. A bare legacy ID supplies none either. An exact record is committed as soon as its
  replacement succeeds. The integer is an independent compatibility mirror. An upgraded reader
  respects the greatest of its valid integer, exact and process-local generation floors. Only a
  real hook reply or an admitted succession reply can publish exact proof.
- Take a coherent snapshot of generation, exact proof and `.ended` under the bounded sessions
  lock before a succession request and again before accepting its reply. Use the same admission
  rule for a hook's registration reply. The marker read has three outcomes: absent, valid floor
  or damaged. Unreadable, malformed, dangling and non-regular markers are damaged. A damaged
  marker or failed mandatory read or lock vetoes admission and all publication and cleanup in
  that attempt. Use the greater of the applicable floor already observed in the attempt and the
  floor under lock. Expiry and cleanup follow D9 and cannot discard a floor already observed by
  the attempt. A shim checks both predecessor and child against the inclusive veto. A hook
  checks the Execution it would use under D9.
- Publish or accept an identical committed winner under the same lock as the final check. For a
  shim, compare the current exact pair with the submitted predecessor or the exact child in the
  reply. Missing exact proof, a different ID at the same generation or a floor above the child
  cannot satisfy this comparison. With the exact predecessor still present, an integer at the
  returned child generation is a floor the exact child must meet. Publish the exact child before
  updating the legacy integer. A failed exact replacement preserves the predecessor. A failed
  integer mirror cannot undo a committed exact record. A registering hook may update the legacy
  integer independently even if its exact write fails. It may never overwrite newer proof or a
  conflicting same-generation pair.
- Retain the submitted predecessor in the resolver across retries and refreshes. A fresh shim
  can use the exact record if it is still present. An in-memory pair alone cannot recreate a
  missing record. A newer committed same-session pair can resolve the retry through the rules
  below. Ordinary age-based pruning remains bounded and can remove evidence. If it wins before
  the compare-and-swap, recovery waits for a real hook unless an identical committed child is
  still present. Keep the lock within the existing local budget and release it before HTTP.

### Shim transitions

All paths that return, keep, use or renew a binding go through admission, including cached refresh,
the ordinary tick, Codex id-mode lookup, explicit handles and wake/watch lookups. A preliminary
candidate does not become `Bound`, feed a handover or heartbeat, or advance the resolver's remembered
proof. Only the existing positive native session-ID routes may attempt stage 1 succession: own
Rung 2 and a Codex id-mode root for its own session, with a live parent and any required process
tuple rechecked before dispatch and admission. Rung 3, Codex sub-agents and foreign-session
lookups, watch and wake helpers, active `IRIS_EXECUTION` overrides and explicit-handle calls have
no automatic hop. They cannot consume a hookless succession child through an ordinary binding
exception. A verified synthetic fallback and its handover keep their existing D6 identity checks.

| Discovery and local evidence | Transition |
|---|---|
| Live ordinary binding or a child reached by a real hook | Apply ordinary generation, identity, end and restore checks. A healthy legacy binding can have no exact record. A genuine registering hook can establish current evidence again under D9. |
| Live hookless succession child with an exact committed child pair | An eligible shim may admit that same pair after applying the current end floor to the child and the predecessor named by the hub. Recheck the evidence before renewal or use. |
| Live child immediately after the retained exact predecessor, with matching hub lineage and no committed child pair | An eligible shim asks `succeed` with the retained predecessor. Bind identifies this transition without furnishing either pair. A refusal is returned as `lost` when appropriate to the caller. |
| No live candidate for the positively identified session | Try D9 recovery first. With exact proof at the current floor, use the exact Execution and generation for heartbeat reattachment, including an already admitted hookless child. Check recorded lineage, eligibility and the end floor before sending its heartbeat. An integer-only reattach remains limited to the existing hook-registered D9 path and cannot supply succession proof. An authoritative loss of an eligible exact predecessor can lead to `succeed`. A recovering child whose exact pair was never committed locally needs a real hook. |
| Missing or damaged required evidence, a reached end floor, a forked or unrelated candidate, a lower restored history, a superseded process or a failed recovery request | Refuse use. An ambiguous transport outcome leaves a retryable attempt eligible while its exact predecessor and fences still permit it. A local proof refusal does not mark the candidate irreversibly gone. A genuine hub loss of an Execution still bars reuse of that Execution itself. |

Use a distinct internal outcome for a candidate needing exact replay. The plain shim's call and tick
and the Codex root's own call take the same transition regardless of the public binding error.
Retain the pair after an ambiguous response. Each bounded attempt, including a transport retry,
rechecks the same identity, parent, local proof and fences before sending it again. If another
writer published the exact child, accept the identical winner through reply admission or ordinary
exact verification. An unrelated `lost`, authentication or transport error does not select another
predecessor. A `succeed` answer must name the same session and the immediate child of the submitted
pair, with consistent provenance, and pass the final locked check before it is usable. A live
hookless child's ordinary bind answer must match the recorded local lineage as well as the child
pair. Accept a hook-created winner only when the same hub transaction identifies the lineage or
ordinary admission can independently establish a current hook-registered binding. A rejected
recovery has no generation-zero retry or synthetic substitute. Fresh no-hook bootstrap continues
under D6 and Choice A.

### Hook outcomes under D3

The hook makes the mandatory admission decision before using an HTTP registration reply, including
ordinary, outside-git, full and reduced combined shell registration. A lower-generation reply or
an equal generation with a different committed ID fails admission. Repeat the same locked check
before a separate reconcile or Touch if work intervenes after admission. Apply the same marker
veto to any write or cleanup that could make the reply usable.

| Result under the sessions lock | Result for the hook |
|---|---|
| Confirmed applicable end floor reaches the returned generation | Publish nothing. Preserve D9's deliberate pre-tool ended refusal and post-shell warning. |
| Marker damaged, reply violates a generation or identity floor, or required check unavailable within budget | Publish nothing and send no dependent Touch, TouchFiles or split reconcile. Return valid D3 allow output. Record damage with the local path for doctor, and use D3 failure recording when an unavailable check skips ordinary protection. |
| Reply admitted, optional evidence write fails | Record a proof-write observation. A failed exact replacement supplies no exact proof for the reply. A failed mirror leaves any exact commit intact. Use the admitted reply for the current hook operation within its remaining budget. A genuine Touch conflict or authoritative retry refusal still follows D3, including a deliberate deny. |
| Reply admitted and evidence committed | Use the admitted reply and the newly committed proof within their respective checks. |

An optional write failure alone cannot change the adapter's decision. This narrows ADR 038's
unqualified ALLOW wording to a failure that prevents the hook from running its ordinary check.
SessionEnd keeps its exact end semantics. A combined reconcile has already sent its snapshot when
the hook checks the reply. A local veto can suppress proof and use of that reply. It cannot undo
work that committed on the hub.

### Trade-offs

| Discovery and replay option | Benefit | Cost or limit |
|---|---|---|
| Infer succession from a local sidecar | No bind change | Pruning and a healthy legacy binding both look like absence. |
| Replay every outward `lost` | Simple routing | Conflates an unproved child with a damaged marker, a restore refusal or a different identity. |
| **Use durable hub provenance and an explicit exact replay transition** | Discovery classifies even after pruning, and the hub still decides the winner | Requires a small additive binding contract and one admission rule across call paths. |
| Write an append-only intent before dispatch and retain it until settlement | Could extend replay through process exit and ordinary pruning | Requires bounded retention, incomplete-intent handling and end ordering. It cannot survive all local loss or authorize adoption by itself. Stage 1 relies on retained exact proof and can wait for a hook. |

| Publication option | Benefit | Cost or limit |
|---|---|---|
| Use the integer as commit for a staged sidecar | Keeps one visible generation for readers | An old integer-only writer can commit a staged pair after an interrupted update. |
| **Atomically commit the exact pair and keep the legacy integer as a floor** | Old writers cannot manufacture proof. Failed exact replacement leaves the old pair intact. | The files can differ. Readers must honor both floors and distinguish exact evidence from a counter. |
| Fail open on every optional write failure | Simple hook outcome | Skips an otherwise usable claim check. |
| **Veto on a failed mandatory check and observe optional write failures** | Preserves D3 and retains authoritative claim handling where the reply is usable | Separates two outcomes and adds one bounded local admission check. |

### Wire, schema and mixed versions

Stage 1a owns migration 029 (`prior_execution_id`, `creation_source`, `creation_proof`), the
existing create-or-replay transaction and the refusal of abandoned ADR 037 schemas. Stage 1b
requires no further migration or provenance backfill. Keep the integer file format. The new
exact record has one atomic commit point and is written only by binaries that implement it.

A stage 1b hub extends successful bind replies for session, process, combined and explicit
lookups with `prior_execution_id`, `creation_source`, `creation_proof` and the current
`hook_seen` state. A capable reply includes the provenance fields explicitly, with JSON null
for absent values on legacy rows, and names `succession_proof` in `capabilities`.
`creation_source:"shim"` with `creation_proof:"shim_succession"` identifies a shim-created child.
Its predecessor generation is the child's generation minus one. The existing recorded lineage
distinguishes a same-predecessor hook winner for replay. Include the same classification on
succession, non-hook reattachment and heartbeat replies that can replace a cached binding.
A capable reply with missing, unknown or inconsistent provenance is unusable. Decode
classification as fresh data. A lease-only legacy reply never erases a classification already
known to the resolver.

Stage 1b clients use `POST /v1/sessions/succeed/exact`, a distinct v1 entry point to the same stage 1a
transaction, with the same exact predecessor, source, process and `ended_through` request fields.
It provides the classified reply even when no earlier bind succeeded. An older or stage 1a-only
hub returns `not_found` before a mutation. Clients never fall back to the stage 1a entry point or
ordinary registration. This avoids treating successful negotiation on an earlier bind as a
guarantee about the hub that handles the mutating request. On the stage 1b hub the original entry
point returns `not_found` before mutation. Stage 1a has no shipped callers of that entry point.

A stage 1b resolver opts in with `succession_proof:true` on bind and non-hook heartbeat. The
capable hub returns `lost` for a matched hookless succession child on these paths without that
opt-in, including handle bind and heartbeat by ID, before it can return or renew the child.
Both registration routes
refuse a non-hook request that would attach to a hookless succession child. They also refuse a
non-hook generation-zero replacement while the newest retained row is such a child, including
after it is lost and the predecessor has been pruned. These guards protect shipped older clients
that ignore the response fields. Opt-in identifies the protocol. The client still has to
establish local proof. A genuine hook can attach through normal registration and the older-client
bind and heartbeat paths become available after the hub records `hook_seen`.
Ordinary legacy rows, hooks, healthy fallback bindings and D9 in-window recovery retain their
wire contracts. An already known hookless successor cannot be reclassified as legacy by an
unclassified answer.

| Hub | v0.1.15 or any client without this protocol | v0.1.16 client with stage 1b |
|---|---|---|
| v0.1.15 hub | Existing behavior. | Ordinary binding and D9 recovery on its original schema. The new route refuses cleanly. No automatic post-window succession. |
| v0.1.16 hub with stage 1a only | Ordinary paths with Choice A over retained history. No shipped client invokes succession. | Same ordinary paths. The new route is absent, so the client cannot create a child. |
| v0.1.16 hub with stage 1b | Ordinary paths with Choice A. Hookless succession children stay unavailable to old shims and watchers until a hook registers them. | Exact create/replay and admitted renewal, with the same ordinary paths. |

These are protocol profiles. A release version alone never enables succession. A downgrade to
a hub without the stage 1b guard cannot safely serve a database with live or recovering hookless
succession children. A pre-029 hub cannot interpret or preserve the new lineage. Use a
provenance-aware rollback build or restore the pre-migration backup under
[ADR 006](006-retention-and-restore.md), accepting the loss of changes after the backup.
Compatibility of v1 requests gives no guarantee about opening a later database in place.

## Invariants

1. A hookless succession child can be used or renewed only after exact admission. Local absence,
   a numeric floor, a bind reply and a failed CAS cannot confer it. Only a registering hook can
   establish hook reachability.
2. While the original exact pair remains usable, discovering an unproved immediate child with
   matching lineage leads to the exact hub operation. A lost reply does not move the local commit
   point or consume the retry. A rejected child is never a new predecessor by virtue of discovery.
3. Local end-marker validation and proof publication share one lock and one order with Iris
   SessionEnd and pruning. Damage is a veto even if an earlier check saw absence. A floor that
   reaches the submitted predecessor or child fences shim replay and adoption.
4. Creation and replay keep the stage 1a transaction invariants. At most one child is created.
   It shares the permitted mailbox and repository, inherits no claims and cannot begin another
   automatic hop until a hook registers it. Replay creates no second lease renewal, grant, event
   or generation change. The client never renews an unadmitted binding or one confirmed lost.
5. Hooks stay within D3's deadline and return their client adapter's valid output. A local
   persistence error alone cannot deny an edit. A confirmed end or an authoritative claim
   decision retains its existing semantics. No session-file lock spans HTTP.

## Limits

A lost reply is recoverable only while usable exact evidence and the hub's eligible live child
survive. Local file deletion, expiry, a held Windows handle or an unavailable lock can leave a
committed child waiting for a hook. A machine crash can also lose local writes. Neither the
counter nor the hub response substitutes for lost local proof. A backup with identical retained
lineage can be indistinguishable from the original hub. History retention can remove the hub's
evidence for Choice A.

The local check orders observed local state. An end or process change after admission can race
an already dispatched request. Hub order still governs what commits, and the local protocol
cannot roll back a mutation or detect an end that neither side records. D13 trusts the host
token holder for its own host and the OS user for its local files. Provenance and opt-in cannot
authenticate a native event against someone who can forge that event or rewrite the proof.

## Deterministic verification

Use fake hub time, request barriers and injected read, write and lock outcomes. Run the decisions
through the resolver, real SQLite store and HTTP handlers. Assert request pairs, cache and local
proof contents, claims, lease renewal time, events and generation changes as applicable.

| Case | Interleaving and required result |
|---|---|
| Lost replies after commit | Drop both replies after creating G+1. Present G+1 through ordinary bind while exact G remains. The next eligible call or tick uses `succeed` with G and its original ID, commits the returned pair and then admits the child. Run for a surviving shim and a fresh shim, with plain and Codex root call paths. Exactly one child and one creation event. Replay itself leaves the lease unchanged. |
| Competing writers | Pause two shims after preparing G. Release a shim or eligible hook winner first. Move publication before the loser's lookup, during reattachment and during its request. The loser accepts only the same eligible winner. Exercise the identical installed pair and equal generation with a different ID. |
| Prune between prepare and CAS | Remove the exact record under the lock after prepare, with and without the integer. Commit at the hub and resume the CAS. No replacement proof or heartbeat. Repeat with only the integer pruned and verify the retained exact pair can still advance. Restart with neither file and look up the live child by session, process and handle. Refuse it. Under the same local absence a healthy legacy binding works. A genuine registering hook restores the ordinary path. |
| Publication interrupted | Fault the exact replacement and then, separately, the legacy mirror. Restart on either cut. A generation-only old writer cannot certify the child. An exact commit survives a failed mirror. A committed greater generation or a same-generation ID mismatch rejects a delayed reply. Repeat with the destination file held open on native Windows. |
| Marker changes after HTTP | Barrier after the early reply check and before locked admission. Insert a valid floor at the predecessor or returned generation, or replace the marker with malformed data, a dangling link or a non-regular/unreadable entry. Shim neither adopts nor publishes. Damaged marker gives the hook valid ALLOW, the marker observation and no dependent request. Confirmed end gives D9's refusal or warning. A strictly lower floor permits publication. Cover both orders of proper SessionEnd and publication. |
| Optional hook persistence fails | Fail proof replacement after successful locked admission. Check normal allow and an actual conflict on both Touch paths, plus valid 409 retry refusals and malformed or transport failures. Check D3 adapter encoding and the proof-write observation. Force a mandatory lock timeout and a conflicting same-generation reply separately. Each gives bounded allow with no dependent request. Run reply admission for SessionStart, ordinary and outside registrations and full/reduced shell routes, allowing for already committed combined reconcile. |
| Eligibility and boot recovery | Run ended, expired, recovering, superseded, restored-lower and same-generation fork cases. With exact current proof, D9 can heartbeat-reattach the same recoverable child. Install an end floor at its predecessor and restart hub and shim. No heartbeat is sent. Without exact current proof, replay cannot renew a recovering child. Prove a hookless child cannot create a further hop. Exercise dead parent, PID reuse, cross-load, watcher, wake, active override, handles, Rung 3 and Codex foreign and sub-agent paths. |
| Versions and fallback | Run all six hub/client pairs above, plus a missing or inconsistent provenance reply from a capable hub. Assert zero mutation against stage 1a with the stage 1b route and against stage 1b with the stage 1a route. Exercise old bind and heartbeat on a live and recovering hookless child. Prune the hub predecessor and prove that non-hook registration cannot renew the child or replace it after loss. Exercise ordinary reattachment, D6 fallback and Choice A. Test rollback from a pre-029 backup and refusal of reverted ADR 037 schemas. |

## Effort and sequencing

1. Land stage 1a as reviewed, hub only. No shipped client invokes succession.
2. Land the additive classified replies, compatibility guard and exact wire entry point first
   in stage 1b. Verify mixed-version requests against real handlers and migration 029. Estimate
   1 engineering day.
3. Land shared resolver admission and replay with locked hook admission and single-commit exact
   proof. Enable the native session-ID path once the hub and client matrix passes. Estimate
   2–3 engineering days plus 1–2 days for the deterministic matrix, native Windows, fixture
   replay and the repository checks. Total stage 1b estimate is 4–6 engineering days.
4. Sequence process-bound activation and durable fallback cutover with ADR 036 as specified in
   ADR 038. Stage 1b supplies no process activation witness.

Before enabling the implementation, run the existing `make test`, `make race`, `make cov`,
`make cov-check`, `make check-all` and `make fixtures` checks. Use the native Windows session-file
tests for sharing and replacement behavior.
