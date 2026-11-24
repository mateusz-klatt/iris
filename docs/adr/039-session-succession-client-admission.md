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
  floor under lock. Cleanup cannot discard a floor already used by the attempt. A shim checks
  both predecessor and child against the inclusive veto. A hook
  checks the Execution it would use under D9.
- For a new shim compare-and-swap, the current exact pair must still be the submitted predecessor
  at G. The legacy integer and process-local floor must be at most G, including any higher floor
  already observed during this attempt. A missing or lagging mirror is allowed. An integer at
  G+1 could describe a different Execution, so it vetoes creating exact proof from G, even if
  the reply also names G+1. Apply this predecessor check at prepare, retry and publication. If
  the exact child was independently committed in the meantime, admit it only after checking the
  same pair, lineage and fences, with no floor above the child. This path leaves the committed
  record in place. Missing exact proof and a conflicting ID never authorize the swap.
- Publish the exact child before updating the legacy integer. A failed exact replacement preserves
  the predecessor. A failed integer mirror leaves the exact commit intact. A registering hook
  may update the legacy integer independently if its exact write fails. It may never overwrite
  newer proof or a conflicting same-generation pair.
- Retain the submitted predecessor in the resolver across retries and refreshes. A fresh shim
  can use the exact record if it is still present. An in-memory pair alone cannot recreate a
  missing record. A newer committed same-session pair can resolve the retry through the rules
  below. Ordinary age-based pruning remains bounded and can remove evidence. If it wins before
  the compare-and-swap, recovery waits for a real hook unless an identical committed child is
  still present.

### Sessions lock budget

Use one absolute monotonic deadline for each sessions-lock operation, set before lock acquisition
to the earlier of the caller's deadline and 50 ms from entry. Acquisition, locked reads, exact
replacement, integer mirroring, marker writes and cleanup in that hold all draw from the same
budget. Prepare protected temporary files outside the lock and discard unused temporary files
after release. Release the lock before HTTP or diagnostic logging.

On Windows, check the remaining budget before each filesystem attempt, including the
`MoveFileEx` fallback when the POSIX form is unsupported. Optional replacements make no
sharing-violation retry sleeps while holding the shared lock. A required marker write can retry
only within the same deadline and with pauses capped by time remaining. Once the budget is spent,
release the lock and leave unfinished optional work for a future operation. A deadline can bound
Iris retries and waits. It cannot interrupt an already started filesystem call or guarantee
acquisition under contention.

### Exact record lifecycle

- **SessionEnd and Forget.** After the hub confirms the exact end, commit a monotonic `.ended`
  floor first under the sessions lock, even if pruning removed the live files. If the floor could
  not be read or committed, preserve both proof files and record the marker path for doctor.
  Once the marker is committed, check the integer and exact record separately and remove each
  vetoed entry. The floor vetoes an exact record at or below it and a shim-origin record whose
  predecessor is at or below it. Preserve newer, unvetoed evidence. Failed removal leaves the
  marker effective and records the failed cleanup.
- **Expiry and pruning.** The 30-day retention age makes a readable marker eligible for cleanup. The
  marker stays a veto until cleanup completes. Under the same lock, re-read the marker and its
  matching files, then delete or confirm absence of every vetoed entry before removing the
  marker last. Inspect both proof files regardless of mtime or whether one is already absent.
  If a required read, removal or budget check fails, keep the marker. Apply this order both to
  the idle sweep and to a hook cleaning an expired marker before registration or publication.
  A damaged marker is never eligible. An attempt that already used the floor keeps it for the
  rest of that attempt, even if another process completes the cleanup.

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
| No live candidate for the positively identified session | Try D9 recovery first. The own-session Rung 2 path can reattach by exact heartbeat when proof is at the current floor. Check recorded lineage, eligibility and the end floor before sending it, including for an already admitted hookless child. An authoritative loss of an eligible exact predecessor can lead to `succeed`. A recovering hookless child whose exact pair was never committed locally needs a real hook. |
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

Rung 3 memory reattachment sends a positive-generation registration with `source:"shim"` and
the verified `client_pid` and `client_pid_start_ms`. Use the same pinned request when a
process-bound refresh finds it needs recovery. This applies even when exact memory is available.
The hub checks the tuple and hook registration in the same transaction before renewal or resume.
Missing or changed process identity vetoes that path. Exact local evidence adds a reply identity
check. It cannot replace the server's process pin with a heartbeat by ID. An integer-only
reattach remains on the existing hook-registered D9 path and supplies no exact succession proof.

An older hub's heartbeat may omit `hook_seen`. When the resolver reattaches hook-backed memory
by exact heartbeat and that fact is missing, it follows up with `/v1/bind` by ID. It requires the
same session, Execution and generation and affirmative `hook_seen` before admitting the result.
A lease-only reply for the same already-admitted pair keeps the established hook state. Missing
hook state cannot promote a hookless child or grant wake eligibility.

### Wake and MCP renewal

Codex wake uses `Primary()` to select a target and `StartIfPrimary()` to check it again at native
start. Both require normal local admission and affirmative hub `hook_seen` for the same exact
pair. An admitted hookless child can handle MCP work but produces no wake target, worker, stream,
or wake `HookSeen` probe. A resolver refresh can expose the same pair to wake after a real hook
registers it. The wake worker's exact hub `HookSeen` check remains in place.

Primary `whoami` renews its resolved pair through the same resolver path as scheduled and refresh
heartbeats. The resolver checks local evidence and any required process identity before dispatch,
sends the applicable source and opt-in, then validates the returned pair, classification and
current local fences before returning or caching it. It discards a reply if the binding changed
in flight. `whoami` for a lookup-only binding performs no renewal. The MCP handler sends no
separate heartbeat.

### Hook outcomes under D3

Before each registration dispatch, including SessionStart, outside-git, a D9 retry, and full or
reduced combined shell registration, take the locked snapshot after checkout discovery or shell
status work. Compute the generation from the greatest usable remembered floor, including a
committed exact record when the mirror is absent or behind. Send the effective `ended_through`.
Retain D9's rules for a genuine first registration, an authorized SessionStart after end, and a
single stale-proof retry. That retry requires a valid `highest_generation` from the initial
refusal at or above the retained floor and carries that floor in `retry_generation`. Mirror
failure alone never produces a generation-zero request.
A damaged `.execution`, integer or marker stops dispatch, including the combined shell mutation,
with a local path observation and valid D3 allow output. A missing exact record can still leave
usable legacy generation proof.

Admit the registration reply under the sessions lock before using it. A lower-generation reply
or an equal generation with a different committed ID fails admission. Where work intervenes,
repeat the locked check immediately before the dependent request, including after `fileGroups`
before TouchFiles and after split full or reduced git status before reconcile. Retain the floor
already used in the attempt across those checks. Apply the same veto to every publication and
cleanup that could make the reply usable.

| Result under the sessions lock | Result for the hook |
|---|---|
| Confirmed applicable end floor reaches the returned generation | Publish nothing. Preserve D9's deliberate pre-tool ended refusal and post-shell warning. |
| Damaged evidence or a reply that violates a generation or identity floor | Publish nothing and send no dependent Touch, TouchFiles or split reconcile. Return valid D3 allow output and record the proof refusal or local damage path for doctor. |
| Required admission check misses its lock or time budget | Publish nothing and send no dependent request. Return valid D3 allow output. If it skipped edit or shell protection, record a D3 failure in `session` or `shell session`. Doctor grades that fail-open `silent`, with the warning that edits may have gone unclaimed. |
| Reply admitted, optional evidence write fails or runs out of local time | Record a proof-write observation. A failed exact replacement supplies no exact proof for the reply. A failed mirror leaves any exact commit intact. Use the admitted reply for the current hook operation within its remaining budget. A genuine Touch conflict can still deny the edit, and hub errors follow the client's D3 policy. The optional write observation alone does not change doctor to `silent`. |
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

A stage 1b shim opts in with `succession_proof:true` on bind and its own heartbeat. A capable hub
returns `lost` for a matching hookless succession child on bind without opt-in, including a
handle bind. On heartbeat it requires both `source:"shim"` and `succession_proof:true` and checks
them before either renewing a live child or resuming a recovering one. A hook or watch heartbeat
cannot satisfy this check. Both registration routes refuse a non-hook request that would attach
to a hookless succession child. They also refuse a non-hook generation-zero replacement while
the newest retained row is such a child, including
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

These are wire profiles. Enable writing and using exact records only after every process that
writes or prunes the shared sessions directory uses the stage 1b lifecycle. Older writers can
retire `.ended` independently of `.execution`. Before cutover, use ordinary D9 generation files.
Retain older clients on installations with separate local state. Restart old shims and replace
old hook or watcher entry points before enabling stage 1b on shared state. A local rollback needs
pre-activation state or a separate sessions directory.

A release version alone never enables succession. A downgrade to a hub without the stage 1b
guard cannot safely serve a database with live or recovering hookless succession children.
A pre-029 hub cannot interpret or preserve the new lineage. Use a
provenance-aware rollback build or restore the pre-migration backup under
[ADR 006](006-retention-and-restore.md), accepting the loss of changes after the backup.
Compatibility of v1 requests gives no guarantee about opening a later database in place.

## Invariants

1. A hookless succession child can be used or renewed only after exact admission. Local absence,
   a numeric floor, a bind reply and a failed CAS cannot confer it. Only a registering hook can
   establish hook reachability.
2. While the original exact pair remains usable, discovering an unproved immediate child with
   matching lineage leads to the exact hub operation. A lost reply does not move the local commit
   point or consume the retry. A counter ahead of the predecessor prevents new exact publication.
3. Local end-marker validation and proof publication share one lock and one order with Iris
   SessionEnd and pruning. Damage is a veto even if an earlier check saw absence. A floor that
   reaches the submitted predecessor or child fences shim replay and adoption. An expired marker
   remains effective through removal of all proof it vetoes.
4. Creation and replay keep the stage 1a transaction invariants. At most one child is created.
   It shares the permitted mailbox and repository, inherits no claims and cannot begin another
   automatic hop until a hook registers it. Replay creates no second lease renewal, grant, event
   or generation change. The client never renews an unadmitted binding or one confirmed lost.
5. Rung 3 memory reattachment retains the server process pin. Resolver renewal validates the exact
   reply before returning it to primary `whoami`. Codex wake requires positive hook evidence for
   that pair even when MCP use is already admitted.
6. Hooks stay within D3's deadline and return their client adapter's valid output. A local
   persistence error alone cannot deny an edit. A confirmed end or an authoritative claim
   decision retains its existing semantics. All locked operations share one deadline per hold,
   and no session-file lock spans HTTP.

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
| Counter ahead of exact predecessor | Leave exact `(G, P)` and raise only the integer to G+1, first with a legacy writer and then with a hook whose exact replacement fails. Present a delayed same-lineage G+1 reply for a different child after a fork or restore. Test the counter advance before prepare, before retry and between the request and CAS. No new exact record or shim heartbeat. Prune the higher counter after the attempt has seen it and check that the veto persists. Independently commit the identical child and check that it can pass ordinary admission without a rewrite. |
| Prune between prepare and CAS | Remove the exact record under the lock after prepare, with and without the integer. Commit at the hub and resume the CAS. No replacement proof or heartbeat. Repeat with only the integer pruned and verify the retained exact pair can still advance. Restart with neither file and look up the live child by session, process and handle. Refuse it. Under the same local absence a healthy legacy binding works. A genuine registering hook restores the ordinary path. |
| Publication interrupted | Fault the exact replacement and then, separately, the legacy mirror. Restart on either cut. A generation-only old writer cannot certify the child. An exact commit survives a failed mirror. Run the next hook with an absent and a lagging mirror. It sends the committed positive generation in ordinary and combined registration, with the effective end floor. A committed greater generation or a same-generation ID mismatch rejects a delayed reply. |
| Shared Windows deadline | Use two session IDs on the same sessions lock and a fake monotonic clock. Spend most of A's time on acquisition and the first replacement. Inject a sharing violation on the second and independently make the POSIX form report unsupported at the cutoff. Assert no optional retry sleep, no fallback after cutoff and no fresh budget. B can take the released lock and complete its protection. Run the sharing case with the actual destination held without delete sharing on native Windows. |
| Marker changes after HTTP | Barrier after the early reply check and before locked admission. Insert a valid floor at the predecessor or returned generation, or replace the marker with malformed data, a dangling link or a non-regular/unreadable entry. Shim neither adopts nor publishes. Damaged marker gives the hook valid ALLOW, the marker observation and no dependent request. Confirmed end gives D9's refusal or warning. A strictly lower floor permits publication. Cover both orders of proper SessionEnd and publication. |
| Exact record and expired marker | Confirm SessionEnd with the integer absent or newer than the ended exact record. Test the inverse generations and a shim-origin child vetoed through its predecessor. Assert marker publication comes first. At the retention boundary run both sweep and on-demand cleanup, with distinct mtimes and each proof file absent in turn. Inject marker write failure, proof read or delete failure, and interruption between each removal. Assert that every surviving vetoed record remains fenced and that unvetoed newer evidence is preserved. |
| Damaged exact record before dispatch | Damage `.execution` while discovery or status is paused. Run SessionStart, ordinary and outside registration, a D9 retry after its initial refusal, and full and reduced combined shell routes. Require a local path observation, valid bounded ALLOW and zero registration or reconcile requests after the barrier. Repeat with a missing exact record and healthy integer to verify legacy recovery. |
| Intervening checkout or shell work | Pause in `fileGroups` after registration and in split full and reduced git status before reconcile. Advance or damage `.ended`, or damage `.execution`, during the pause. Resume after the old admission has passed. Require a fresh locked check and zero dependent requests. Confirmed end gives the applicable D9 refusal or warning. Damage gives D3 ALLOW and an observation. |
| Optional hook persistence fails | Fail proof replacement after successful locked admission. Check normal allow and an actual conflict on both Touch paths, plus authoritative 409 retry refusals where supported and malformed or transport failures. Check D3 adapter encoding and the proof-write observation, with no `silent` grade from that observation alone. Force a mandatory lock miss during edit or shell admission separately. Require bounded allow, no dependent request, a D3 failure and `silent`. Also reject a conflicting same-generation reply. A combined reconcile may already have committed when its reply is vetoed. |
| Eligibility and boot recovery | Run ended, expired, recovering, superseded, restored-lower and same-generation fork cases. An eligible Rung 2 shim with exact current proof can reattach the same recoverable child. Install an end floor at its predecessor and restart hub and shim. No heartbeat is sent. Without exact current proof, replay cannot renew a recovering child. Prove a hookless child cannot create a further hop. Exercise dead parent, cross-load, watcher, wake, active override, handles and Codex foreign and sub-agent paths. |
| Rung 3 pin and legacy hook state | Try exact and integer-only Rung 3 recovery, including a refresh that finds the row recovering. Vary matching process identity, omitted tuple, another PID and same PID with a different start time. Assert no ID-only reattachment and no renewal or recovery mutation from a failed pin. Against v0.1.15 omit `hook_seen` on Rung 2 exact heartbeat recovery. Test exact follow-up bind with `hook_seen:true`, with missing or false hook state, and with a different pair. Only the affirmative matching bind admits a hook-backed result. |
| Codex wake and primary `whoami` | Feed an admitted hookless child to the Codex root resolver. `Primary()` and `StartIfPrimary()` refuse it, with no wake worker or `HookSeen` probe. Register a real hook and refresh the same child, then verify wake eligibility. On the live admitted pair, an MCP `whoami` call sends a single resolver heartbeat with shim source and opt-in. Inject wrong pair, invalid provenance, late local end and a concurrent binding replacement. No stale reply reaches MCP or cache. Lookup-only `whoami` sends no heartbeat. |
| Versions and fallback | Run all six hub/client pairs above and exercise missing or inconsistent provenance on the capable hub. Assert zero mutation against stage 1a with the stage 1b route and against stage 1b with the stage 1a route. On live and recovering hookless children, test legacy bind and every heartbeat source with and without opt-in. Only opted-in `shim` renews. Prune the hub predecessor and check non-hook registration cannot renew or replace the child after loss. Check ordinary recovery, D6 fallback, Choice A, the shared-directory upgrade prerequisite, pre-029 backup rollback and refusal of reverted ADR 037 schemas. |

## Effort and sequencing

1. Land stage 1a as reviewed, hub only. No shipped client invokes succession.
2. Land the additive classified replies, compatibility guard and exact wire entry point first
   in stage 1b. Verify mixed-version requests against real handlers and migration 029. Estimate
   1–2 engineering days.
3. Land shared resolver admission and replay, wake and MCP renewal checks, and the locked exact
   proof lifecycle. Enable session-ID succession after the matrix passes and local session-file
   writers have been upgraded. Estimate 2–3 engineering days plus 2–3 days for the deterministic
   matrix, native Windows, fixture replay and repository checks. Total stage 1b estimate is
   5–8 engineering days.
4. Sequence process-bound activation and durable fallback cutover with ADR 036 as specified in
   ADR 038. Stage 1b supplies no process activation witness.

Before enabling the implementation, run the existing `make test`, `make race`, `make cov`,
`make cov-check`, `make check-all` and `make fixtures` checks. Use the native Windows session-file
tests for sharing and replacement behavior.
