# ADR 036: Single-session `/clear` supersession and atomic session end

Status: accepted, 2026-11-23. Assessed against `main` at `e03aa76`: **the problem remains open**.
This ADR amends D6, D8, D9, `POST /v1/sessions`, `POST /v1/sessions/outside`, a new
`POST /v1/sessions/end` route, and hook `SingleSession` classification. Until the implementation
merges, `main` keeps today's behaviour. That implementation follows ADR 035's, because both change
registration and session end.

## Context

Kimi `/clear` starts a new native session id and runs `SessionEnd` for the old id concurrently with
`SessionStart` for the new one ([`docs/kimi-wake.md:124-130`](../kimi-wake.md#L124-L130)). The new
start has no payload field that distinguishes `/clear` from startup
([`internal/hook/testdata/kimi/sessionstart_clear.json:1`](../../internal/hook/testdata/kimi/sessionstart_clear.json#L1)).
The durable correlation available to Iris is the positively identified single-session process,
`(host_id, client, client_pid, client_pid_start_ms)`. Antigravity likewise tracks its current
conversation per `(agy pid, start_ms)`, although that local wake state is not hub state
([`internal/hook/antigravity_wake.go:663-681`](../../internal/hook/antigravity_wake.go#L663-L681)).

### Open behavior on current main

1. **A new session id allocates before the old process session necessarily ends.** Existing-session
   renewal only matches the same `(host, client, session_id)`. A different id proceeds to repository
   and Agent allocation without looking for another Execution on the process
   ([`internal/store/sessions.go:419-502`](../../internal/store/sessions.go#L419-L502)). The slot loop
   skips every Agent with an active Execution
   ([`internal/store/sessions.go:506-538`](../../internal/store/sessions.go#L506-L538)). Consequently,
   an old session whose end is still in flight, fails open, or never arrives can make the new session
   take another mailbox slot; a later `/clear` can take the first slot back.
2. **Two active Executions on one single-session process remain ambiguous to Rung 3.** The process
   lookup matches every `single_session=1` Execution, including subagents, and refuses multiple
   matches ([`internal/store/sessions.go:606-654`](../../internal/store/sessions.go#L606-L654)). The
   current Kimi `/clear` test explicitly pins that behavior
   ([`internal/store/bind_process_test.go:44-63`](../../internal/store/bind_process_test.go#L44-L63)).
   A long-lived MCP child can also keep renewing the old Execution until it observes the loss.
3. **`SessionEnd` is register-then-end, not lookup-and-end.** The hook calls `register` and then
   `EndExecution` sequentially ([`internal/hook/runner.go:267-290`](../../internal/hook/runner.go#L267-L290)).
   With no remembered generation and no active Execution, registration allocates the next generation,
   potentially a Repository and Agent, before the hook ends it
   ([`internal/store/sessions.go:475-567`](../../internal/store/sessions.go#L475-L567)). The `.ended`
   marker is written only after that sequence succeeds; it does not prevent the allocation
   ([`internal/hook/memory.go:115-145`](../../internal/hook/memory.go#L115-L145)). There is no
   session-keyed end route in the current route table
   ([`internal/hub/hub.go:86-87`](../../internal/hub/hub.go#L86-L87)).
4. **Cursor IDE is still classified as single-session by the hook.** `sessionReq` ORs process
   evidence with `singleSession(ev.Client)`, which returns true for every Cursor event
   ([`internal/hook/runner.go:622-668`](../../internal/hook/runner.go#L622-L668)). This overrides
   `proc.SingleSession`, which deliberately admits `cursor-agent` but excludes Cursor IDE
   ([`internal/proc/proc.go:294-323`](../../internal/proc/proc.go#L294-L323)). Process supersession
   must not ship while that override remains.

### Shipped constraints this ADR must preserve

The following changes are adjacent guarantees, not implementations of this ADR's two mechanisms:

| Change | Current guarantee and evidence | Why the problem remains open |
|---|---|---|
| #381 and #408, reattachment and terminal non-hook registrations | Exact-generation hook, shim and watch reattachment works during the 120-second recovery window; ended Executions stay terminal ([ADR 006](006-retention-and-restore.md#amendment-of-2026-11-22-in-window-reattachment-by-iris-mcp-and-iris-watch), [`internal/store/sessions.go:419-476`](../../internal/store/sessions.go#L419-L476)). After an ended predecessor, generation-zero `mcp`, `shim`, `watch` and legacy empty-source registrations get `lost` and create nothing ([`internal/store/sessions.go:475-484`](../../internal/store/sessions.go#L475-L484), [`internal/store/floor_bypass_repro_test.go:9-128`](../../internal/store/floor_bypass_repro_test.go#L9-L128)). | Recovery and terminal-source checks neither order two native ids during `/clear` nor supply an atomic end. The #408 guard examines the newest retained Execution; retention can eventually remove that witness ([ADR 006:225-229](006-retention-and-restore.md#L225-L229)). |
| #389 and #394, generation floor, `.ended`, `Forget` | Registration retries carry `retry_generation`; `SessionEnd` is excluded from proof-dropping retry ([`internal/hook/runner.go:434-475`](../../internal/hook/runner.go#L434-L475)). A successful end records the highest ended generation and clears only matching proof; marker failure retains proof and is visible to doctor ([`internal/hook/memory.go:115-186`](../../internal/hook/memory.go#L115-L186)). | The floor prevents a downgrade, and the marker protects local proof. Neither stops generation-zero registration from creating a row on an end, and neither ends another session id on the process. |
| #392, Rung 2 reattach | Session-only shim reattach is pinned to the native session rather than requiring a process match ([`internal/store/shim_reattach_test.go:98-130`](../../internal/store/shim_reattach_test.go#L98-L130)). | This is recovery binding only. It does not supersede a `/clear` predecessor or resolve two live process matches. |
| #402 and #405, Antigravity state persistence | `current_session_id` and `superseded_session_ids` guard wake helpers ([`internal/hook/antigravity_wake.go:663-681`](../../internal/hook/antigravity_wake.go#L663-L681)). Windows uses POSIX replacement where supported, with bounded classic-rename fallback ([`internal/hook/antigravity_file_windows.go:32-67`](../../internal/hook/antigravity_file_windows.go#L32-L67)); process and conversation write failures are recorded for doctor ([`internal/hook/antigravity_wake.go:753-770`](../../internal/hook/antigravity_wake.go#L753-L770)). | These files control wake ownership, not Execution registration, mailbox allocation or hub transaction order. Their persistence cannot make hub supersession atomic. |
| #399, stall credit | A bounded hub-stall credit runs inside expiry before an operation ([`internal/store/store.go:672-729`](../../internal/store/store.go#L672-L729)); D8 caps it and never revives a lost row ([`docs/architecture.md:614-628`](../architecture.md#L614-L628)). | Credit changes which rows are live after a hub stall. It is not a `/clear` end and can deliberately keep the predecessor live longer. |
| ADR 006 | The restore floor and exact-generation recovery are accepted invariants ([ADR 006:179-212](006-retention-and-restore.md#L179-L212)). Automatic post-window succession is explicitly deferred to another ADR ([ADR 006:176-177](006-retention-and-restore.md#L176-L177)). | ADR 036 must compose with those rules rather than bypass them with generation-zero registration or shim reattachment. |

Thus neither core problem is superseded on main. The stale-proof protection already present must
remain, while the atomic-end design must additionally return the generation needed by the newer
`.ended` contract.

## Decision

### 1. Supersede only on positive initial `SessionStart` evidence

Add `supersede_process` (default `false`) to `POST /v1/sessions` and
`POST /v1/sessions/outside`.

A hook sends `supersede_process: true` only when all of these hold:

- the event is `KindSessionStart`, never `KindSessionEnd`, `KindPrompt`, `KindPreInvocation`,
  `KindPreTool`, `KindSubagentStart`, `KindStop`, MCP, shim or watch traffic;
- this is a top-level session (`parent_session_id` is empty), registered with `source: "hook"`;
- this is the initial registration attempt (`generation == 0 && retry_generation == 0`), not the
  ADR 006 floor retry;
- `proc.SingleSession(anc, string(ev.Client))` is true and both `anc.PID` and `anc.StartMS` are
  positive.

The hub honors the field only when the request independently has `Source == "hook"`, no parent,
`Generation == 0`, `RetryGeneration == 0`, `SingleSession == true`, and positive process identity.
Client JSON is evidence for an idempotent protocol decision, not authority outside the host-token
boundary (D13).

Registration first performs the existing lookup and renewal for an active Execution with the same
`(host, client, session_id)`, including all generation, recovery and floor checks. A duplicate or
hedged start therefore renews its own Execution and supersedes nothing. Only when that lookup finds
no active Execution does supersession look for active top-level Executions `old` on the exact same
process:

```sql
WHERE e.host_id = ?
  AND e.client = ?
  AND e.client_pid = ?
  AND e.client_pid_start_ms = ?
  AND e.single_session = 1
  AND e.parent_id IS NULL
  AND e.session_id <> ?
  AND e.state = 'active'
```

These are predecessor **roots**, not the complete end set. Every active descendant reachable through
`parent_id` from a selected root belongs to the same `/clear` lifecycle and is included under
decision 3. Ending only the root would leave a child with `single_session=1` matching Rung 3 beside
the successor: the process predicate has no top-level exception for a single-session child
([`internal/store/sessions.go:626-630`](../../internal/store/sessions.go#L626-L630)).

A recovering `old` is still active and may be ended by this positively classified new start. A lost
or ended row is never revived or ended again. Expiry, including #399 stall credit, has already run
for the store operation and decides which rows are active.

### 2. Reject reverse and stale registration, not only reverse ending

Before any generation-zero creation for a top-level hook registration with positive single-session
process identity, the hub applies a monotonic same-process guard. If a different active Execution
`current` exists on that exact process, and the incoming `session_id` already has any Execution on
that process whose `rowid < current.rowid`, the incoming session ran before `current`. The hub
returns `409 lost` and creates nothing.

This guard runs whether or not `supersede_process` was sent. In particular:

- a stale `SessionStart(S_old)` with remembered proof first receives `lost`; its ADR 006 retry has
  `retry_generation > 0`, cannot supersede, and is rejected by this guard rather than registered as
  another active Execution;
- a stale start without proof, or a later stale hook of that old native session, cannot recreate
  `S_old` beside `S_new`;
- a legitimate same-process resume of a previously seen session is deliberately refused too; see
  Limits.

The guard does not infer the order of two session ids that both have prior rows solely from their
ids or wall-clock timestamps. The persisted Execution `rowid` on the exact process is the ordering
witness. An incoming id with no prior row has no such witness; only an eligible initial
`SessionStart` may supersede in that case. Other event types retain their current registration
behavior and never end `current`.

### 3. End the predecessor tree and create the successor in one transaction

When eligible supersession finds one or more matching `old` roots, registration first computes the
closure of each root and every active descendant reachable through `parent_id`, recursively. It
then performs all of the following in the same `BEGIN IMMEDIATE` transaction under `Store.mu`,
before `priorAgent` and slot allocation:

1. Mark every selected root and descendant Execution `state = 'ended'`.
2. Requeue every ended Execution's unacknowledged inflight deliveries with the existing `t.requeue`
   semantics (`execution_id = NULL`, `attention = 1`, `receipt_due_ms = NULL`, and one
   `inbox_changed` event per freed delivery). A child requeues to its own durable recipient; it does
   not inherit the successor's Agent.
3. Emit a `changed` event for the active-to-ended transitions so streams and console readers observe
   them even when there is no mail or claim to release.
4. Run the existing `t.expire()` path to revoke claims held by every now-ended root and descendant,
   emit release and waiter-wake events, and perform the operation's other expiry work. Claims never
   transfer across `/clear`.
5. Resolve the repository, select or allocate the Agent under decision 4, and insert `S_new` with
   `hook_seen_ms` and its process identity as usual.

Ending the descendant tree is deliberate. `/clear` retires the root conversation and its delegated
work, so no child remains as an independent live session on that process. The alternative of
excluding single-session children from Rung 3 would change D6 binding for ordinary subagents
globally and still leave their claims and heartbeats live. The same descendant closure applies to
supersession and to `POST /v1/sessions/end` when that route ends a top-level root. Ordinary
subagent binding and the existing Execution-id end semantics do not change, and children of
unrelated roots are untouched.

The implementation uses one shared store helper for the selected end set, mail requeue, change
notification and claim cleanup on supersession, `POST /v1/sessions/end`, and the existing
Execution-id end route. Supersession and a session-keyed top-level root end supply the
root-and-descendant set; a session-keyed non-root end and the existing Execution-id route retain
their one-Execution selection. The helper must not grow a second, subtly different mail or claims
implementation inside `register`.

This closes the end-first race. If `SessionEnd` commits first, it ends the root **and every active
descendant** in that root's transaction, so `SessionStart` can find no active predecessor root but
also leaves no `single_session=1` child beside `S_new`. If `SessionStart` commits first,
supersession ends the same closure and the later session-keyed end is only an idempotent replay.
Both paths compute the closure before marking the root ended, and both requeue and revoke per
Execution in the same transaction as that root's end.

Any failure—including repository resolution, Agent allocation, event insertion, the new Execution
insert, or commit—rolls back every root and descendant end, mail, claims, events and new registration
together. There is no committed state in which any part of `S_old`'s tree ended but `S_new` was not
registered by this operation.

After commit, an old bound shim or watcher gets `lost` on its next refresh or heartbeat, records the
ended Execution as gone, and can bind again through the ordinary D6 chain. With the whole
predecessor tree ended and only `S_new` left active on the process, Rung 3 is unambiguous. The #381
and #392 reattach paths never revive the ended old generation or set `supersede_process`, and #389's
floor prevents a downgrade. #408 is the terminal fallback rule: while the old native id's newest
retained Execution is `ended`, a generation-zero `mcp`, `shim`, `watch` or legacy empty-source
registration returns `lost` and creates no Repository, Agent, Execution or event. Thus a proofless
long-lived client cannot recreate `S_old`; it can only bind `S_new` through positive D6 evidence.

### 4. Reuse the predecessor's Agent only within the same mailbox scope

When creating `S_new`:

1. Its own `priorAgent(r, repo)` wins as before.
2. Otherwise, choose the newest superseded top-level `old` by `rowid` whose `repository_id == repo`,
   whose Agent worktree equals `r.Worktree`, and whose `AgentID` has no active Execution after the
   predecessor trees ended. Reuse that Agent directly. A descendant's different Agent is never a
   candidate for the successor root.
3. If no such Agent exists, run normal slot allocation.

This keeps a terminal on its existing slot across `/clear`, including slot 2 when slot 1 belongs to
a different terminal. Reusing the Agent also makes deliveries requeued from the old Execution
immediately available to the new Execution on that same durable mailbox.

A repository or worktree change, including movement between a checkout and `outside:<host>`, never
reuses the old Agent. The old Execution still ends; mail remains on its old durable mailbox, and the
new context allocates normally. Mailboxes and claims never cross that boundary (ADR 034).

### 5. Add atomic `POST /v1/sessions/end`

Add a session-keyed end operation accepting:

```json
{"client":"kimi","host":"host","session_id":"native-id","generation":3}
```

`generation` is the hook's exact remembered generation, or zero when memory is absent. The route
needs no repository or worktree lookup and covers both checkout and outside-git sessions. It runs
after the store operation's normal expiry and stall-credit step, in one transaction:

| Hub state | Request | Result |
|---|---|---|
| Active Execution exists | `generation > 0` and differs from the Execution | `409 lost`; change nothing and never retry without the captured proof. |
| Active Execution is recovering | `generation == 0` | `409 lost`; recovery requires exact proof. |
| Active, non-recovering Execution has `Generation > 1` | `generation == 0` | `409 lost`; a replacement requires exact proof. Do not risk a delayed old end ending it. |
| Active, non-recovering generation 1, or any active generation with exact proof | `generation == 0` in the first case, otherwise matching positive generation | End it through the shared helper. If the selected Execution is top-level (`parent_id IS NULL`), include its active recursive `parent_id` descendants in the same transaction, with requeue and claim revocation per Execution; a non-root retains one-Execution selection. Return `200 {"ended":true,"execution_id":"...","generation":G}` with the selected Execution's actual generation. Exact proof can end a recovering Execution. |
| No active Execution; a retained `ended` Execution has the requested positive generation | Exact `generation == G` | Return `200 {"ended":true,"execution_id":"...","generation":G}` for the newest such row by `rowid`. This is an idempotent replay: do not transition state, requeue, release claims or emit an event again. |
| No active Execution; a retained `ended` Execution for this session has `generation = 1`, the only generation this proofless request could have ended | `generation == 0` | Return `200 {"ended":true,"execution_id":"...","generation":1}` for the newest such generation-1 row by `rowid`, without any second end or requeue. Never substitute a newer ended generation. |
| No active Execution and no matching ended replay | Any request generation | Return `200 {"ended":false}`. Create no Repository, Agent, Execution, claim, delivery or event. |

Generation zero is therefore sufficient only for a non-recovering first-generation Execution. A
legitimate proofless end of a later generation is deliberately refused too, leaving lease expiry as
the backstop, because the hub cannot distinguish it from a delayed end of an earlier generation.
The generation counter is not restore provenance; ADR 006's equal-history limit remains.

Proofless replay uses that same eligibility rule, not the newest terminal row. The hub looks up the
retained `ended` Execution under the request's exact `(host, client, session_id)` whose generation is
1, the sole generation the request could have ended without proof, and returns generation 1. If
multiple generation-1 rows remain from different histories, the newest such `rowid` determines only
the returned `execution_id`; it does not establish restore provenance. A newer ended generation 2 or
later never satisfies this lookup, and with no retained ended generation 1 the result is
`ended:false`. Positive-generation replay instead uses its explicitly requested generation as the
preceding table specifies.

Every `ended:true` response, whether it ended an active row or replayed an already-ended one, must
carry `generation: G`, including when the request had no proof. The hook treats both identically and
passes that returned `G` to `Memory.Forget`, which writes or raises `.ended` under the sessions lock
and clears only proof equal to `G`
([`internal/hook/memory.go:110-145`](../../internal/hook/memory.go#L110-L145)). It must not pass the
request's zero or assume the latest local generation is the one the hub ended. Repeating `Forget` is
monotonic and matching-only, but the hub does not requeue or emit a second end event on a replay.

This makes the route idempotent across a lost response. If the first request committed end `G` but
its response was lost, the retry finds the retained ended row, returns `ended:true` with `G`, and the
hook performs the local marker and proof cleanup that the missing response previously prevented.
There is no second delivery requeue. Marker damage, write failure or a missed lock still retains
proof and produces the existing doctor observation, exactly as in #389 and #394. An `ended:false`
response writes no marker and changes no proof; it is an idempotent hub no-op, not evidence that a
retained terminal row was lost or ended at a particular generation.

`KindSessionEnd` calls this route first instead of `r.register`. It preserves the current rule that
`SessionEnd` never makes the ADR 006 generation-floor retry. Wrong-generation `lost` leaves both the
replacement Execution and local proof untouched. Existing behavior for local told-state and wake
cleanup remains unchanged.

For an older hub that answers the new route with the exact `hubclient.OlderHub` response, the hook
falls back to legacy `RegisterSession` plus `EndExecution` **only when captured generation is
positive**. With no proof it skips legacy registration and allows the event, leaving lease expiry as
the backstop rather than creating a phantom generation. No timeout, authentication failure or other
error is treated as an older hub.

This removes the registration round trip and all exit-only allocation on a current hub. Its narrow
generation-zero rule also prevents a delayed proofless end from ending a later replacement.

### 6. Make process classification fail closed

Remove the unconditional `singleSession(ev.Client)` override from `Runner.sessionReq` **in the same
change as supersession**. D5's cross-load lockout already returns before repository work or any hub
call ([`internal/hook/runner.go:164-176`](../../internal/hook/runner.go#L164-L176)), so the override
is not needed to keep a cross-loaded hook from registering. `SingleSession` comes only from
`proc.SingleSession(anc, string(ev.Client))`:

- `cursor`: true only for process image `cursor-agent`; image `cursor` (Cursor IDE) stays false;
- `kimi`: true only for `kimi` or `kimi-code`;
- `antigravity`: true only for `antigravity` or `agy`;
- `codex`: true only for `codex` whose argument 1 read from Linux `/proc/<pid>/cmdline` is `exec`;
  app-server stays false, and this classification is false on macOS and Windows because there is no
  such `/proc` evidence ([`internal/proc/proc.go:300-323`](../../internal/proc/proc.go#L300-L323));
- `claude`: false. D6's separate Rung-3 allowance for one top-level Claude session is not positive
  single-session evidence and never enables supersession;
- every other client, unknown ancestor, or multiplexing host: false.

Supersession additionally requires positive PID and start time. No process-name alias, including
Cursor's possible `agent` alias, joins the allowlist without captured process evidence and tests on
the supported platforms. The override removal and its tests are part of the supersession change, not
a later follow-up.

### 7. Keep local wake state outside the hub transaction

Kimi's accepted `SessionStart` continues to publish its process-to-session map after hub acceptance
([`internal/hook/runner.go:479-498`](../../internal/hook/runner.go#L479-L498)). Antigravity continues
its ADR 028 wake-state ordering and stale-trailing-hook handling. Its process state is updated in
the Antigravity adapter before the ordinary repository and registration pipeline
([`internal/hook/runner.go:178-183`](../../internal/hook/runner.go#L178-L183),
[`internal/hook/antigravity_wake.go:645-681`](../../internal/hook/antigravity_wake.go#L645-L681)).

This ADR does not make those files authoritative for Execution order and does not couple their locks
to SQLite. The #402 Windows POSIX replacement, bounded fallback, and #405 doctor recording remain
unchanged. A failed local write can leave wake state stale even though supersession committed, and
Antigravity wake state can advance even if a later hub registration fails open. Both are wake
consequences, not evidence for rolling back or retrying the hub transaction.

## Invariants

An accepted implementation must preserve all of these under real concurrency:

1. Same-session renewal and exact-generation recovery run before supersession. A duplicate or
   hedged start neither ends another Execution nor changes its Agent.
2. Only an initial, top-level, hook-originated, positively classified single-session
   `SessionStart` with generation and retry generation both zero can end a different session id on
   the process. Every other source and event leaves the predecessor alone.
3. Supersession and registration commit or roll back as one state change. After commit, exactly one
   newly registered Execution is active for that start, every matched predecessor root and active
   descendant is ended, each ended Execution's inflight mail is requeued once, and all their claims
   are revoked, never transferred. A session-keyed top-level root end applies the same recursive
   closure and per-Execution cleanup in its own transaction. Whether root end or supersession commits
   first, no predecessor child remains to make Rung 3 ambiguous.
4. A stale session id with an older same-process Execution can neither end nor recreate itself
   beside the newer active Execution. No stale generation, floor retry or shim reattachment revives
   an ended row or lowers the ADR 006 floor.
5. While the predecessor's ended row is its native session's newest retained row, #408 makes every
   generation-zero `mcp`, `shim`, `watch` or legacy empty-source registration return `lost` without
   creating a Repository, Agent, Execution or event.
6. Agent reuse is stable within `(repository_id, worktree)`, never shares an Agent with an active
   Execution, never chooses a descendant's different Agent for the successor root, and never crosses
   repository, worktree, checkout/outside, or host boundaries.
7. `POST /v1/sessions/end` never allocates on exit. Exact stale proof cannot end a replacement;
   proofless end can neither bypass recovery nor end a later replacement generation. Every
   `ended:true` response names the generation actually ended so `.ended` and proof cleanup are
   monotonic and exact. A replay matching an already-ended generation returns `ended:true` without a
   second state transition, requeue, claim release or event. Generation-zero replay reports only a
   retained ended generation 1 for the exact session key, never a newer ended generation that the
   proofless request could not have ended.
8. `t.requeue`, claim release, waiter wakes, stream notification, retention, restore-floor checks,
   and #399 stall-credit bounds retain their existing semantics. A stall-credited predecessor may
   be superseded while active; a lost predecessor is never revived.
9. Cursor IDE (image `cursor`), Claude, Codex app-server, Codex without Linux `/proc` evidence,
   unknown launchers and incomplete process identities never trigger process supersession.

## Alternatives considered

### Keep register-then-end and add client-side serialization

A sessions-directory lock could order local hooks, but it cannot make two hub requests one SQLite
transaction. A crashed or timed-out hook cannot hold the lock across the whole client lifecycle, and
Antigravity or Kimi local wake files do not cover every client. A missing `SessionEnd` still leaves
two active Executions. Rejected: it preserves the hub race and adds cross-process blocking to D3's
tight deadline.

### End by Execution id before every start

The hook could cache an Execution id and end it before registering a new native id. That requires
reliable local state, cannot prove that a different native id is a `/clear` successor rather than a
concurrent chat, and still leaves hub end and registration non-atomic. It also mishandles absent or
stale generation memory. Rejected in favor of positive process evidence and one hub transaction.

### Reuse the old Agent without ending the old Execution

Name stability alone would leave two active Executions on one mailbox and process. Rung 3 would stay
ambiguous, old claims and heartbeats would remain live, and delivery ownership would be unclear.
Rejected: mailbox continuity requires the atomic end, not just a different slot query.

### Rely on lease expiry, recovery reattachment or stall credit

Lease expiry eventually frees claims and slots, but leaves a 60-second or 15-minute window rather
than establishing `/clear` order. The #381 window is for hub restart recovery, and #399 can extend a
live lease after a hub stall. Neither is a session-end mechanism. Rejected as a substitute; both
remain unchanged inputs to the liveness check.

### Treat Antigravity `current_session_id` or Kimi's map as hub authority

These records are useful wake prechecks, but they are local, independently locked, and can fail to
persist. Antigravity's update also runs before ordinary registration. Using them to authorize hub
ends would create split-brain outcomes and still would not atomically update mail and claims.
Rejected: the hub uses its own serialized process history; local wake state stays advisory to wake.

### Apply process supersession to every client or every Cursor process

A process that multiplexes conversations can host legitimate concurrent chats. Ending every other
Execution on that process would revoke another chat's claims and strand its mailbox. Rejected:
positive single-session classification and the Cursor IDE correction are mandatory preconditions.

## Limits: what this cannot do

- **A fresh OS process is not a `/clear` witness.** If `/clear` changes `(client_pid,
  client_pid_start_ms)`, the hub cannot distinguish the new session from another terminal. Normal
  slot allocation applies until the old Execution ends.
- **A missed or failed initial `SessionStart` performs no supersession.** Later prompts, edits,
  shell registrations, shims and watches never set `supersede_process`; the predecessor ends through
  `SessionEnd` or lease expiry. This ADR adds no general same-process succession mechanism for
  non-start events.
- **A delayed first-ever start has no ordering witness.** If neither `S_old` nor its first start
  reached the hub before `S_new` registered, the later arrival of `S_old` has no older Execution row
  for the monotonic guard. The hub cannot reconstruct native launch order from the ids alone.
- **Same-process return to an old native id is refused.** The monotonic guard cannot distinguish an
  explicit same-process `/resume` of a superseded id from a stale replay. Resume in a fresh process,
  which has a new process identity, is unaffected. Antigravity's wake-state support for conversation
  resume does not override this hub rule.
- **Existing split-brain pairs are not necessarily repaired by a duplicate start.** Same-session
  renewal runs first and supersedes nothing. An already active old/new pair therefore relies on an
  eligible different-id start, an explicit end, or lease expiry.
- **Proofless end is intentionally narrow.** A later-generation active Execution refuses a
  generation-zero end even when the current session legitimately lost its local proof; its lease is
  the backstop. Only a non-recovering first generation can end without proof. As ADR 006 already
  notes, equal generation counters from different restored histories carry no restore provenance,
  so this rule is not proof that every generation-one end belongs to the same history.
- **Local generation and wake files are not part of SQLite's transaction.** A committed hub end can
  be followed by `.ended` or Antigravity write failure; proof is then retained and doctor names the
  failure under the existing rules. Conversely, local wake state can change while a hub call fails
  open. No distributed atomicity is claimed.
- **Claims do not carry over.** The cleared session loses its edit context, so old claims are
  revoked and waiters wake. Only the eligible durable Agent mailbox and its requeued mail carry
  over.
- **#408's non-hook terminal guard is retention-bounded.** It checks the newest retained Execution.
  Once retention removes that ended row, the hub has no terminal witness for the guard
  ([ADR 006:225-229](006-retention-and-restore.md#L225-L229)). ADR 036 relies on the retained row
  immediately after supersession and adds neither a new tombstone nor a change to retention.
- **Automatic post-window succession is out of scope.** ADR 036 acts on a registering hook with
  positive same-process evidence. It does not decide ADR 006's deferred automatic succession after
  the 120-second recovery window ([ADR 006:176-177](006-retention-and-restore.md#L176-L177)).
- **Mixed versions provide mixed guarantees.** An old hub ignores `supersede_process` and lacks the
  session-keyed end route; a new hook's proofless end therefore skips legacy registration and name
  stability is unchanged. An old client on a new hub sends neither field and retains register-then-end
  behavior, including possible phantom allocation. Upgrade the hub first, then clients together.

## Implementation verification

### Store, routes and transactions

1. For an eligible start, assert the old Execution ends and the new one commits on the same Agent;
   unacknowledged inflight mail is requeued once with attention, claims revoke and waiters wake, and
   a `changed` event is committed. Repeat with a recovering old Execution and with a stall-credited
   old Execution.
2. Give a predecessor direct and nested active children with `single_session=1`, including children
   with their own inflight mail and claims. Assert the whole root-and-descendant closure ends, each
   Execution's mail requeues once to its own recipient, all their claims release, and Rung 3 finds
   only `S_new`. A child of an unrelated root remains active and keeps its mail and claims. Assert
   the successor root never inherits a descendant's different Agent.
3. Inject failures at every new statement and at commit through the existing fault-driver and
   rollback harnesses. Each failure exposes none of: root or descendant end, delivery changes, claim
   release, events, Repository/Agent allocation or new Execution. Cover both session routes and the
   shared end helper.
4. Prove slot-1 and slot-2 stability across repeated `/clear`; prove no Agent reuse across
   repository, worktree, host or outside-git boundaries while the old Execution still ends.
5. Cover every eligibility refusal: non-start, non-hook source, parent session, `generation > 0`,
   `retry_generation > 0`, `single_session = false`, missing or partial PID/start, different process
   and different host. Same-session renewal, recovery proof and floor checks run first in every case.
6. Race duplicate starts and different-id starts on one process, including a predecessor with active
   children. Exercise `/clear` in both explicit orders: session-keyed root end commits before
   `SessionStart`, and supersession commits before `SessionEnd`. In both orders, assert the root and
   every active descendant are ended, no child remains active beside `S_new`, Rung 3 finds only
   `S_new`, and every root and child gets its mail requeue and claim revocation exactly once; the
   late root-end replay adds no second requeue. Also assert idempotent same-id renewal, at most one
   active successor for each accepted supersession, and no reverse-supersession or stale
   re-registration with remembered proof, floor retry, or no proof.
7. Immediately after supersession, send generation-zero registrations for the old native id with
   `source` equal to `mcp`, `shim` and `watch`, plus the legacy empty source, on both registration
   routes. Assert #408's `lost`, unchanged retained history, no new Repository, Agent, Execution or
   event, and an unambiguous binding to `S_new`. Include positive retry-generation variants, keep a
   negative retry generation `bad_request`, and verify a lost predecessor keeps #408's deliberately
   different succession behavior.
8. Exercise every atomic-end table row for checkout and outside-git sessions: normal end with exact
   proof, proofless first-generation end, proofless refusal for a later generation, late end after
   supersession, already ended, already lost, never registered, wrong-generation replacement,
   recovering with zero proof and recovering with exact proof. Commit an end, drop its response, and
   retry both with exact generation and with generation zero for a first-generation end: each retry
   returns `ended:true` with the actual generation and leaves delivery, claim and event counts
   unchanged, proving no second requeue. With ended generations 1 and 2 both retained, generation-zero
   replay returns generation 1, never the newer generation 2; with no retained ended generation 1 it
   returns `ended:false` even when an ended generation 2 exists. Assert zero row/event allocation on
   every `ended:false` response and the actual ended generation on every `ended:true` response.
9. Keep the existing stale-end replacement test, recovery-window tests, restore-floor and #408
   terminal-source tests, process-ambiguity tests with supersession disabled, and #399 stall-credit
   tests green.

### Hook, memory and binding

1. Assert `supersede_process` appears only on an eligible initial `SessionStart`; it is absent from
   retries, prompts, edits, shell registration, ends, subagents, shim and watch requests. Image
   `cursor` and client `claude` are false while `cursor-agent` is true, using process-image evidence
   rather than the client enum. Prove Codex `exec` classification from Linux `/proc`, and false for
   app-server and on macOS and Windows. Keep the D5 cross-load lockout test proving no hub call.
2. Assert `KindSessionEnd` makes one current-hub call, never registers generation zero on an older
   hub, falls back only on `hubclient.OlderHub` with positive proof, and never retries stale proof
   without that proof. A no-op response does not log a hub failure or alter proof.
3. For a successful end with request generation zero, assert `Forget` receives the response's actual
   generation. Commit an end and lose its response, then verify the idempotent retry drives `Forget`
   again, writes `.ended` and clears matching proof without a second hub requeue. Cover monotonic
   `.ended` writes, matching-only proof removal, damaged markers, replacement failure, lock timeout
   and doctor observations, including delayed higher-generation registration between the hub reply
   and local `Forget`.
4. Verify hedged and duplicate starts and ends are safe, and that a bound old MCP/watch drops the
   ended generation and rebinds to `S_new` on the same Agent without an ambiguous Rung 3. Verify Rung
   2 and shim memory reattachment cannot revive the old ended Execution.
5. Keep #402's Windows rename tests and #405's process/conversation failure-reporting tests green.
   Add integration coverage showing that local Antigravity persistence success or failure does not
   change the hub's supersession decision and is never used as generation or ordering proof.
6. Run the full Go behavior, race and coverage gates (`make test`, `make race`, `make cov`,
   `make cov-check`, `make check-all` and `make fixtures`). No uncovered branch is excluded.

### Live client checks before claiming coverage

- **Kimi on Linux:** in a scratch repository, record Agent, Execution, process id and start time;
  run `/clear` twice; assert the same Agent and slot, one active new Execution, ended predecessors,
  requeued mail readable, claims released, and MCP/watch rebound without restart or `ambiguous`.
- **Antigravity:** repeat on each supported platform, checking helper exit and re-arm behavior,
  `current_session_id`, stale trailing hooks, and doctor output after an induced state-write failure.
  On Windows also exercise POSIX replacement and the bounded classic fallback with an open reader;
  do not claim the fallback replaces a file that a reader still holds open.
- **`cursor-agent`:** on Linux, macOS and Windows, capture the process image and `(pid, start_ms)`
  before and after `/clear`, including an installed `agent` alias only as a separate candidate.
  Claim name stability only where the process identity is preserved and classification is positively
  verified. Cursor IDE gets a concurrency test proving one chat cannot end another.
- Any client or platform that starts a fresh process, lacks a reliable start time, or has no live
  check remains documented under Limits rather than being presented as covered.
