# ADR 035: Automatic post-window session succession by `iris mcp`

Status: proposed, 2026-11-23 (revised against `main`). Proposes changes to D6, D8, D9, [ADR 006](006-retention-and-restore.md) decision 7 and its amendments, `internal/hook/runner.go` (`singleSession`), `internal/hook/sessions.go`, `internal/binding/`, `internal/store/`, and `internal/mcpserver/` recovery guidance; **no implementation starts until this ADR is reviewed and accepted.**

## Context

When `iris serve` starts or restores a database, every `active` Execution without an existing `recovery_deadline` receives a persisted 120 s deadline and enters recovery (`internal/store/store.go`, D9, ADR 006 decision 7). Current `main` handles in-window recovery and transient hub stalls through four mechanisms:

1. **In-window reattachment (ADR 006 amendment of 2026-11-22).** Inside the 120 s window:
   - A bound `iris mcp` whose `/v1/bind` refresh returns `lost` immediately heartbeats with its exact remembered generation (`internal/binding/binding.go`, `internal/binding/call.go`).
   - An unbound `iris mcp` (excluding Codex id-mode sub-agents, `IRIS_EXECUTION` overrides, explicit non-gone handles, and `iris watch`) reads the hook's local generation memory (`internal/hook/sessions.go`) and sends a reattach-only registration (`source: "shim"`, `generation > 0`) for a positively identified, hook-registered session (`internal/store/sessions.go`). Rung 2 (`CLAUDE_CODE_SESSION_ID`, `COPILOT_AGENT_SESSION_ID`, `GROK_SESSION_ID`) pins reattachment to `(host_id, client, session_id)` without requiring `(client_pid, client_pid_start_ms)` to match across desktop launchers or wrappers, while Rung 3 pins to the verified client process.
   - `iris watch` heartbeats immediately on reconnect and `reset` frames (`internal/watch/watch.go`), and retains `RecallBindingGeneration` (`cmd/iris/watch.go`) to refuse lower or malformed generations.
2. **Restore generation floor and `.ended` markers (ADR 006 addendum of 2026-11-22).** Registration `409 lost` responses include `error.details.highest_generation` (`internal/store/generation.go`). Hooks retry once with `generation: 0, retry_generation: G` only when `highest_generation >= G` (`internal/hook/runner.go`). `SessionEnd` records `<hash>.ended` under `Sessions.Lock` before removing `<hash>`, keeping `<hash>` if writing `.ended` fails or misses the lock (`internal/hook/memory.go`). `iris mcp` and `iris watch` reject bindings or fallbacks below local proof or with unreadable/malformed `<hash>` files (`internal/binding/binding.go`).
3. **Bounded hub stall credit (migration 028, D8).** While the hub's stream expiry sweep is armed, a monotonic gap above 5 s moves non-recovering active deadlines forward up to `min(now + 20 s, lease_renewed_tick_ms + lease + 60 s)` (`internal/store/store.go`). Stall credit explicitly never moves `recovery_deadline`, never applies when no stream is armed, never credits client, network, or machine sleep stalls while the hub runs, never covers stalls past ~120 s since the last heartbeat, and never revives a `lost` row.
4. **Console read pool (D9 item 5).** Read-only console queries (`ui_overview`, `ui_deliveries`, `ui_threads`, `ui_thread`, `ui_doctor_get`, `ui_events`, `ui_receipts`, and read-only console session checks) run in `BEGIN DEFERRED` on a separate `PRAGMA query_only=ON` pool (`readDB`) without acquiring `Store.mu` or running `expire`, stopping slow console snapshots from delaying heartbeat renewals while the hub is live (`internal/store/store.go`, `internal/store/api.go`).

Two post-window problems remain open on `main`:

1. **Live `iris mcp` sessions locked in `lost` after >120 s outages or >60 s client/network stalls.** Once `t.expire()` (`internal/store/store.go`) marks Execution `ex_1` (generation `G`) `state = 'lost'`, revokes its claims, and requeues its deliveries:
   - `POST /v1/executions/{id}/heartbeat` fails `t.live()` (`internal/store/sessions.go`) with `409 lost`.
   - `reattachByMemoryHeld` sends `POST /v1/sessions` with `generation = G > 0, source = "shim"`, which `internal/store/sessions.go` unconditionally refuses with `409 lost` (`"expired generations cannot be revived"`).
   - `orRegister` refuses fallback whenever `err` is `lost`, `len(r.gone) > 0`, or `r.last.HookSeen` is true (`internal/binding/binding.go`).
   - Every tool call returns `lost` (`internal/mcpserver/server.go`) and every 20 s `Maintain` tick repeats the same failing reattach loop until a registering hook fires. On clients whose prompts do not register (Cursor and Grok, `docs/api.md`), an idle or post-outage session cannot recover even when the user sends a prompt unless `SessionStart` or an edit hook runs.
2. **Remaining unguarded `orRegister` `generation = 0` gaps when local `<hash>` proof is absent.** Although `cmd/iris/watch.go` now installs `Recall` and `fallbackGenerationError` blocks `orRegister` when `<hash>` is present or malformed (`internal/binding/binding.go`), two paths still reach `register()` with `source = "mcp", generation = 0` and create `G+1` (or a new Execution) on `internal/store/sessions.go` without post-window or restore guards:
   - **After `SessionEnd` or missing `<hash>` on Rung 2 / `iris watch`:** `Forget` (`internal/hook/memory.go`) writes `<hash>.ended` and removes `<hash>`. `Sessions.RecallBindingGeneration` (`internal/hook/sessions.go`) reads only `<hash>` and ignores `<hash>.ended`. An unbound Rung-2 shim or `iris watch` (`r.last.HookSeen == false`, `len(r.gone) == 0`) sees `Recall` absent, passes `fallbackGenerationError`, sends `source: "mcp", generation: 0`, and causes `internal/store/sessions.go` to resurrect an `'ended'` (or `'lost'`) hook-registered Execution at `G+1`.
   - **Unbound Rung-3 shim falling back to synthetic `mcp-<pid>-<start_ms>`:** If a Rung-3 shim (`cursor-agent`, `antigravity`, `kimi`, `codex exec`) never bound `ex_1` before `ex_1` expired or ended (`r.last.HookSeen == false`, `r.envSession() == ""`), `recallSessionHeld` returns `("", 0)`. `orRegister` then calls `register` with `session_id = "mcp-<pid>-<start_ms>"` (`internal/binding/binding.go`), `fallbackGenerationError` checks the nonexistent proof file for `"mcp-<pid>-<start_ms>"` rather than the hook's session ID, and `internal/store/sessions.go` creates a stray Execution and splits the session.

Naive post-window succession is unsafe for four reasons:

1. **Resurrection after `SessionEnd` or `/clear`.** An orphaned shim or a shim belonging to a session superseded by `/clear` (whether the newer session is `active`, `lost`, or `ended`) could spawn a replacement Execution and claim the agent's mailbox.
2. **Restore rollback violation (ADR 006 decision 7).** On a hub restored from an older backup at generation `G` while the host's local proof is `G+2`, accepting `generation = 0` or matching an older row would grant restored authority or fork history.
3. **Hook-policy bypass and imprecise `single_session` flags.** Allowing an unverified MCP process to replace a hook-registered Execution—or trusting `single_session = 1` from multi-chat launchers such as Cursor IDE (`internal/hook/runner.go` currently sets `singleSession(ClientCursor) = true` even when `proc.SingleSession` in `internal/proc/proc.go` is `false` for `cursor`)—would break D6's rule that hooks are the session's registrar.
4. **Generation-memory and concurrency races.** Unsynchronized writes from a late MCP process could downgrade a newer hook generation `G+2` or recreate a `<hash>` file deleted by `SessionEnd`, and concurrent shims or hooks could split Executions or stall a hook's 1.5 s D3 deadline.

## Decision

Introduce an explicit, one-hop **post-window succession** protocol on a dedicated route (`POST /v1/sessions/succeed`), paired with **Choice A: server-side refusal of `generation = 0` non-hook fallbacks over any hook-registered session or single-session process** and client-side `.ended` / per-process `/clear` checks under `Sessions.Lock`.

An `iris mcp` process may create a fresh Execution `ex_2` (`generation = G+1`, zero inherited claims) for its own expired (`state = 'lost'`) hook-registered session **only** when every client precondition, server guard, `/clear` supersession check, restore guard, and concurrency rule below holds. Any missing, stale, ambiguous, or unreadable check **fails closed** with `lost` and never falls back to `generation = 0`.

### 1. Dedicated route (`POST /v1/sessions/succeed`)

Post-window succession uses a dedicated HTTP endpoint `POST /v1/sessions/succeed` (store operation `"succeed"`) rather than an optional flag on `POST /v1/sessions`.

- **Fail-closed upgrade safety:** `internal/store/sessions.go` ignores unknown JSON fields on `POST /v1/sessions`. Adding a succession flag alongside `generation = 0` to `POST /v1/sessions` would let an older hub silently ignore the flag and execute an unguarded `generation = 0` registration. A dedicated route returns `404 not_found` (`hubclient.OlderHub`) on older hubs, so upgraded shims fail closed with `lost`.
- **Fixed context preservation:** `POST /v1/sessions/succeed` takes `{client, host, session_id, prior_generation, prior_execution_id, client_pid, client_pid_start_ms}` and omits working-directory / git-discovery fields. Succession preserves the prior Execution's repository (or `outside:<host>` context) rather than re-running repository resolution, allocating repositories, or mutating aliases.

### 2. Hook-equivalent authorization and Rung 2 / Rung 3 identity alignment

Succession is permitted **only** when the shim presents evidence matching what D6 authorizes for that session:

- **Eligible clients and bindings:**
  - **Rung 2 (`claude`, `copilot`, `grok` via client-owned session environment variable) and Codex id-mode root (`_meta.sessionId == _meta.threadId`):** Identified positively by `(host, client, session_id)` plus local `<hash>` proof (`prior_generation`). Consistent with Rung 2 in-window reattachment (`internal/binding/binding.go`, `internal/store/sessions.go`), Rung 2 and Codex id-mode root omit `(client_pid, client_pid_start_ms)` unless the shim's retained `r.last` originally bound through Rung 3 (`r.last.Rung == 3`).
  - **Rung 3 (`cursor-agent`, `antigravity`/`agy`, `kimi`/`kimi-code`, `codex exec`, and top-level `claude` bound via Rung 3 such as `--continue`):** Requires positive `proc.SingleSession(anc, client)` (or `r.last.Rung == 3` with a `claude` top-level session). The shim re-verifies its nearest client ancestor `(client_pid, client_pid_start_ms)` via `proc.Nearest` before and after reading local state (preventing PID reuse) and sends `(client_pid, client_pid_start_ms)` on the request.
  - **Fix `singleSession` over-classification before enabling Rung-3 succession:** Update `internal/hook/runner.go` so `SingleSession` is `true` only when `proc.SingleSession(anc, string(ev.Client))` holds (excluding Cursor IDE `cursor`, which can host multiple chats in one process and matches `internal/proc/proc.go`). Cursor IDE (`single_session = 0`) and multiplexed launchers without per-session env identity remain ineligible for Rung-3 post-window succession.
- **Explicitly excluded:** Codex id-mode sub-agents and foreign-session lookups (ADR 017), `iris watch` (`HeartbeatSource == "watch"`), Antigravity/Kimi wake helpers (`r.wakeSession != ""`), live `IRIS_EXECUTION` overrides, explicit handle arguments, and any Execution whose `hook_seen_ms == 0`.
- **Live OS parent check:** Before attempting succession, `iris mcp` verifies `parentAlive()` (`internal/binding/binding.go`).

### 3. `SessionEnd` and `/clear` supersession guards

Checking only "no other `active` Execution exists" is insufficient: after `/clear`, a newer session `S2` on the same single-session process may itself have expired (`lost`) or ended (`ended`), leaving no `active` Execution while `S1` remains superseded. ADR 035 enforces monotonicity on both sides:

- **Client-side local precheck (under lock, bounded by `hookLockBudget`, never held across HTTP I/O):**
  1. Read `<hash>` (`internal/hook/memory.go`) and `<hash>.ended` under `Sessions.Lock`:
     - `<hash>` must exist, parse as a positive integer, and equal `prior_generation` (and `>= r.processGenerationHeld(session)`).
     - `<hash>.ended` must be absent or parse strictly below `prior_generation`. If `<hash>.ended` is unreadable or malformed (including a dangling symlink) or holds `ended >= prior_generation`, fail closed (and emit `damaged generation proof at <path>` when damaged).
  2. For single-session processes (`proc.SingleSession`), verify local per-process current-session state:
     - **Antigravity:** `antigravity-helpers/<pid>-<start_ms>.json` (`internal/hook/antigravity_wake.go`) must be readable under its lock, list `current_session_id == session_id`, and not list `session_id` in `superseded_session_ids`.
     - **Kimi:** `kimi-wake/sessions/<pid>-<start_ms>.json` (`internal/kimiwake/kimiwake.go`) must be readable under its lock and map `(pid, start_ms)` to `session_id`.
     - **Other `proc.SingleSession` launchers (`cursor-agent`, `codex exec`):** Hooks persist a per-process current-session witness (`sessions/proc-<pid>-<start_ms>`, written atomically under `Sessions.Lock` at `SessionStart` before registration). Succession proceeds only if that file exists, is readable, and names `session_id`; any missing, malformed, locked, or mismatched witness fails closed.
     - Single-session clients that do not yet record a per-process current-session witness at `SessionStart` remain ineligible for post-window succession until they do.
- **Server-side supersession checks inside the `BEGIN IMMEDIATE` transaction:**
  1. Fetch the single highest-generation Execution `prev` for `(host_id, client, session_id)` (`ORDER BY generation DESC LIMIT 1`).
     - Refuse with `lost` if none exists, `prev.Generation != r.PriorGeneration`, `r.PriorExecutionID != "" && prev.ID != r.PriorExecutionID`, `prev.HookSeenMS == 0`, or `prev.State != "lost"` (`"active"` and `"ended"` are never succeeded).
  2. If `r.ClientPID != 0 || r.ClientPIDStartMS != 0 || prev.Single`:
     - Require `r.ClientPID > 0 && r.ClientPIDStartMS > 0` and `prev.PID == r.ClientPID && prev.PIDStart == r.ClientPIDStartMS` (with `prev.PID > 0 && prev.PIDStart > 0` required whenever `prev.Single` holds).
  3. If `prev.Single || (prev.Parent == NULL && prev.Client == "claude")` and `prev.PID > 0 && prev.PIDStart > 0`:
     - Refuse with `lost` if **any** hook-registered Execution (`hook_seen_ms > 0`) exists for `(host_id, client, prev.PID, prev.PIDStart)` with `session_id <> prev.Session` and `rowid > prev.rowid` in **any** state (`active`, `lost`, or `ended`).

### 4. Closing the remaining `orRegister` `generation = 0` gaps (Choice A)

Two design choices were evaluated for the remaining `orRegister` `generation = 0` fallback paths:

- **Choice A (RECOMMENDED): Authoritative server guard on `generation = 0` non-hook registrations + client `.ended` inspection.**
  1. **Hub guard:** On `POST /v1/sessions` and `POST /v1/sessions/outside`, when `r.Generation == 0` and `r.Source` is `"mcp"`, `"shim"`, or `"watch"` (any non-hook source):
     - Refuse with `lost` (via `t.registrationLost` so `highest_generation` is returned) if **any** prior hook-registered Execution (`hook_seen_ms > 0`) exists for `(host_id, client, session_id)` in **any** state (`active`, `lost`, or `ended`).
     - In addition, when `r.ClientPID > 0 && r.ClientPIDStartMS > 0` and (`r.SingleSession` or `r.Client == "claude"`), refuse with `lost` if **any** prior hook-registered Execution (`hook_seen_ms > 0`) exists for `(host_id, client, client_pid, client_pid_start_ms)` in **any** state (`active`, `lost`, or `ended`). This blocks the Rung-3 synthetic `"mcp-<pid>-<start_ms>"` fallback once the client process has any hook-registered history.
  2. **Client guard (`.ended` marker):** `hook.Sessions.RecallBindingGeneration` (`internal/hook/sessions.go`) checks `<hash>.ended` alongside `<hash>`. When `<hash>.ended` is damaged/unreadable or holds a positive generation `ended >= 1` while `<hash>` is absent (or `ended >= current`), binding and `fallbackGenerationError` fail closed with `errMalformedGeneration` / `errGenerationFloor` rather than falling through to `orRegister` with `generation = 0`.
  3. **Preserved D6 untrusted-folder fallback:** A fresh `iris mcp` or `iris watch` in a folder whose hooks never reached the hub has zero prior `hook_seen_ms > 0` rows and no `<hash>` or `<hash>.ended` marker, so initial untrusted-folder bootstrap (`TestFirstFallbackChecksGenerationProofAfterGrace` and `adopt()` handover in `internal/binding/binding.go`) continues working unchanged.
- **Choice B (rejected): Route missing-memory `orRegister` through ADR 035 succession.**
  - Callers lacking `<hash>` proof cannot satisfy restore monotonicity (§5) or distinguish a `SessionEnd`-cleared session unless `.ended` is checked and refused. Keeping missing-memory callers rejected under Choice A guarantees only verified `prior_generation` holders can succeed a hook-registered Execution.

**Limits of Choice A:**
- If local `<hash>` proof is deleted or pruned (after 30 days) on a host whose hook-registered session later expires past 120 s, neither ADR 035 succession nor `orRegister` recovers automatically; one registering hook must re-establish authority.
- If ADR 006 retention purges every unreferenced Execution of a long-dead session (>30 days old with no mail/claims/children), the hub retains no historical `hook_seen_ms > 0` row for that session; 30-day retention exceeds any live client process lifetime.
- Against an older hub deployed without Choice A, checking `<hash>.ended` locally closes the post-`SessionEnd` `orRegister` path on upgraded clients, while the Rung-3 synthetic fallback against an older hub still requires a hub upgrade for full enforcement.

### 5. Restore monotonicity and fail-closed rules (ADR 006 decision 7 and floor addendum)

Succession never weakens ADR 006 backup-restore safety:

- **Exact highest-generation match required.** `prev` (`ORDER BY generation DESC LIMIT 1`) must satisfy `prev.Generation == r.PriorGeneration`. If local proof is `G+2` while the restored hub holds only `G` (or no row), the hub refuses with `409 lost` including `highest_generation` (`t.registrationLost`) and creates no row.
- **No silent erasure or `generation = 0` / `retry_generation` retry by `iris mcp`.** On any refusal (`lost`, `conflict`, `olderHub`, timeout, network error), `iris mcp` never deletes or downgrades `<hash>` or `<hash>.ended` and never retries with `generation = 0` or `retry_generation`. Only registering hooks retain ADR 006's bounded `retry_generation` floor check (`internal/hook/runner.go`).
- **Recovery window precedence.** Inside the 120 s recovery window (`prev.State == "active" && prev.Recovery.Valid`), `POST /v1/sessions/succeed` refuses with `lost`; in-window reattachment (`heartbeat` or `POST /v1/sessions` with `generation = G, source = "shim"`) handles matching generations.

### 6. Exact server transaction and resulting Execution (`ex_new`)

Inside one `Store.transact` (`BEGIN IMMEDIATE` under `Store.mu`, after `t.expire()`, without touching `readDB`):

1. Validate `r.Client != ""`, `r.SessionID != ""`, `r.Host != ""`, `r.PriorGeneration > 0`, and `r.Source == "shim"`.
2. Run the `prev` lookup, state/hook checks, Rung 2/3 process-identity checks, and `/clear` supersession checks (§3, §5). On any domain refusal, return via `t.registrationLost` (`409 lost` with `highest_generation`).
3. Verify `prev.AgentID` is not currently held by any `active` Execution (`SELECT COUNT(*) FROM Execution WHERE agent_id=? AND state='active'`); if busy, fail closed with `lost` rather than allocating a `-2` slot.
4. Insert `ex_new`:
   - `generation = prev.Generation + 1`
   - `agent_id = prev.AgentID` (same mailbox), `repository_id = prev.Repo`, `worktree = prev.Worktree`, `branch = prev.Branch`, `parent_id = prev.Parent`, `client_pid = prev.PID`, `client_pid_start_ms = prev.PIDStart`, `single_session = prev.Single`
   - `heartbeat_mode = 1`, `state = 'active'`, `boot_epoch = t.s.epoch`
   - `deadline_tick_ms = tick + 60000`, `lease_renewed_tick_ms = tick` (initializing migration 028's stall-credit baseline)
   - `hook_seen_ms = 0` until a hook reaches the hub for `ex_new` (preserving D8/ADR 017's strict hook-reachability semantics so wake arming waits for a real hook on `ex_new` and a second consecutive shim succession without an intervening hook is blocked)
   - **Zero inherited claims** (`prev`'s claims were already revoked when `prev` expired)
5. Emit `GenerationChange` diagnostic (`ExecutionID: ex_new`, `PreviousExecutionID: prev.ID`, `Generation: prev.Generation + 1`, `PreviousGeneration: prev.Generation`, `Reason: reason` matching `internal/store/generation.go`: `"recovery_expired"`, `"lease_expired"`, or `"prior_lost"`, `Source: "shim"`, `Proof: "shim_succession"`, `RetryGeneration: 0`) and emit `t.event("", "", "changed", "")`.

**One-hop bound by construction:** Because `ex_new` begins with `hook_seen_ms = 0`, at most one automatic succession can occur after a hook-registered Execution expires. If `ex_new` later expires with no intervening hook, a second automatic succession is refused (`prev.HookSeenMS == 0`).

### 7. Concurrency, generation-memory CAS, and hook/shim races

- **Locked compare-and-advance (`AdvanceGeneration`) under `Sessions.Lock`:**
  - `hook.Sessions` adds `AdvanceGeneration(client, session string, expectedOld, newGen int) (current int, updated bool, err error)`:
    - Takes `Sessions.Lock(hookLockBudget)` without calling `directory()` inside the lock (avoiding Windows ACL traversal latency under the lock, `internal/hook/memory.go`).
    - Reads both `<hash>` and `<hash>.ended`.
    - Writes `newGen` only when `<hash>` is readable with `cur == expectedOld && newGen == expectedOld + 1`, and `<hash>.ended` is absent or `< expectedOld`.
    - If `cur > expectedOld` (a hook already advanced `<hash>` while the HTTP call was in flight), leaves `<hash>` untouched and returns `(cur, false, nil)`.
    - If `<hash>` is absent or `<hash>.ended >= expectedOld` (a concurrent `SessionEnd` ran `Forget`), does not recreate `<hash>` and returns `(0, false, nil)`.
- **Concurrent attempts adopt the valid winner:**
  - Because `Store.mu` + `BEGIN IMMEDIATE` serializes every registration and succession, if two shims or a shim and a hook race on `(session_id, G)`, exactly one commits `G+1`.
  - A losing shim receives `409 lost`, performs one read-only `POST /v1/bind` lookup for that session (by `session_id` on Rung 2 / Codex root, or `(client_pid, client_pid_start_ms)` on Rung 3), and adopts the live winner if `winner.Generation >= G+1` and `!r.gone[winner.ExecutionID]` (checking `checkGenerationHeld`).
- **Hook priority and zero D3 deadline impact:**
  - `Sessions.Lock` is held only for local file operations within `hookLockBudget` (50 ms), never across network I/O (`internal/hook/memory.go`). Shims never stall hooks' 1.5 s D3 deadline.
  - If `ex_new` (`G+1`) commits just before a hook reads `<hash>`:
    - If `AdvanceGeneration` wrote `G+1` first, the hook sends `generation = G+1` and attaches directly to `ex_new` (setting `hook_seen_ms > 0`).
    - If the hook had already read `G`, the hub refuses `G` with `409 lost` and `highest_generation = G+1`. The hook's existing restore-floor retry (`internal/hook/runner.go`) checks `G+1 >= G`, retries once with `generation = 0, retry_generation = G`, attaches to active `ex_new` (setting `hook_seen_ms > 0`), and calls `Remember(..., G+1)` without creating `G+2`.
- **MCP guidance and docs update:** Once implemented, update `lostMessage` in `internal/mcpserver/server.go` and `docs/api.md` ("MCP tools answering `lost`") to describe the bounded one-hop succession behavior and when a registering hook is still required.

## Invariants affected

Implementing this ADR amends or extends the following invariants in `docs/architecture.md` (D6, D8, D9, Section 5) and `docs/adr/006-retention-and-restore.md`:

- **Expired and ended generations remain non-revivable (D9, ADR 006 decision 7):** `ex_1` stays `'lost'` and `'ended'` stays terminal. Post-window succession creates a new Execution `ex_2` (`generation = G+1`) only from `'lost'`, never from `'ended'` or `'active'`.
- **Zero inherited claims and no pre-expiry fence carry-over (D9, D10):** `ex_1`'s claims were revoked at expiry; `ex_2` starts with zero claims, and stale fence tokens remain rejected.
- **Strict one-hop bound (`hook_seen_ms = 0`, D8/ADR 017):** `ex_2` starts with `hook_seen_ms = 0`, preserving wake arming semantics and preventing chained shim successions without an intervening hook.
- **Restore floor monotonicity (ADR 006 floor addendum):** Neither succession nor `generation = 0` fallback can succeed when `prev.Generation != prior_generation`, when local `<hash>` or `<hash>.ended` is missing/damaged/ended, or when local proof exceeds the hub's retained `highest_generation`.
- **Generation-change observability and stall-credit baseline (D8, D9, migration 028):** `succeed` emits `GenerationChange` after commit and initializes `lease_renewed_tick_ms = tick` on `ex_new` so bounded stall credit applies from creation while `recovery_deadline` remains immutable.
- **Rung 2 session vs Rung 3 process pinning (D6):** Rung 2 succession remains pinned to `(host_id, client, session_id)` without requiring ancestor PID equality; Rung 3 requires exact `(client_pid, client_pid_start_ms)` match and verified `proc.SingleSession` launcher evidence.
- **Console read isolation and hook deadline safety (D3, D9 item 5):** All succession state transitions run inside `Store.transact` (`BEGIN IMMEDIATE`), leaving `readDB` snapshots unaffected, and `Sessions.Lock` never wraps HTTP calls so D3's 1.5 s hook deadline stays unblocked.

## Alternatives considered

1. **Rely on in-window reattachment, the `readDB` console read pool, and bounded stall credit, keeping post-window recovery hook-only.**
   - *Rejected:* Stall credit deliberately covers only bounded hub-sweep stalls (~120 s) and never moves `recovery_deadline` or credits client/network/laptop-sleep stalls (`docs/architecture.md` D8). Sessions outliving a >120 s hub outage or >60 s sleep remain stuck `lost` until a registering hook fires (and Cursor/Grok prompt hooks do not register), while the two `orRegister` `generation = 0` fallback holes on `main` remain exposed.
2. **Extend `recovery_deadline` when shims/watches call, or revive `'lost'` Executions in place.**
   - *Rejected:* Breaks D9 and ADR 006 invariants that `recovery_deadline` is persisted once per restart, claims expire cleanly after 120 s, and `'lost'` Executions are never revived.
3. **Send `succeed: true` on `POST /v1/sessions` or route missing-memory `orRegister` through succession.**
   - *Rejected:* Unknown JSON fields on `POST /v1/sessions` are ignored by older hubs (`internal/store/sessions.go`), risking unguarded `generation = 0` creation; missing-memory callers lack proof for restore rollback and `/clear` checks.
4. **Disable `orRegister` unconditionally in `iris watch` now.**
   - *Not taken here:* `cmd/iris/watch.go` already installs `RecallBindingGeneration`, Choice A's server guard + `.ended` check blocks `watch` from registering `generation = 0` over any hook-registered or ended session, and `watch` is excluded from succession. Preserving `watch`'s untrusted-folder fallback keeps ADR 034's bootstrap (`TestFirstFallbackChecksGenerationProofAfterGrace` and `TestAMonitorFollowsAdoptionBeforeHandoverIsConfirmed`) working; retiring untrusted-folder fallback for `watch` can be decided separately if desired.

## Limits (what this mechanism cannot do)

- **Cannot revive claims or pre-expiry fences.** Claims of `ex_1` were revoked when `ex_1` expired; `ex_2` starts with zero claims until pre-edit hooks or explicit `claim` calls take new ones.
- **Cannot recover clients lacking positive `(session_id, single-session/process)` evidence across `/clear`.** Multi-chat launchers without per-session env identity (such as Cursor IDE once `singleSession` is restricted to `proc.SingleSession`) and single-session clients without a recorded current-session witness remain ineligible.
- **Cannot recover a hook-registered session whose local `<hash>` proof is missing or damaged (Choice A).** With `generation = 0` non-hook fallbacks blocked over hook-registered history, a fresh shim missing `<hash>` stays `lost` after 120 s until a registering hook fires.
- **Cannot run when the OS parent/client process is dead or after `SessionEnd`.** `ended` and `<hash>.ended` are terminal; dead parents terminate the shim before sending requests.
- **One automatic hop without a hook.** Because `ex_new` has `hook_seen_ms = 0`, a second prolonged outage (>120 s) with zero intervening hooks requires a registering hook.
- **Fails closed after a backup restore where local proof is ahead of the hub.** Only a hook on a user turn can re-establish authority under ADR 006's restore-floor rules.
- **Read-only console snapshots remain one commit behind concurrent succession.** Under D9 item 5 (`readDB` `BEGIN DEFERRED`), the console observes `ex_new` on subsequent WAL snapshots.
- **Not a security boundary across the same OS user (D12).** Session memory, `.ended` markers, and per-process witnesses sit beside `hub.token` with owner-only permissions; any process running as the same OS user can already read the token.

## Verification plan (required before shipping implementation)

Any PR implementing this ADR must achieve 100% Go statement coverage (`make cov-check`), pass `make race`, keep `internal/restorecheck` green, and add deterministic tests for:

1. **Deadline and state boundaries (`store` & `binding`):**
   - Inside 120 s (`t = 119 s`, `prev.State == 'active' && prev.Recovery.Valid`): `POST /v1/sessions/succeed` is refused (`lost`); in-window reattachment handles it instead.
   - Past 120 s (`t = 121 s`) or past 60 s lease / stall-credit cap (`prev.State == 'lost'`): succession commits `ex_new` with `generation = G+1`, `hook_seen_ms = 0`, `lease_renewed_tick_ms = tick`, zero inherited claims, same `Agent`, emits `GenerationChange` (`Source: "shim"`), and requeued unacked deliveries become fetchable and ackable on `ex_new`.
   - Second consecutive outage past 120 s without an intervening hook: refused (`prev.HookSeenMS == 0`).
   - `SessionEnd` (`prev.State == 'ended'`): always refused; `<hash>.ended` on client also fails closed locally.
   - `ex_new` created by succession subsequently receives stall credit across a >5 s hub stall while `sweepArmed == true`, bounded by its initialized `lease_renewed_tick_ms + 60 s + 60 s`.
2. **Choice A (`generation = 0` gaps) and older-hub fail-closed:**
   - Post-`SessionEnd` unbound Rung-2 shim or `iris watch` (where `Forget` removed `<hash>` and left `<hash>.ended`): refused locally (`RecallBindingGeneration` reads `.ended`) and refused on hub (`hook_seen_ms > 0` row exists), leaving `Execution` count unchanged.
   - Unbound Rung-3 shim after `ex_1` expired (`r.last.HookSeen == false`, synthetic `mcp-<pid>-<start_ms>` fallback): refused on hub (`hook_seen_ms > 0` exists for `(host_id, client, pid, start_ms)`), leaving `Execution` count unchanged.
   - Fresh `iris watch` or `iris mcp` in an untrusted folder (zero prior `hook_seen_ms > 0` rows, no `<hash>` or `<hash>.ended`): D6 fallback and `adopt()` handover still succeed.
   - Older hub returning `404 not_found` on `POST /v1/sessions/succeed`: shim fails closed with `lost` and sends zero `generation = 0` fallback requests.
3. **Rung 2 vs Rung 3 identity, Cursor IDE exclusion, parent death, and PID reuse:**
   - Rung 2 (`CLAUDE_CODE_SESSION_ID`, `COPILOT_AGENT_SESSION_ID`, `GROK_SESSION_ID`) succeeds when the shim's ancestor PID differs from `prev.PID` or `prev.PID == 0`, provided `parentAlive()` is true.
   - Rung 3 (`cursor-agent`, `antigravity`, `kimi`, `codex exec`) sends verified `(client_pid, client_pid_start_ms)`; simulated PID reuse (`start_ms` mismatch) or wrong PID is refused on both client and hub.
   - Cursor IDE (`cursor`, `proc.SingleSession == false`) records `single_session = 0` in hooks and is ineligible for Rung-3 succession.
   - Excluded callers (`parentAlive() == false`, Codex sub-agents, `iris watch`, wake helpers, live `IRIS_EXECUTION`, explicit handles, `hook_seen_ms == 0`) are refused.
4. **`/clear` supersession (`active`, `lost`, `ended`):**
   - Single-session process starts `S1`, then `/clear` starts `S2` on the same `(pid, start_ms)`.
   - With `S2` in each of `active`, `lost`, and `ended`: `S1`'s shim is refused locally by the per-process current-session witness (Antigravity `antigravity-helpers`, Kimi `kimi-wake/sessions`, or `sessions/proc-<pid>-<start_ms>`) and independently refused on the hub (`rowid > prev.rowid`).
   - Missing, corrupted, or locked per-process witness fails closed without calling the hub.
5. **Backup restore rollback:**
   - Hub backed up at `G = 1`, host advances to `G = 3`, backup restored and `G = 1` expires to `lost`: shim presenting `prior_generation = 3` is refused with `409 lost` (`highest_generation = 1`), local `<hash>` stays `3`, zero `generation = 0` / `retry_generation` retries are sent, and `internal/restorecheck` passes.
6. **Concurrency, `AdvanceGeneration` CAS, and hook/shim races:**
   - Concurrent hook registration and shim succession on `(S1, G)`: exactly one commits `G+1`; losing shim adopts the live winner via `/v1/bind`; no `-2` agent split occurs; hook 1.5 s D3 deadline is unblocked.
   - Concurrent shim successions: one wins `G+1`, loser adopts winner.
   - Hook fires right after shim commits `ex_new` (`G+1`): both when hook reads `G+1` directly and when hook read `G` and retries with `retry_generation = G` against `highest_generation = G+1`, hook attaches to `ex_new`, sets `hook_seen_ms > 0`, and creates no `G+2`.
   - Concurrent `AdvanceGeneration(1 -> 2)` vs hook `Remember(3)` and vs `Forget` (`.ended` written) under `Sessions.Lock`: never downgrades `3` and never recreates a forgotten `<hash>`.
