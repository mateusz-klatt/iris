# ADR 035: Automatic post-window session succession by `iris mcp`

Status: proposed, 2026-11-22 (amended 2026-11-22 to close the pre-existing `orRegister`
`generation = 0` gap and unify lifecycle authority with [ADR 036](036-single-session-clear-supersession.md)).
Drafted by `antigravity-home-iris` after the coordinator and `gpt-6.1-sol` approved PR A
(in-window reattachment, merged in #381; [ADR 006](006-retention-and-restore.md) amendment of
2026-11-22) and required a dedicated ADR with stronger guards before any implementation of
post-120 s succession. This document proposes changes to D6, D8, D9 and ADR 006 decision 7;
**no implementation starts until this ADR is reviewed and accepted.**

## Context

When the hub restarts, every `active` Execution without an existing `recovery_deadline` gets a
persisted 120 s deadline and enters recovery (D9, ADR 006 decision 7). With #381 (2026-11-22
amendment to ADR 006):

- a bound `iris mcp` reattaches inside the 120 s window on its first tool call or 20 s tick;
- an unbound `iris mcp` can reattach a positively identified, hook-registered session inside the
  120 s window using the hook's local generation memory (`generation > 0`);
- `iris watch` heartbeats immediately on reconnect and `reset`.

That leaves two related post-120 s problems:

1. **Legitimate sessions stuck `lost` after >120 s outages.** When a hub outage, deploy rollback,
   laptop sleep, or network disconnect lasts **longer than 120 s** (or past a 60 s lease), the hub
   sweeps Execution `ex_1` (generation `G`) from `active` to `state = 'lost'`, revokes its claims,
   and requeues its unacknowledged deliveries. Once `ex_1` is `lost`, `heartbeat` and
   `POST /v1/sessions` with `generation = G` are refused with `lost` (D9, ADR 006). Until a
   registering hook fires (`SessionStart`, prompt on clients whose prompts register, or pre-edit),
   `iris mcp` stays locked in `lost` even though its client process and MCP transport are alive.
2. **Pre-existing unguarded `generation = 0` succession via `orRegister` (identified in #381
   review).** In `internal/binding/binding.go`, a freshly started legacy `iris mcp` with no local
   generation memory (`Recall` returns absent, `r.last.HookSeen == false`, `len(r.gone) == 0`), or
   a fresh `iris watch` (`cmd/iris/watch.go` installs no `Recall`), still falls through `/v1/bind`
   (`unbound`) into `orRegister()` -> `register()` with `source = "mcp"` and `generation = 0`.
   After the 120 s window expires, `internal/store/sessions.go` accepts that `generation = 0`
   request and creates `previous.Generation + 1` on `priorAgent` (or a new synthetic
   `mcp-<pid>-<start_ms>` Execution for single-session processes). In effect, that performs
   post-window succession through the D6 untrusted-folder fallback with **none** of ADR 035's
   guards (no hook-equivalent proof, no `/clear` incarnation check, no backup-restore check, and no
   generation-memory CAS).

Naive automatic re-registration from `iris mcp` or `iris watch` is unsafe for four reasons:

1. **Resurrection after `SessionEnd` or `/clear`.** A stale or orphaned MCP process, or one
   belonging to a session superseded by `/clear`, could silently spawn a replacement Execution and
   hijack the agent's mailbox slot.
2. **Restore rollback violation (ADR 006 decision 7).** On a hub restored from an older backup
   that only contains generation `G` while the host has already advanced to `G+2`, falling back to
   `generation = 0` or picking an older predecessor would grant unintended authority or fork
   history.
3. **Hook-policy bypass.** Allowing an MCP shim or monitor to register a replacement for a session
   whose hooks previously reached the hub (or for a client whose MCP process lacks positive,
   stable session identity) breaks D6's rule that hooks are the session's registrar.
4. **Generation-memory and concurrency races.** An unsynchronized write of `G+1` to
   `~/.config/iris/sessions/<hash>` from a late MCP process could roll back a newer generation
   `G+2` already written by a hook, and concurrent succession attempts from multiple shims or a
   hook could split state or stall hook recovery.

## Decision

Introduce a narrow, explicit **post-window succession** protocol (`POST /v1/sessions/succeed` or
an explicit succession flag on `POST /v1/sessions` — exact wire surface decided during review,
referred to below as **succession**) paired with a **server-side refusal of `generation = 0`
non-hook fallbacks over hook-registered sessions** and unified with [ADR 036](036-single-session-clear-supersession.md)'s
per-process `(incarnation, generation)` lifecycle authority.

An `iris mcp` process may create a fresh Execution `ex_2` (`generation = G+1`, zero inherited
claims) for its own expired hook-registered session **only** when every client precondition,
server guard, incarnation guard, restore guard, and concurrency rule below holds. Any missing,
stale, ambiguous, or unreadable check **fails closed** with `lost` and never falls back to
`generation = 0`.

### 1. Hook-equivalent authorization

Succession is permitted **only** when the shim presents evidence equivalent to what the hook
policy would authorize for that exact current session:

- **Supported clients with positive, stable, hook-registered identity only.**
  - Eligible clients must have both (a) a positively identified `session_id` (Codex id-mode root
    via `_meta.sessionId == _meta.threadId`; Claude Code / Copilot / Grok via their client-owned
    session variable or retained `hook_seen` session identity verified against the client process;
    Antigravity / Kimi / Cursor only when positive current-session and incarnation evidence from
    the client's hook-maintained process state confirms the exact `(session_id, incarnation)` —
    see decision 2).
  - **Explicitly excluded:** Codex id-mode sub-agents and foreign-session lookups (ADR 017),
    `iris watch` (monitors never register or succeed), Antigravity/Kimi wake helpers, live
    `IRIS_EXECUTION` overrides, explicit handle arguments, and any session that has never been
    registered by a hook (`hook_seen_ms == 0`).
- **Verified parent process identity and link to the prior Execution.**
  - Before attempting succession, `iris mcp` must verify that its immediate OS parent is alive
    (`parentAlive()`) and re-verify its nearest client process ancestor `(client_pid,
    client_pid_start_ms)` via `proc.Nearest` (confirming pid + start time match before and after
    inspecting local state, preventing pid reuse).
  - The request carries `(client_pid, client_pid_start_ms, prior_execution_id, prior_generation,
    incarnation)` (with `incarnation` required for single-session clients tracked by ADR 036).
  - On the server, the prior Execution `e` must have `e.hook_seen_ms > 0`, matching
    `(host_id, client, session_id)`, and — whenever `e.client_pid > 0 && e.client_pid_start_ms > 0`
    — matching `(e.client_pid == r.ClientPID && e.client_pid_start_ms == r.ClientPIDStartMS)`
    (except on multiplexed daemons such as Codex app-server where hooks record `single_session = 0`
    and the verified root `_meta.sessionId == _meta.threadId` plus verified live parent process is
    the identity link). If the client process died or its pid was reused (`start_ms` differs),
    the server refuses with `lost`.
- **Hook-equivalent checkout/outside policy.**
  - Succession preserves the prior Execution's repository or outside-git context record rather
    than re-resolving an arbitrary working directory: it never migrates a session between a Git
    repository and `outside:<host>`, never allocates a new repository, and never learns or mutates
    repository aliases (`local:`, `gitdir:`, `remote:`, `root:`, `checkout:`).

### 2. Shared `(incarnation, generation)` authority and `/clear` supersession (unified with ADR 036)

Checking only "no other `active` Execution exists on this client process" is **not sufficient**:
after `/clear`, the user may have started a newer session `S2` on the same client process, and
`S2` may later have expired (`lost`) or ended (`ended`). At that point no other `active`
Execution exists, yet `S1` was already superseded and must never succeed.

ADR 035 and [ADR 036](036-single-session-clear-supersession.md) therefore share **one** lifecycle
authority:

- **Per-process monotonic session incarnation (`ProcessIncarnation`):**
  - For every single-session client process verified by `proc.SingleSession` (`kimi`/`kimi-code`,
    `cursor-agent`, `antigravity`/`agy`, `codex exec`) and top-level single-root `claude`, hooks
    persist a monotonically increasing per-process `incarnation` (`1, 2, 3, ...`) paired with the
    active `session_id` under `hook.Sessions.Lock` **before** sending a `SessionStart` to the hub
    (ADR 036 decision 1).
  - The hub persists the process's `high_water_incarnation` and `current_session_id` keyed by
    `(host_id, client, client_pid, client_pid_start_ms)` in `ProcessIncarnation`, and records
    `incarnation` on each `Execution` row.
- **Server-side supersession checks on succession:**
  - If a `ProcessIncarnation` row exists for `(host_id, client, client_pid, client_pid_start_ms)`:
    - `r.Incarnation` **must** equal `ProcessIncarnation.high_water_incarnation`,
      `prev.Incarnation` **must** equal `r.Incarnation`, and `ProcessIncarnation.current_session_id`
      **must** equal `r.SessionID`. Any lower incarnation (`r.Incarnation < high_water`) or
      mismatched session id is **permanently disqualified** and refused with `lost`, regardless of
      whether the newer incarnation is currently `active`, `lost`, or `ended`.
  - In addition, for any single-session process (`single_session = 1`) or top-level `claude`
    process (`parent_id IS NULL`), if any hook-registered Execution with a different `session_id`
    or higher `incarnation` exists on `(host_id, client, client_pid, client_pid_start_ms)` in
    **any** state (`active`, `lost`, or `ended`) with `rowid > prev.rowid`, `prev` is permanently
    disqualified.
  - Likewise, if any newer Execution already exists for `(host_id, client, session_id)` with
    `generation > prior_generation` — in **any** state (`active`, `lost`, or `ended`) — succession
    from `prior_generation` is refused with `lost`. Only the single highest-generation Execution
    for `(host_id, client, session_id)` may be succeeded, and only when its state is `'lost'`
    (never `'active'` and never `'ended'`).
- **Client-side positive current-session and incarnation evidence, serialized under lock:**
  - Before sending a succession request, the shim acquires `hook.Sessions.Lock` (bounded budget;
    never held across network I/O) to read a consistent snapshot of local hook state:
    1. Local generation memory for `(client, session_id)` must exist, parse as a positive integer,
       and equal `prior_generation`.
    2. For single-session processes tracked by ADR 036, the local per-process incarnation record
       for `(client, client_pid, client_pid_start_ms)` **must** exist, name `session_id` as the
       current session, and provide the exact `incarnation` sent on the request (and for clients
       with additional hook-maintained state such as Antigravity `current_session_id` /
       `superseded_session_ids`, Kimi `kimi-wake/` process map, or Grok process session file, that
       record must agree and must not list `session_id` as superseded).
    3. If any required local record is missing, unreadable, corrupted, locked past budget, or
       disagrees with `session_id`, the shim **does not attempt succession** (fails closed).
  - Because hooks update generation memory and per-process incarnation state under `Sessions.Lock`,
    reading under the lock serializes succession prechecks against concurrent `/clear` and
    `SessionStart` updates.
  - Clients that can `/clear` without restarting their MCP child and have not yet enabled ADR 036's
    per-process incarnation record remain **ineligible for automatic succession** until they record
    it on `SessionStart`.

### 3. Closing the pre-existing `orRegister` `generation = 0` gap (MCP and `iris watch`)

In #381's review, Kimi identified a pre-existing bypass in `internal/binding/binding.go` and
`cmd/iris/watch.go`: a freshly started legacy `iris mcp` with no local generation memory, or a
fresh `iris watch` (which installs no `Recall`), reaches `orRegister()` -> `register()` with
`source = "mcp"` and `generation = 0`. Past the 120 s window, `internal/store/sessions.go` creates
`previous.Generation + 1` (or a stray `mcp-<pid>-<start_ms>` Execution), bypassing every guard in
ADR 035.

Two design choices were evaluated:

- **Choice A (RECOMMENDED): Refuse on the server + disable registration in `iris watch`.**
  1. **Hub guard (authoritative):** On `POST /v1/sessions` and `POST /v1/sessions/outside`, when
     `r.Generation == 0` and `r.Source` is `"mcp"`, `"shim"`, or `"watch"` (i.e., any non-hook
     source):
     - Refuse with `lost` if **any** prior hook-registered Execution (`hook_seen_ms > 0`) exists
       for `(host_id, client, session_id)` in **any** state (`active`, `lost`, or `ended`).
     - In addition, when `r.ClientPID > 0 && r.ClientPIDStartMS > 0` and (`r.SingleSession` or
       `r.Client == "claude"`), refuse with `lost` if **any** prior hook-registered Execution
       (`hook_seen_ms > 0`) or `ProcessIncarnation` row exists for `(host_id, client, client_pid,
       client_pid_start_ms)` in **any** state. This closes the synthetic `mcp-<pid>-<start_ms>`
       Rung-3 fallback path as well as the Rung-2 env-var session path.
  2. **Client guard (`iris watch`):** `iris watch` is a read-only monitor (D6/D9). Configure its
     `binding.Resolver` so `iris watch` **never** calls `orRegister()` / `register()` (`NoRegister:
     true` or equivalent). A watch process only binds, heartbeats, and streams; it never creates
     Executions.
  3. **Preserved untrusted-folder fallback (D6):** A brand-new MCP process in an untrusted folder
     whose hooks **never** reached the hub has zero prior `hook_seen_ms > 0` rows and zero
     `ProcessIncarnation` rows for both its `session_id` and `(client_pid, client_pid_start_ms)`.
     Its initial D6 `orRegister` fallback continues working unchanged (and still hands over to the
     hook via `adopt()` once a hook later runs).
- **Choice B (rejected): Route missing-memory `orRegister` through ADR 035 succession.**
  - A fresh MCP process without generation memory or a fresh `iris watch` lacks `prior_generation`
    and `incarnation` proof. It therefore cannot satisfy the backup-restore rollback check (§4) or
    the `/clear` incarnation check (§2). Allowing a memory-less caller into ADR 035 would defeat
    the fail-closed boundary; keeping it rejected under Choice A guarantees that **only** callers
    presenting verified `(prior_generation, incarnation)` proof can succeed a hook-registered
    Execution, while callers that *do* possess valid local memory already take ADR 035's explicit
    succession path before `orRegister` is ever reached.

**Why Choice A is recommended:** enforcement lives on the hub (so even older or buggy shims/watches
cannot bypass ADR 035 via `generation = 0`), `iris watch` stops acting as an accidental registrar,
and the legitimate D6 untrusted-folder fallback remains intact.

**Limits of Choice A:**
- If local generation memory is deleted or corrupted on a host whose hook-registered session then
  suffers a >120 s outage, neither ADR 035 succession nor `orRegister` will recover the session
  automatically; one registering hook (user prompt, `SessionStart`, or pre-edit) is required to
  re-establish authority. This is the intended fail-closed behavior.
- If retention (ADR 006) later purges every historical Execution and `ProcessIncarnation` row of a
  long-dead session, the hub no longer has historical evidence of `hook_seen_ms > 0` for that
  session ID. In practice, retention windows (days) vastly exceed any live client process's
  lifetime; retention rules must prune `ProcessIncarnation` no earlier than the Executions of that
  process.
- An older hub deployed before Choice A still accepts `source = "mcp"` `generation = 0`; disabling
  `orRegister` in `iris watch` and gating `iris mcp`'s `orRegister` on missing prior hook evidence
  provides client-side defense in depth until the hub is upgraded.

### 4. Fail closed after a backup restore

ADR 006 decision 7 requires that a restored backup never grants restored or downgraded authority
to a host whose session moved ahead after the backup was taken. Succession enforces strict
monotonicity in both directions:

- **Exact highest-generation and incarnation match required.**
  - The shim reads local generation memory (and local `ProcessIncarnation`, when applicable) under
    lock and sends exact `(prior_generation, prior_execution_id, incarnation)`.
  - If local generation memory says `G+2` while the restored hub's highest Execution for
    `(host_id, client, session_id)` is `G` (or `G` is still recovering, or absent), or if local
    `incarnation` exceeds the restored hub's `ProcessIncarnation.high_water_incarnation`:
    - the hub sees `highest.Generation != r.PriorGeneration` or
      `highest.Incarnation != r.Incarnation` and **refuses with `lost`**;
    - the hub never selects an older predecessor (`G`), never downgrades to `G+1`, and never
      creates a row from a mismatched `(prior_generation, incarnation)`.
- **No silent memory erasure or generation-0 retry.**
  - On any refusal (`lost`, `conflict`, network error, timeout), `iris mcp` **never** deletes or
    modifies local generation memory or incarnation state and **never** retries with
    `generation = 0`.
  - If local generation memory is missing, unreadable, or malformed, `iris mcp` refuses to
    attempt succession, never retries with `generation = 0`, and Choice A independently rejects
    any `source = "mcp"` `generation = 0` attempt on the hub.
  - Only a registering hook — on an explicit client lifecycle event (`SessionStart`, prompt,
    pre-edit) — retains the existing D9 rule of discarding stale generation proof and requesting a
    new generation on a fresh hook event (while still preserving ADR 036's monotonic
    `incarnation`).
- **Restored-hub recovery window still wins.**
  - While `prior` is `state = 'active'` with `recovery_deadline IS NOT NULL` (inside the 120 s
    window after restart or restore), succession is refused: #381's in-window reattachment handles
    matching generations, and mismatched generations must fail closed.
  - Succession is eligible **only** after `t.expire()` has committed `prior.state = 'lost'` (past
    its 120 s `recovery_deadline` or 60 s lease deadline) and `prior.state != 'ended'`.

### 5. Exact server transaction checks and resulting Execution

Inside a single `BEGIN IMMEDIATE` store transaction (after `t.expire()` runs as usual):

1. Validate `r.Client`, `r.SessionID`, `r.Host`, `r.PriorGeneration > 0`, `r.Source == "shim"`,
   and required parent process identity.
2. Fetch the single highest-generation Execution `prev` for `(host_id, client, session_id)`
   (`ORDER BY generation DESC LIMIT 1`).
   - If none exists -> `fail("lost", ...)`.
   - If `prev.Generation != r.PriorGeneration` -> `fail("lost", ...)` (catches restore rollback,
     stale shim behind a newer hook generation, and already-succeeded sessions).
   - If `r.PriorExecutionID != "" && prev.ID != r.PriorExecutionID` -> `fail("lost", ...)`.
   - If `prev.HookSeenMS == 0` -> `fail("lost", ...)` (hook-registered sessions only).
   - If `prev.State != "lost"` -> `fail("lost", ...)` (`ended` stays terminal; `active` /
     recovering must use #381's in-window reattach, never succession).
   - If `prev.PID > 0 && prev.PIDStart > 0` and
     `(r.ClientPID != prev.PID || r.ClientPIDStartMS != prev.PIDStart)` -> `fail("lost", ...)`
     (parent died or pid reused).
3. Check unified ADR 036 incarnation and `/clear` supersession:
   - If a `ProcessIncarnation` row exists for `(host_id, client, client_pid, client_pid_start_ms)`,
     require `r.Incarnation > 0`, `r.Incarnation == high_water_incarnation`,
     `prev.Incarnation == r.Incarnation`, and `current_session_id == prev.Session`; otherwise
     `fail("lost", ...)`.
   - If `prev.Single || (prev.Parent == NULL && prev.Client == "claude")`, verify no
     hook-registered Execution exists with the same
     `(host_id, client, client_pid, client_pid_start_ms)`, `session_id <> prev.session_id`, and
     `rowid > prev.rowid` (in any state: `active`, `lost`, or `ended`); otherwise `fail("lost", ...)`.
4. Create successor Execution `ex_new`:
   - `generation = prev.Generation + 1`, `incarnation = prev.Incarnation`;
   - `agent_id = prev.AgentID` (same mailbox, only if no other `active` Execution currently holds
     `prev.AgentID`; otherwise fail closed rather than silently splitting into `-2`);
   - `repository_id = prev.Repo`, `worktree = prev.Worktree`, `branch = prev.Branch`,
     `parent_id = prev.Parent`, `client_pid = prev.PID`, `client_pid_start_ms = prev.PIDStart`,
     `single_session = prev.Single`;
   - `heartbeat_mode = 1`, 60 s lease (`deadline_tick_ms = tick + 60000`), `state = 'active'`;
   - `hook_seen_ms = 0` until a hook reaches the hub for `ex_new` (preserving D8/ADR 017's strict
     meaning that `hook_seen_ms` records hook reachability for *this* Execution, so wake arming
     waits for a real hook on the new generation and a second consecutive succession without an
     intervening hook is blocked);
   - **Zero inherited claims** (all claims of `prev` were already revoked when `prev` expired);
   - Emit `GenerationChange` diagnostic with `reason = "recovery_expired"` or `"lease_expired"`,
     `source = "shim"`, `previous_execution = prev.ID`, `previous_generation = prev.Generation`,
     `execution = ex_new`, `generation = prev.Generation + 1`.

**One-hop limit by construction:** Because `ex_new` starts with `hook_seen_ms = 0`, at most **one**
automatic succession can occur after a hook-registered Execution expires. If `ex_new` also expires
without any hook running, a second automatic succession is refused (`prev.HookSeenMS == 0`) until
a real hook registers again.

### 6. Concurrency, generation-memory locking, and races

To prevent a late MCP succession from clobbering a newer hook generation or deadlocking against
hook registration:

- **Locked compare-and-update (CAS) on generation memory.**
  - Hook `fileMemory.Remember`, `fileMemory.Forget`, ADR 036's local `ProcessIncarnation` updates,
    and MCP succession's memory update all serialize under `hook.Sessions.Lock` (`.lock` in the
    sessions directory, bounded by `hookLockBudget`, never held across network I/O).
  - MCP succession uses a **compare-and-advance** helper:
    `AdvanceGeneration(client, session string, expectedOld, newGen int) (current int, updated bool, err error)`:
    - Takes `Sessions.Lock(budget)`.
    - Reads the current on-disk generation `cur`.
    - Updates the file (atomic temp + `restrict` + rename) **only** if `cur == expectedOld && newGen == expectedOld + 1`.
    - If `cur > expectedOld` (a hook already advanced the file to `G+1` or `G+2` while the MCP
      request was in flight), it **does not overwrite** the file and returns `(cur, false, nil)`.
    - If the file is missing (for example a concurrent `SessionEnd` hook called `Forget`), it
      **does not recreate** the file and returns `(0, false, nil)`.
  - Hook `Remember` likewise never downgrades an existing higher generation under the lock unless
    preceded by an explicit hook stale-proof reset, and `Forget` runs under the lock so
    `SessionEnd` cannot race a stale write.
- **Concurrent attempts adopt the valid winner.**
  - Because the hub runs every registration and succession inside one `BEGIN IMMEDIATE`
    transaction, if two MCP processes or an MCP process and a hook race on `(session_id, G)`:
    - Exactly one commits `generation = G+1` (`ex_winner`).
    - The loser's succession call sees `prev.Generation == G+1 != G` (and `prev.State == 'active'`)
      and receives `lost`.
    - On receiving `lost` from a succession attempt, the losing shim immediately performs one
      read-only `/v1/bind` lookup for `(client, session_id)` (or `(client_pid, start_ms)`). If a
      live `ex_winner` with `generation >= G+1` is found and `!r.gone[ex_winner]`, the loser
      adopts `ex_winner` cleanly instead of failing the call.
- **Daemon/shim recovery never blocks an authorized hook replacement.**
  - A hook is always the authoritative registrar.
  - Because `Sessions.Lock` is held only for local file reads/writes (microseconds, bounded by
    `hookLockBudget = 50 ms`) and never across HTTP calls, a slow or retrying shim never stalls a
    hook's 1.5 s deadline (D3).
  - If a hook registers `G+1` (or `G+2`) at the hub, the hub accepts the hook immediately; any
    in-flight shim succession carrying `G` fails cleanly and adopts the hook's live Execution via
    `/v1/bind`.
  - If a shim has already committed `ex_new` (`G+1`) and a hook fires moments later:
    - If the shim committed `AdvanceGeneration(G -> G+1)` before the hook read memory, the hook
      reads `G+1`, sends `POST /v1/sessions` with `generation = G+1` (and matching `incarnation`),
      and seamlessly attaches to `ex_new` (updating `hook_seen_ms > 0`).
    - If the hook read `G` before the shim wrote `G+1`, the hook sends `generation = G`, gets
      `lost` (`e.Generation == G+1 != G`), drops stale `G`, retries with `generation = 0` (still
      carrying the same `incarnation`), and attaches directly to active non-recovering `ex_new`
      (setting `hook_seen_ms > 0`) without splitting Executions.

## Limits (what this mechanism cannot do)

- **Cannot revive claims or pre-expiry fences.** Any claim held by `ex_1` was revoked when the
  120 s recovery window expired. Succession starts with zero claims; the model must re-check
  claims before editing or let pre-edit hooks take fresh claims.
- **Cannot recover clients that lack positive `(session_id, incarnation)` evidence across
  `/clear`.** Clients whose MCP server survives `/clear` without an immutable per-session ID or
  ADR 036's hook-written per-process incarnation record remain ineligible until their hooks record
  it.
- **Cannot recover a hook-registered session whose local generation memory was lost (Choice A).**
  Refusing `generation = 0` non-hook fallbacks over prior hook-registered sessions means a fresh
  missing-memory MCP or fresh `iris watch` after 120 s stays `lost` until a registering hook fires.
- **Does not run while the client or parent process is dead or after `SessionEnd`.** `ended` is
  always terminal; dead parents stop the shim before any request.
- **One automatic hop without a hook.** Because `ex_new` has `hook_seen_ms = 0`, a second
  prolonged outage (>120 s) with zero intervening hooks still requires a registering hook.
- **Not a substitute for working hooks.** After a backup restore where local memory is ahead of
  the hub, succession intentionally fails closed; a hook must explicitly register on a user turn.
- **Not a security boundary across the same OS user (D12).** Generation memory and incarnation
  files live beside `hub.token` with owner-only permissions; any process running as the same OS
  user can already read the token.

## Verification plan (required before shipping implementation)

Any future PR implementing this ADR must achieve 100% Go statement coverage (`make cov-check`),
pass `make race`, keep `internal/restorecheck` 6/6 green, and include deterministic tests for:

1. **Deadline boundaries:**
   - At `t = 119 s` (inside recovery window): succession is refused (`prev.State == 'active'`);
     #381's in-window reattach handles it instead.
   - At `t = 121 s` (after `t.expire()` flips `prev.State = 'lost'`): succession succeeds with
     `generation = G+1`, `incarnation = prev.Incarnation`, `hook_seen_ms = 0`, zero inherited
     claims, same `Agent`, and requeued unacked mail becomes fetchable and acknowledgeable on
     `ex_new`.
   - Second outage past 120 s with no intervening hook: refused because `ex_new.hook_seen_ms == 0`.
   - `SessionEnd` (`prev.State == 'ended'`): always refused, never succeeded or revived.
2. **Pre-existing `orRegister` `generation = 0` gap (Choice A regression tests):**
   - **Missing-memory legacy `iris mcp` after 120 s (Rung 2 env var AND Rung 3 synthetic
     `mcp-<pid>-<start_ms>`):** hook registers `ex_1`, hub restarts and advances 121 s (`ex_1`
     expires to `lost`), a fresh legacy `iris mcp` with no generation memory (`Recall` returns
     absent) calls `Resolve()` -> refused with `lost` and **leaves the hub's `Execution` count
     unchanged** (zero `G+1` or synthetic fallback Execution created).
   - **Fresh `iris watch` after 120 s:** hook registers `ex_1`, hub restarts and advances 121 s,
     a fresh `iris watch` (no `Recall`, `NoRegister`) calls `Resolve()` -> refused and **leaves
     the hub's `Execution` count unchanged**.
   - **Untrusted-folder D6 fallback still works:** a fresh MCP in a folder whose hooks *never*
     reached the hub (zero prior `hook_seen_ms > 0` or `ProcessIncarnation` rows) still registers
     its MCP-only fallback after `Grace` and hands over via `adopt()` once a hook later runs.
   - **Store direct guard:** `POST /v1/sessions` with `generation = 0` and `source` in
     `{"mcp", "shim", "watch"}` is refused with `lost` whenever a prior `hook_seen_ms > 0`
     Execution (in `active`, `lost`, or `ended`) or `ProcessIncarnation` row exists for the
     session or single-session process.
3. **Parent death and PID reuse:**
   - `parentAlive() == false` -> shim exits without calling succession.
   - Client ancestor PID gone or `StartMS` changed (simulated PID reuse) -> refused on client and
     independently refused on server (`client_pid_start_ms` mismatch).
   - Non-eligible client (Codex sub-agent, MCP-only `hook_seen_ms == 0`, live `IRIS_EXECUTION`
     override, explicit handle, client without positive `(session_id, incarnation)` evidence) ->
     refused.
4. **`/clear` supersession (unified with ADR 036):**
   - Hook registers `S1` (`incarnation = 1`) on `(pid, start_ms)`, then `/clear` registers `S2`
     (`incarnation = 2`) on the same `(pid, start_ms)`.
   - Test all three states of `S2`: `S2` `active`, `S2` `lost`, and `S2` `ended` — in every case
     `S1`'s shim is refused by both the local incarnation check and the server's
     `ProcessIncarnation.high_water_incarnation` check.
   - Missing, unreadable, or ambiguous local incarnation file -> client fails closed without
     calling the hub.
5. **Backup restore rollback (fail closed):**
   - Hub backed up at `G = 1`, host advances to `G = 3` in local memory, hub restored from backup
     and `G = 1` expires to `lost`.
   - Shim presenting `prior_generation = 3` is refused with `lost`; local memory stays `3` (never
     erased or downgraded); zero `generation = 0` fallback requests are sent; `internal/restorecheck`
     remains 6/6 green.
   - Missing or malformed generation memory -> refused on client with zero hub requests and zero
     `generation = 0` fallback.
6. **Hook/MCP races and concurrent memory updates:**
   - Hook `SessionStart`/`UserPromptSubmit` races MCP succession at the hub: exactly one commits
     `G+1`, the losing shim adopts the live winner via `/v1/bind`, no split Executions or `-2`
     agent slots are created, and hook deadline is never blocked.
   - Two MCP shims race succession on `(S1, G)`: one wins `G+1`, the loser adopts the winner.
   - Hook runs right after MCP commits `G+1` (both before and after `AdvanceGeneration` writes
     `G+1` to disk): hook attaches to the same `ex_new` and sets `hook_seen_ms > 0` without
     creating `G+2`.
   - Concurrent `AdvanceGeneration(1 -> 2)` vs hook `Remember(3)` and vs `SessionEnd` `Forget`
     under `Sessions.Lock`: late MCP CAS never overwrites `3` and never recreates a forgotten
     file.
