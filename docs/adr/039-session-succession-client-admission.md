# ADR 039: Admit session successors only from exact local proof

Status: accepted by the owner on 2026-11-25, with implementation of stage 1b to follow. The
owner decided the four open questions on 2026-11-25. Until the stage 1b implementation
merges, `main` keeps today's client behaviour.

**In short.** The hub can create a successor Execution even when the shim never receives the
reply. The hub labels shim-created successors so the client can distinguish a child that still
needs proof from a healthy legacy binding. The client checks each binding before using it. A
shim-created child that has yet to receive a hook requires a local record of its generation and
Execution ID. With the predecessor's record still usable, the shim can repeat the original
request and the hub decides which child to return. Without either record, the child waits for
a registering hook. A failed mandatory check follows D3's allow rule and the hook records
why, even when the same attempt independently confirms an end. Failure to save optional
evidence leaves the ordinary claim check in place. D9 governs a confirmed end when the
other mandatory checks complete and the evidence is undamaged.
A local check cannot undo work the hub already committed or protect against someone who can
rewrite the local files. Stage 1b adds a route and reply fields, uses migration 029 from stage 1a,
and takes an estimated 5–8 engineering days. Succession defaults to off and the owner
enables it explicitly after verification and the upgrade of local writers. Rolling back a
hub with live or recovering hookless succession children requires a rollback build that
understands their provenance or a backup restore.

Amends [ADR 038](038-process-activation-and-exact-proof-succession.md) for the stage 1
session-ID shim and registering hooks. It applies D3, D6, D8, D9, D13 and section 5 of the
[architecture](../architecture.md). Process-bound succession, local start intents and
synthetic mailbox cutover remain sequenced with
[ADR 036](036-single-session-clear-supersession.md).

## Context

A bind reply can identify a child after a dropped reply from the request that created it.
Discovery neither supplies the exact proof to use the child nor prevents the shim from asking
the hub to replay the request using the predecessor's exact record. If local pruning removes
that record, the hub can still name the child. The client needs to distinguish it from a
healthy legacy binding that also lacks an exact record. A hook faces a related distinction
after registration. An end marker can veto use of the reply. An optional write failure can
leave the reply usable for the current claim check.

## Decision

The hub identifies the candidate's kind and lineage. The client applies the required checks
under the sessions lock before using the candidate. An eligible shim with the predecessor's
exact record can ask the hub to create or replay even when bind found a child the shim has yet
to admit. Hook admission and optional evidence writes have separate outcomes.

### Terms

- **Exact pair.** A positive generation and its Execution ID for one native session. The
  **predecessor** is the Execution from which succession starts, with generation G and
  Execution ID P. Its exact pair is `(G, P)`. The **child** has generation G+1.
- **Generation file.** The existing `<hash>` file that stores a generation as an integer.
- **Exact record.** The file `<hash>.execution` that stores an exact pair and its origin. A
  shim-origin record also stores the predecessor's exact pair. **Exact proof** is the evidence
  the exact record supplies for the pair.
- **Publication.** Writing the generation file or the exact record. A protected atomic
  replacement commits the exact record locally once it succeeds.
- **End marker and end floor.** A valid `.ended` file holds the end floor. A generation must be
  strictly above that cutoff to pass the local end check.
- **Generation floor.** The minimum generation that can pass the generation check. It is the
  greatest valid generation from the generation file, the exact record and process memory.
  Equality passes this check, subject to the separate Execution ID and end checks.
- **Candidate and admission.** A candidate is a binding from the hub awaiting the client's
  checks. Admission makes the reply usable after those checks pass. A **veto** stops the
  current attempt from using the reply, writing either the generation file or the exact
  record, or cleaning up evidence.
- **Hookless succession child.** A child created by shim succession whose hub row has yet to
  record a registering hook.
- **Eligible shim.** A shim on one of the native session-ID paths listed in
  [Shim transitions](#shim-transitions).
- **Stage 1a and stage 1b.** Stage 1a supplies migration 029 and the hub transaction. Stage 1b
  supplies classified replies, a route to that transaction and client admission. **Replay**
  repeats a `succeed` request with the same predecessor's exact pair.
- **Choice A.** The [ADR 035](035-post-window-shim-succession.md) rule that refuses non-hook
  generation-zero registration over retained hook history for the session or applicable
  client process.

### Local evidence

| File | Contents and role | Writer |
|---|---|---|
| Generation file | A generation for the D9 generation check. It carries no Execution ID. | Older and newer binaries. |
| Exact record | The exact pair and its origin, plus the predecessor's exact pair for a shim-origin record. | A binary implementing stage 1b, from a real hook registration reply or an admitted `succeed` reply. |
| End marker | The end floor for the local session. | The SessionEnd hook's Forget path after the hub confirms the end. |

The files can advance independently. A generation file can be ahead of the exact record.
A bare legacy Execution ID supplies no exact proof. A fresh shim uses a shim-origin record
to check the predecessor against the end floor before sending a heartbeat for a recovering
child that bind does not return.

The shim reads the three files together under the sessions lock before each `succeed`
request and again before admitting the reply. The hook uses the same locked checks for its
registration reply. The end marker can be absent, valid or damaged. An unreadable, malformed,
dangling or non-regular end marker vetoes the attempt, as does a failed mandatory read or
lock acquisition.

Within an attempt, use the greater of the end floor already observed and the end floor read
under the lock. Cleanup cannot discard an end floor that the attempt already used. The shim
checks the end floor against both the predecessor and the child. The hook checks the
Execution it would use under D9. The shim's predecessor check keeps any higher generation
floor seen during the attempt. The hook keeps the generation floor used for its request
when it rechecks the reply, rechecks before a dependent request or sends the D9 retry.

To publish the child's exact record, the shim uses a compare-and-swap. It checks the
predecessor when preparing the first request, before each retry and immediately before
replacing the exact record. At each point the conditions under the sessions lock are the same.

1. The exact record still holds the submitted predecessor's exact pair `(G, P)`.
2. The generation file is absent or its value is at most G.
3. The process-local generation floor is at most G, including any higher generation already
   observed during the attempt.

A generation file at G+1 can name the generation of a different Execution, so it vetoes
publication from G even when the hub returns G+1. A missing exact record or a conflicting
Execution ID also vetoes the swap. If another writer has already published the child's exact
record, the shim can admit the child without rewriting the record. The child's exact pair
and lineage must match, the end and process checks must pass, and the generation floor must
be at most G+1.

The shim writes the child's exact record before writing the generation file. A veto prevents
either write. A failed replacement of the exact record preserves the predecessor's record.
A failed write of the generation file leaves the published exact record intact. Once the
registration reply passes admission, the hook can update the generation file if writing the
exact record fails. Writers preserve an exact record at a higher generation or at the same
generation with a different Execution ID.

The resolver retains the predecessor's exact pair across retries and refreshes. A fresh
shim can use a surviving exact record. An exact pair held only in memory cannot recreate a
missing exact record. A newer exact record for the same session can resolve the retry
through the rules in [Shim transitions](#shim-transitions). Bounded age-based pruning can
remove evidence. If pruning removes the predecessor's exact record before the swap, the
shim waits for a registering hook unless another writer has already published a matching
child's exact record.

### Sessions lock budget

The client sets one monotonic deadline before requesting the sessions lock. It uses the
earlier of the caller's deadline and 50 ms from entry. Acquisition, reads, file replacements
and cleanup in that hold share the deadline. The admission reads and checks are mandatory.
After a hub-confirmed end, writing the end marker is required before removing the evidence
it vetoes. Writing the exact record and generation file is optional for an admitted hook's
current operation. The client prepares protected temporary files before taking the lock and
discards unused temporary files after releasing it. HTTP and diagnostic logging run outside
the lock.

On Windows the client checks the remaining time before each filesystem attempt, including
the `MoveFileEx` fallback when the POSIX form is unsupported. The client does not sleep and
retry an optional replacement after a sharing violation while holding the sessions lock.
Other sessions in the directory use the same lock. A required end marker write can retry
only within the same deadline, with pauses capped by the time remaining. When the time runs
out, the client releases the lock and leaves unfinished optional work for a future
operation. The deadline bounds Iris retries and waits. It cannot interrupt a filesystem
call that has already begun or guarantee acquisition under contention.

### Exact record lifecycle

The SessionEnd hook registers under D9. Registration can use the generation file alone or
proceed with neither file where D9 permits generation zero. The hook then requests the
**exact end** using the Execution ID and generation returned by registration. The hook calls
Forget after the hub confirms the end. Forget uses the hook's D3 deadline and the sessions
lock budget.

1. Under the sessions lock, read and write the end marker with an end floor that can only
   increase. Do this even if pruning removed the generation file and the exact record.
2. If the hook cannot take the lock or cannot read or write the end marker within budget,
   leave the generation file and exact record in place, record the end marker path for
   doctor and stop.
3. Once the end marker is written, check the generation file and exact record independently
   and remove each entry that the end floor vetoes. The end floor also vetoes a shim-origin
   record whose predecessor is at or below it. Preserve newer evidence that passes the end
   check.
4. If removal fails, keep the end marker effective and record the failed cleanup.

A failed end marker write leaves any previous end floor in place. If there was no end
marker, the hub-confirmed end has no new local end floor. The hook records the write
failure as an observation for `iris doctor` and leaves the existing local evidence in
place. There is no repair or retry after the hook attempt. The hub-confirmed end stands.

The 30-day age allows the idle sweep or a hook to clean up a valid end marker. The end floor
still applies until cleanup succeeds. Whether cleanup runs during the sweep or before
registration or publication, the client uses the following order under the sessions lock.

1. Read the end marker again and inspect the generation file and the exact record regardless
   of their modification times or whether one file is absent. A damaged end marker stays in
   place.
2. Remove or confirm the absence of every entry that the end floor vetoes. Keep the end
   marker if a required read, removal or budget check fails.
3. Remove the end marker last. An attempt that previously used the end floor keeps it for
   the rest of that attempt, even if another process completes cleanup.

### Shim transitions

The resolver checks candidates before returning, caching, using or renewing a binding.
Those checks cover cached refresh, the ordinary tick, Codex id-mode lookup, explicit
handles and wake or watch lookups. The resolver uses admitted replies to set `Bound`, feed a
handover or heartbeat, and update remembered evidence. Recovery can also start from an
exact record that was already published.

With the succession setting enabled, the eligible paths are a Rung 2 shim for the session
it runs in and a Codex id-mode root for its own session. Both recheck that the parent is
alive and verify any required process tuple before dispatch and before admission.

Rung 3, Codex sub-agents, foreign-session lookups, watch and wake helpers, active
`IRIS_EXECUTION` overrides and explicit-handle calls cannot attempt succession or use a
hookless succession child through an ordinary binding exception. A verified synthetic
fallback and its handover keep their D6 identity checks.

| Discovery and local evidence | Transition |
|---|---|
| Live ordinary binding or a live child a real hook has registered | Apply the ordinary generation, identity, end and restore checks. A healthy legacy binding can lack an exact record. A registering hook can establish current evidence under D9. |
| Live hookless succession child with a published exact record | An eligible shim can admit the child if the record matches the returned exact pair and lineage and both the predecessor and child pass the end check. The shim rechecks before use or renewal. |
| Live child immediately after the retained predecessor, with matching hub lineage and no published exact record for the child | An eligible shim sends `succeed` with the predecessor's exact pair from its exact record. Bind identifies this transition and supplies no exact proof. |
| No live candidate for the positively identified session | Try D9 reattachment first. |
| Required evidence missing or damaged, end floor at or above a checked generation, forked or unrelated candidate, lower restored history, superseded process or failed recovery request | The resolver leaves the candidate unusable. |

**After a refusal.** A transport outcome that leaves the hub commit unknown preserves the
predecessor's exact pair for another attempt. The resolver can retry while the exact record
and the generation, end and process checks still permit it. A local veto leaves open later
admission of the same candidate. An authoritative loss of an Execution at the hub prevents
the resolver from reusing that Execution.

**Retrying.** The plain shim's call and tick and the Codex root's call use the same internal
transition to replay. Before each bounded `succeed` request, including a transport retry, the
resolver rechecks identity, parent, local evidence and the end and process checks. An unrelated
`lost`, authentication or transport error cannot select another predecessor. A `succeed`
refusal uses only the existing D9 binding errors. Confirmed loss and local generation vetoes
map to `lost`. A first bind or Codex id-mode lookup with no usable binding maps to `unbound`
under the existing lookup rules. These rules also map an older hub's raw `404 not_found`
for the stage 1b route. The ordinary tick returns no MCP tool result, lookup-only callers
send no `succeed` request, and the replay transition stays internal.

**Accepting replies.** A `succeed` reply must name the same session and the immediate child of
the submitted predecessor's exact pair, with consistent provenance. The resolver must pass
the final checks under the sessions lock before using it. If another writer has published a
matching exact record, the shim can admit the same child from the reply or through bind. A
bind reply for a hookless succession child must match both the exact pair and the lineage
recorded locally. A hook-created winner is usable when the same hub transaction identifies its
lineage or the ordinary binding checks independently establish a current hook-registered
binding. Generation-zero registration and synthetic fallback are unavailable after a recovery
refusal. Fresh no-hook bootstrap continues under D6 and Choice A.

**D9 reattachment.** For the native session on the Rung 2 path, a heartbeat can reattach the
Execution named by an exact record at the current generation floor. The resolver checks the
recorded lineage, the end floor and shim eligibility before dispatch, including for an
already admitted hookless succession child. An authoritative `lost` for a predecessor whose
exact record and checks still permit succession can lead to `succeed`. A recovering hookless
succession child whose exact record was never published needs a real hook.

For Rung 3 memory reattachment, including when refresh finds that a process-bound binding
needs recovery, the resolver sends positive-generation registration with `source:"shim"`,
`client_pid` and `client_pid_start_ms`. The hub checks the verified process tuple and hook
registration in the transaction before renewal or resume. A missing or changed tuple vetoes
reattachment. When the resolver has an exact pair, it also checks that the reply names the
same pair. Rung 3 recovery uses the registration with the process tuple. Reattaching with
only the generation file follows the existing hook-registered D9 path and supplies no
exact proof.

When a heartbeat reattaches a hook-registered pair and the older hub's reply omits
`hook_seen`, the resolver calls `/v1/bind` by Execution ID. The resolver requires the same
session and exact pair, with `hook_seen:true`, before admitting the reply. A lease-only
reply for the same already-admitted pair keeps the established hook state. Missing hook
state cannot promote a hookless succession child or allow a wake.

### Wake and MCP renewal

Codex wake uses `Primary()` to select a target and `StartIfPrimary()` to check it again at native
start. Both require admission and affirmative hub `hook_seen` for the same exact pair. An
admitted hookless succession child can handle MCP work. Wake requires a registering hook,
so such a child produces no wake target, worker, stream or `HookSeen` probe. A resolver
refresh can expose the same exact pair to wake after the hook registers it. The wake
worker's hub `HookSeen` check for the exact pair remains in place.

Primary `whoami` renews the resolved exact pair through the same resolver path as scheduled
and refresh heartbeats. The resolver checks local evidence and any required process identity
before dispatch and sends the applicable source and opt-in. Before returning or caching the
reply, it validates the exact pair and classification and rechecks the current local evidence.
The resolver discards a reply if the binding changed while the request was in flight.
Lookup-only `whoami` performs no renewal. The MCP handler sends no separate heartbeat.

### Hook outcomes under D3

Before every registration request, including SessionStart, registration outside git, a D9
retry and full or reduced combined shell registration, the hook follows this order.

1. Finish checkout discovery or shell status work.
2. Read the generation file, exact record and end marker under the sessions lock.
3. If any of the three is damaged, suppress the request, including any combined shell
   mutation. Record the path and return valid D3 allow output.
4. Compute the generation using the greatest usable generation. Count the exact record if
   the generation file is absent or behind. A healthy generation file remains usable when
   the exact record is missing.
5. Send the request with the effective end floor in `ended_through`.

D9 governs a genuine first registration, an authorized SessionStart after end and a single
stale-generation retry. The retry requires a valid `highest_generation` from the initial
refusal at or above the retained generation floor and sends that generation floor in
`retry_generation`. A failed write of the generation file alone never causes a
generation-zero request.

The hook checks the reply under the sessions lock before using it. A reply below the
generation floor or at the same generation as the exact record with a different Execution
ID fails admission. Where work intervenes, the hook repeats the locked check immediately
before the dependent request. This includes `fileGroups` before TouchFiles and split full or
reduced git status before reconcile. The hook retains the end and generation floors already
used in the attempt. The checks apply to any publication or cleanup that could make the
reply usable.

| Outcome under the sessions lock | Write either the generation file or the exact record? | Send dependent requests? | Hook output and doctor |
|---|---|---|---|
| Confirmed end floor at or above the returned generation, with undamaged evidence and all other mandatory checks complete | No | No | D9's deliberate pre-tool ended refusal or post-shell warning. |
| Independently confirmed end in the same attempt as damaged evidence or an incomplete mandatory check | No | No | Valid D3 allow. Record the damage or incomplete check as an observation. Doctor grades it `silent` if edit or shell protection did not complete. |
| Damaged evidence, reply below the generation floor, or a reply with the same generation and different Execution ID from the exact record | No | No | Valid D3 allow. Record the reason for refusing the reply or the damaged path. |
| Mandatory check misses its lock or time budget | No | No | Valid D3 allow. If edit or shell protection was skipped, record a D3 failure in `session` or `shell session`. Doctor grades it `silent` and warns that edits may have gone unclaimed. |
| Reply admitted, optional write fails or runs out of time | Any successful write stands | Yes, within the remaining budget | Record a write observation. The ordinary claim check still applies. A genuine Touch conflict can deny the edit and hub errors follow the client's D3 policy. The observation alone does not give a `silent` grade. |
| Reply admitted, writes succeed | Yes | Yes, within the remaining budget | The ordinary result. |

ADR 038 says a failed evidence write still emits the normal allow. Here a mandatory
failure that prevents the ordinary check follows D3. An optional write failure after
admission leaves the ordinary decision in place. A failed exact record write supplies no
new exact proof. A failed generation file write leaves any published exact record intact.
SessionEnd keeps its exact end semantics. The reply check in a combined reconcile runs
after the hub has received the snapshot. A local veto prevents use of that reply and writes
to either the generation file or the exact record. It cannot undo work already committed on
the hub.

### Trade-offs

| Discovery and replay option | Benefit | Cost or limit |
|---|---|---|
| Infer succession from the exact record | No bind change | Local pruning and a healthy legacy binding both look like absence. |
| Replay every outward `lost` | Simple routing | Conflates a child awaiting proof with a damaged end marker, a restore refusal or a different identity. |
| **Use durable hub provenance and replay with the predecessor's exact pair** | The hub can identify the kind of child after local pruning and still decides the winner. | Requires additional binding fields and shared admission checks. |
| Write an append-only intent before dispatch and retain it until settlement | Could extend replay through process exit and ordinary pruning. | Requires bounded retention, handling of incomplete intents and coordination with the end marker. An intent cannot survive all local loss or authorize adoption by itself. Stage 1 relies on the surviving exact record and can wait for a hook. |

| Publication option | Benefit | Cost or limit |
|---|---|---|
| Use the generation file to commit a staged exact record | Gives readers one visible generation. | An older writer could make an interrupted write appear complete by advancing only the generation file. |
| **Publish the exact record atomically and keep the generation file independent** | Updating only the generation file cannot create exact proof. A failed replacement preserves the predecessor's exact record. | The files can differ. Readers must apply the generation floor and use the exact record for the exact pair. |
| Fail open on every optional write failure | Simple hook outcome. | Skips an otherwise usable claim check. |
| **Veto on a failed mandatory check and observe optional write failures** | Preserves D3 and authoritative claim handling when the reply is usable. | Requires separate outcomes and one bounded local admission check. |

### Wire, schema and mixed versions

Stage 1a owns migration 029 (`prior_execution_id`, `creation_source`, `creation_proof`), the
create-or-replay transaction and the startup refusal for databases with the ADR 037
worktree-claim schema. Stage 1b requires no further migration or provenance backfill. The
generation file format stays the same. Only binaries implementing stage 1b write the exact
record.

A stage 1b hub adds `prior_execution_id`, `creation_source`, `creation_proof` and the current
`hook_seen` state to successful session, process, combined and explicit bind replies.
The hub sends the provenance fields explicitly, with JSON null for absent values on legacy
rows, and names `succession_proof` in `capabilities`. The combination
`creation_source:"shim"` and `creation_proof:"shim_succession"` identifies a child created by
shim succession. Its predecessor generation is the child's generation minus one. The stored
lineage identifies a hook-created winner from the same predecessor for replay.

The hub includes the same classification in `succeed`, non-hook reattachment and heartbeat
replies that can replace a cached binding. The resolver decodes the classification afresh.
Missing, unknown or inconsistent provenance makes a reply from a capable hub unusable.
A lease-only legacy reply preserves a classification the resolver already knows. An
unclassified reply cannot reclassify a known hookless succession child as legacy.

The **stage 1a route** is `POST /v1/sessions/succeed`. No released client calls it.
The **stage 1b route** is `POST /v1/sessions/succeed/exact`. It calls the same transaction
with the same predecessor's exact pair, source, process fields and `ended_through`. It
returns classified replies even when no earlier bind succeeded. The separate route keeps
an earlier bind reply from serving as evidence about which hub handles the mutating
request. The shim uses only the stage 1b route for `succeed`. An older or stage-1a-only hub
returns `404 not_found` before a mutation, and a stage 1b hub returns `not_found` for the
stage 1a route before any mutation. Neither response leads to ordinary registration as a
substitute. MCP callers receive the existing D9 `lost` or `unbound` result as applicable.

With succession enabled, the stage 1b shim sends `succession_proof:true` on bind and its
own heartbeat. Opt-in identifies the protocol. Exact proof still comes from the local exact
record. The hub applies these guards before returning or renewing a hookless succession
child.

| Request | Hub handling |
|---|---|
| Bind without opt-in, including a handle bind | Returns `lost` for a matching hookless succession child. |
| Heartbeat | Renews a live hookless succession child or resumes a recovering one only with both `source:"shim"` and `succession_proof:true`. Hook and watch heartbeats cannot satisfy the source check. |
| Non-hook registration that would attach to a hookless succession child | Refuses the request on both registration routes. |
| Non-hook generation-zero replacement while a hookless succession child is the newest retained row | Refuses the request on both registration routes, including after the child is lost and its predecessor has been pruned. |
| Genuine hook registration | Applies the ordinary registration rules. Once the hub records `hook_seen`, older clients can bind and renew through their ordinary paths. |

These guards cover older clients that ignore the reply fields. Ordinary legacy rows, hooks,
healthy fallback bindings and D9 in-window recovery keep their wire contracts.

Stage 1a is an unreleased intermediate state on `main` until stage 1b lands. The table
includes it to specify behavior during integration. There is no stage-1a-only release.

| Hub | v0.1.15 or any client without this protocol | v0.1.16 client with succession enabled |
|---|---|---|
| v0.1.15 hub | Existing behavior. | Ordinary binding and D9 recovery on the hub's original schema. The stage 1b route returns `not_found` before any mutation. |
| Stage 1a on `main` (unreleased) | Ordinary paths (`#408`'s `ended` guard active; Choice A deferred to stage 1b). No released client calls the stage 1a route. | Same ordinary paths. The stage 1b route returns `not_found` before any mutation. |
| v0.1.16 hub with stage 1b | Ordinary paths with Choice A. Hookless succession children stay unavailable to older shims and watchers until a hook registers them. | Creates or replays with the predecessor's exact pair and renews admitted bindings. Ordinary paths remain available. |

> **Erratum (2026-11-25):** The stage-1a cell above originally stated that stage 1a enforced Choice A over retained history. Actual stage 1a (`02ed40e`) deferred Choice A to stage 1b—as documented in [docs/architecture.md](../architecture.md)—even though [ADR 038](038-process-activation-and-exact-proof-succession.md#schema-and-compatibility) described Choice A as part of the target new hub. Stage 1b M1 kept Choice A deferred while shipping classified replies and `/v1/sessions/succeed/exact`; M2a enables hub-side Choice A enforcement over all retained hook history as part of the unreleased stage-1b rollout.

The succession setting is an explicit opt-in, defaults to off and serves as a reversible
kill switch. A release version never enables it. The owner enables succession, including
writing and using exact records, only after every process that writes or prunes the shared
sessions directory uses the stage 1b lifecycle. Older writers use the D9 generation file
and can retire an end marker independently of the exact record. Restart old shims and
replace old hook or watcher entry points before enabling succession on shared state. Older
clients can remain in use with a separate sessions directory. A local rollback needs state
from before activation or a separate sessions directory.

A hub without the stage 1b guard cannot safely serve a database with live or recovering
hookless succession children. A pre-029 hub cannot interpret or preserve the new lineage.
Use a provenance-aware rollback build or restore the pre-migration backup under
[ADR 006](006-retention-and-restore.md), accepting the loss of changes after the backup.
Wire compatibility gives no guarantee about opening a later database in place.

## Invariants

1. The client uses or renews a hookless succession child only after admission against an
   exact record. A missing file, a generation file value, a bind reply or a failed
   compare-and-swap supplies no exact proof. A registering hook alone establishes hook
   reachability.
2. With succession enabled and the predecessor's exact record still usable, discovery of
   an immediate child with matching lineage and no matching exact record leads to `succeed`.
   A dropped reply does not advance the local record or consume the retry. A generation
   file above the predecessor's generation vetoes publication from that predecessor.
3. End marker checks, publication, SessionEnd and pruning use the same lock. Damage vetoes
   the attempt even if an earlier check saw no end marker. The shim replays or adopts only
   when both the predecessor and child are strictly above the end floor. An expired end
   marker stays effective until cleanup has removed every entry it vetoes.
4. The stage 1a transaction creates at most one child. The child shares the permitted
   mailbox and repository, inherits no claims and needs a registering hook before another
   automatic succession. Replay adds no lease renewal, grant, event or generation change.
   The client renews only admitted bindings whose Execution the hub has not confirmed lost.
5. Rung 3 reattachment keeps the hub's process check. The resolver checks the exact pair in
   the renewal reply before returning it to primary `whoami`. Codex wake also requires
   affirmative hub `hook_seen` for that exact pair.
6. Hooks use D3's deadline and return valid adapter output. A local write failure alone
   cannot deny an edit. A confirmed end follows D9 when the evidence is undamaged and the
   other mandatory checks complete. Damage or an incomplete mandatory check in the same
   attempt gives D3 allow and still vetoes publication and dependent requests. An
   authoritative claim decision retains its existing behavior. Each hold of the sessions
   lock has one deadline. HTTP runs outside that lock.

## Limits

A dropped reply can be replayed while the predecessor's exact record remains usable and the
hub can still return the live child. Local deletion, expiry, a held Windows handle or an
unavailable lock can leave the child waiting for a hook. A machine crash can lose local
writes. The generation file and hub reply cannot replace the missing exact record. A
backup with identical retained lineage can be indistinguishable from the original hub.
History retention can remove the hub's evidence for Choice A.

The sessions lock orders observed local state. An end or process change after admission can
race a request in flight. Hub order governs what commits. The client cannot roll back a
mutation or detect an end that neither side records. D13 trusts the host token holder for
its own host and the OS user for its local files. Provenance and protocol opt-in cannot
authenticate a native event against someone who can forge it or rewrite the exact record.

## Deterministic verification

Use fake hub time, request barriers and injected read, write and lock outcomes. Run the
decisions through the resolver, real SQLite store and HTTP handlers. Assert requests, cache
and local evidence, claims, lease renewal time, events and generation changes as applicable.

| Case | Interleaving and required result |
|---|---|
| Dropped replies after hub commit | Drop both replies after the hub creates G+1. Bind returns G+1 while the predecessor's exact record remains. The next eligible call or tick sends `succeed` with `(G, P)`, publishes the returned exact pair and admits the child. Use surviving and fresh shims and plain and Codex root call paths. Assert one child and one creation event. Replay leaves the lease unchanged. |
| Competing writers | Pause two shims after preparing the request with `(G, P)`. Release a shim or eligible hook winner first. Move publication before the loser's lookup, during reattachment and during its request. The loser admits only the same child when its checks pass. Try an identical installed exact pair and the same generation with a different Execution ID. |
| Generation file ahead of the predecessor | Leave the exact record at `(G, P)` and raise only the generation file to G+1, first with an older writer and then with a hook whose exact record write fails. Present a delayed same-lineage G+1 reply for a different child after a fork or restore. Advance the generation file before preparation, before retry and between dispatch and the swap. Assert the vetoed shim writes neither file and sends no heartbeat. Prune the higher generation file after the attempt has read it and check that the veto persists. Independently publish the matching child's exact record and check that the child can pass admission without a rewrite. |
| Pruning before the swap | Remove the exact record under the lock after preparation, with and without the generation file. Let the hub commit, then resume the swap. Assert the vetoed shim writes neither file and sends no heartbeat. Prune only the generation file and verify the retained exact record can still advance. Restart with neither file. A session, process or handle lookup of the live hookless succession child fails admission. A healthy legacy binding still works with both files absent. A genuine hook restores the ordinary path. |
| Interrupted publication | Fault the exact record replacement and, separately, the generation file write. Restart after each fault. An older writer updating only the generation file cannot certify the child. A published exact record survives failure of the generation file write. Run the next hook with the generation file absent and behind. It sends the positive generation in the exact record for ordinary and combined registration, with the effective end floor. A higher published generation or the same generation with a different Execution ID vetoes a delayed reply. |
| Windows sessions lock deadline | Use two session IDs sharing the sessions lock and a fake monotonic clock. Spend most of A's time on acquisition and the first replacement. Inject a sharing violation on the second and separately make the POSIX form report unsupported at the deadline. Assert no optional retry sleep, fallback after the deadline or new budget. B can take the released lock and complete its check. Run the sharing case with the actual destination held without delete sharing on native Windows. |
| End marker changes after HTTP | Pause after the early reply check and before admission under the lock. Set the end floor to the predecessor or returned generation, or replace the end marker with malformed data, a dangling link or a non-regular or unreadable entry. Assert no shim adoption on a veto and no write to either the generation file or the exact record by a vetoed shim or hook. A damaged end marker gives valid D3 allow, the end marker observation and no dependent request. A confirmed end with undamaged evidence and all other mandatory checks complete gives D9's refusal or warning. A strictly lower end floor permits publication. Cover both orders of SessionEnd and publication. |
| Exact record and expired end marker | Confirm SessionEnd with the generation file absent or newer than the exact record for the ended Execution. Try the inverse generations and a shim-origin record whose predecessor is vetoed. Assert the hook writes the end marker first. At the retention boundary, run the sweep and on-demand cleanup with distinct modification times and each file absent in turn. Inject end marker write failure, read or removal failure, and interruption between removals. If the end marker write fails, assert a doctor observation, preservation of the local evidence and no repair or later retry. When the end marker was written, assert that every surviving entry it vetoes stays unusable. Preserve newer evidence that passes the end check. |
| Damaged exact record before dispatch | Damage the exact record while discovery or status is paused. Exercise SessionStart, ordinary and outside registration, a D9 retry after its initial refusal, and full and reduced combined shell registration. Require a local path observation, valid bounded D3 allow and zero registration or reconcile requests after the barrier. Repeat with a missing exact record and healthy generation file to verify legacy recovery. |
| Intervening checkout or shell work | Pause in `fileGroups` after registration and in split full and reduced git status before reconcile. Advance or damage the end marker, or damage the exact record, during the pause. Resume after the earlier admission. Require a fresh locked check and zero dependent requests. Confirmed end alone gives D9's refusal or warning. Damage gives D3 allow and an observation. In separate runs, independently confirm the end and inject damaged evidence or a mandatory check that misses its lock or time budget in the same attempt. Require valid D3 allow and the observation, with grade `silent` if edit or shell protection did not complete. Assert that neither file is published and no dependent request is sent. |
| Optional hook write failure | Fail replacement of the exact record after admission. Check normal allow and a real conflict on both Touch paths, plus authoritative 409 retry refusals where supported and malformed or transport failures. Check adapter encoding and the write observation, with no `silent` grade from that observation alone. Separately force a mandatory lock miss during edit or shell admission. Require bounded allow, no dependent request, a D3 failure and `silent`. Also veto a reply with the same generation and a different Execution ID. A combined reconcile may already have committed. |
| Eligibility and boot recovery | Try ended, expired, recovering, superseded, restored-lower and same-generation fork cases. An eligible Rung 2 shim can reattach the same recovering child from its current exact record. Set the end floor at the predecessor, restart hub and shim, and assert no heartbeat. Replay cannot renew a recovering child whose exact record is absent. A hookless succession child cannot start another hop. Exercise dead parent, cross-load, watcher, wake, active override, handles and Codex foreign and sub-agent paths. |
| Rung 3 process check and legacy hook state | Try Rung 3 recovery with an exact record and with only a generation file, including a refresh that finds a recovering row. Vary matching process identity, omitted tuple, different PID and the same PID with a different start time. Assert no heartbeat by Execution ID in place of registration and no renewal or resume from a failed check. Against v0.1.15 omit `hook_seen` on Rung 2 reattachment by heartbeat. Follow with bind replies that confirm `hook_seen:true`, omit it, set it false or name a different exact pair. Only an affirmative bind for the matching pair admits the hook-registered binding. |
| Codex wake and primary `whoami` | Give the Codex root an admitted hookless succession child. Assert `Primary()` and `StartIfPrimary()` refuse it, with no worker or `HookSeen` probe. Register a hook and refresh the same child, then check wake eligibility. On the live admitted pair, MCP `whoami` sends one resolver heartbeat with shim source and opt-in. Inject a wrong exact pair, invalid provenance, a late local end or a concurrent binding replacement. No stale reply reaches MCP or the cache. Lookup-only `whoami` sends no heartbeat. |
| Versions and fallback | Run all six hub and client combinations, including the unreleased stage 1a hub. Verify that the setting defaults to off regardless of release version. Upgrade every writer and pruner of the shared sessions directory before opting in, then verify that turning the setting off disables succession again. Test missing or inconsistent provenance from a capable hub and zero mutation on either stage's unsupported route. Assert D9 `lost` for a confirmed loss or local generation veto and `unbound` for a first bind without a usable binding. Exercise an older hub's raw `404 not_found` on the stage 1b route during first bind and lost-binding recovery. Assert that callers receive only the applicable D9 error and that ticks emit no MCP result. On live and recovering hookless succession children, test legacy bind and all heartbeat sources with and without opt-in. Only `shim` with opt-in renews. Prune the predecessor on the hub and check that non-hook registration cannot renew the child or replace it after loss. Check ordinary recovery, D6 fallback, Choice A, pre-029 backup rollback and startup refusal for the ADR 037 worktree-claim schema. |

## Effort and sequencing

1. Land stage 1a on `main` as an intermediate hub-only state.
2. In stage 1b, first land the classified replies, compatibility guard and stage 1b route.
   Verify mixed-version requests with the real handlers and migration 029. Estimate
   1–2 engineering days.
3. Land resolver admission and replay, wake and MCP renewal checks, the exact record
   lifecycle and the default-off succession setting. The owner enables succession after
   the matrix passes and every process writing or pruning the shared sessions directory
   has been upgraded. Estimate 2–3 engineering days plus 2–3 days for the deterministic
   matrix, native Windows, fixture replay and repository checks. The stage 1b total is
   5–8 engineering days.
4. Sequence process-bound activation and durable fallback cutover with ADR 036 as specified in
   ADR 038. Stage 1b supplies no process activation witness.

Before enabling the implementation, run `make test`, `make race`, `make cov`, `make cov-check`,
`make check-all` and `make fixtures`. Use the native Windows session-file tests for sharing
and replacement behavior.
