# ADR 035: Automatic post-window session succession by `iris mcp`

Status: proposed, 2026-11-23 (revised against `main` after `#408`). Proposes changes to D6, D8, D9, [ADR 006](006-retention-and-restore.md) decision 7 and its amendments, `internal/hook/runner.go` (`singleSession`), `internal/hook/memory.go`, `internal/hook/sessions.go`, `internal/binding/`, `internal/store/`, and `internal/mcpserver/` recovery guidance; **no implementation starts until this ADR is reviewed and the owner decision below is resolved.**

## Context

When `iris serve` starts or restores a database, every `active` Execution without an existing `recovery_deadline` receives a persisted 120 s deadline and enters recovery (`internal/store/store.go`, D9, ADR 006 decision 7). Current `main` handles recovery, restore guards, ended sessions, and transient hub stalls through five mechanisms:

1. **In-window reattachment (ADR 006 amendment of 2026-11-22).** Inside the 120 s window:
   - A bound `iris mcp` whose `/v1/bind` refresh returns `lost` immediately heartbeats with its exact remembered generation (`internal/binding/binding.go`, `internal/binding/call.go`).
   - An unbound `iris mcp` (excluding Codex id-mode sub-agents, `IRIS_EXECUTION` overrides, explicit non-gone handles, and `iris watch`) reads the hook's local generation memory (`internal/hook/sessions.go`) and sends a reattach-only registration (`source: "shim"`, `generation > 0`) for a positively identified, hook-registered session (`internal/store/sessions.go`). Rung 2 (`CLAUDE_CODE_SESSION_ID`, `COPILOT_AGENT_SESSION_ID`, `GROK_SESSION_ID`) pins reattachment to `(host_id, client, session_id)` without requiring `(client_pid, client_pid_start_ms)` to match across desktop launchers or wrappers, while Rung 3 pins to the verified client process.
   - `iris watch` heartbeats immediately on reconnect and `reset` frames (`internal/watch/watch.go`), and retains `RecallBindingGeneration` (`cmd/iris/watch.go`) to refuse lower or malformed generations.
2. **Restore generation floor and `.ended` markers (ADR 006 addendum of 2026-11-22).** Registration `409 lost` responses include `error.details.highest_generation` (`internal/store/generation.go`). Hooks retry once with `generation: 0, retry_generation: G` only when `highest_generation >= G` (`internal/hook/runner.go`). `SessionEnd` records `<hash>.ended` under `Sessions.Lock` before removing `<hash>`, keeping `<hash>` if writing `.ended` fails or misses the lock (`internal/hook/memory.go`). `iris mcp` and `iris watch` reject bindings or fallbacks below local proof or with unreadable/malformed `<hash>` files (`internal/binding/binding.go`).
3. **Closed proofless `ENDED` succession (`#408`, ADR 006 addendum of 2026-11-22).** `SessionEnd` leaves `<hash>.ended` and removes `<hash>`. `RecallBindingGeneration` (`internal/hook/sessions.go`) treats a readable or damaged `.ended` marker as present proof when `<hash>` is absent, blocking `orRegister` `generation = 0` fallback (and recording `damaged generation proof at <path>` when damaged). On the hub (`internal/store/sessions.go`), registration requires `retry_generation >= 0` and `source` in `{"", "hook", "mcp", "shim", "watch"}` (anything else is `bad_request`), and refuses any `generation = 0` non-hook registration (`"mcp"`, `"shim"`, `"watch"`, or legacy `""`) with `409 lost` and `highest_generation` whenever the session's newest retained Execution is `ended`, regardless of `retry_generation`. A registering `hook` still advances `ended` to `G+1`.
4. **Bounded hub stall credit (migration 028, D8).** While the hub's stream expiry sweep is armed, a monotonic gap above 5 s moves non-recovering active deadlines forward up to `min(now + 20 s, lease_renewed_tick_ms + lease + 60 s)` (`internal/store/store.go`). Stall credit explicitly never moves `recovery_deadline`, never applies when no stream is armed, never credits client, network, or machine sleep stalls while the hub runs, never covers stalls past ~120 s since the last heartbeat, and never revives a `lost` row.
5. **Console read pool (D9 item 5).** Read-only console queries (`ui_overview`, `ui_deliveries`, `ui_threads`, `ui_thread`, `ui_doctor_get`, `ui_events`, `ui_receipts`, and read-only console session checks) run in `BEGIN DEFERRED` on a separate `PRAGMA query_only=ON` pool (`readDB`) without acquiring `Store.mu` or running `expire`, stopping slow console snapshots from delaying heartbeat renewals while the hub is live (`internal/store/store.go`, `internal/store/api.go`).

After `#408` closed the proofless `ENDED` path, two asymmetry and safety issues remain open for `LOST` sessions on `main`:

1. **Proof-holding `iris mcp` shims are locked in `lost` after >120 s outages or >60 s client/network stalls.** Once `t.expire()` (`internal/store/store.go`) marks Execution `ex_1` (generation `G`) `state = 'lost'`, revokes its claims, and requeues its deliveries:
   - `POST /v1/executions/{id}/heartbeat` fails `t.live()` (`internal/store/sessions.go`) with `409 lost`.
   - `reattachByMemoryHeld` sends `POST /v1/sessions` with `generation = G > 0, source = "shim"`, which `internal/store/sessions.go` unconditionally refuses with `409 lost` (`"expired generations cannot be revived"`).
   - `orRegister` refuses fallback whenever `err` is `lost`, `len(r.gone) > 0`, or `r.last.HookSeen` is true (`internal/binding/binding.go`).
   - Every tool call returns `lost` (`internal/mcpserver/server.go`) and every 20 s `Maintain` tick repeats the same failing reattach loop until a registering hook fires. On clients whose prompts do not register (Cursor and Grok, `docs/api.md`), an idle or post-outage session with valid `<hash>` proof cannot recover on a prompt alone unless `SessionStart` or an edit hook runs.
2. **Proofless `generation = 0` `LOST -> G+1` (and Rung-3 synthetic fallback) still runs without restore or `/clear` guards.** `#408` intentionally left `LOST` succession unchanged (`docs/adr/006-retention-and-restore.md`), creating an inverted rule where proof-holding shims are blocked while proofless callers can advance `LOST`:
   - **Proofless Rung 2 / `iris watch` over a `lost` Execution:** When `<hash>` and `<hash>.ended` are both absent on disk (and `r.last.HookSeen == false`, `len(r.gone) == 0`), an unbound Rung-2 shim or `iris watch` falls into `orRegister` (`source: "mcp", generation: 0`, `internal/binding/binding.go`), and `internal/store/sessions.go` creates `G+1` on the same `Agent` mailbox with no `prior_generation`, `prior_execution_id`, or `/clear` checks.
   - **Unbound Rung-3 shim after a `lost` (or `ended`) hook Execution:** If a Rung-3 shim (`cursor-agent`, `antigravity`, `kimi`, `codex exec`) never bound `ex_1` (`r.last.HookSeen == false`, `r.envSession() == ""`), `recallSessionHeld` returns `("", 0)`. `orRegister` checks `fallbackGenerationError` against the synthetic ID `"mcp-<pid>-<start_ms>"` (`internal/binding/binding.go`) rather than the hook's session ID and creates a stray active `"mcp-<pid>-<start_ms>"` Execution on the same process.

Naive post-window succession is unsafe for four reasons:

1. **Supersession across `/clear` and `SessionEnd`.** While `#408` blocks proofless `ENDED -> G+1`, an orphaned shim or a shim belonging to session `S1` superseded by `/clear` (`S2` started on the same process, and `S2` later became `lost` or `ended`) could still resurrect `S1` if `S1`'s Execution was left `lost`.
2. **Restore lineage and equal-generation ambiguity (ADR 006 decision 7 and floor addendum).** A backup copy may lag behind the host (`G` on hub vs `G+2` on host), run with `RestoreMarker` / `iris admin mark-restored` on a rotated `stream_id`, or contain a different Execution at the same generation `G` from a diverged history. Allowing `generation = 0` or matching generation counters alone risks cross-history or rolled-back succession.
3. **Hook-policy bypass and imprecise `single_session` flags.** Allowing unverified `generation = 0` MCP/`watch` registrations over `lost` hook history—or trusting `single_session = 1` from multi-chat launchers such as Cursor IDE (`internal/hook/runner.go` currently sets `singleSession(ClientCursor) = true` even when `proc.SingleSession` in `internal/proc/proc.go` is `false` for `cursor`)—breaks D6's rule that hooks are the session's registrar.
4. **Generation-memory and concurrency races.** Unsynchronized writes from a late MCP process could downgrade a newer hook generation `G+2` or recreate `<hash>` files deleted by `SessionEnd`, and concurrent shims or hooks could split Executions or stall a hook's 1.5 s D3 deadline.

## Owner decision required before implementation

Unlike `#408` (which closed `ENDED` succession cleanly without removing normal recovery), **ADR 035 confronts a policy choice on `LOST` sessions** between preserving today's proofless fallback for hook-less/proofless callers versus enforcing strict proof-only succession everywhere:

| Aspect | Choice A (Recommended): Withdraw proofless `LOST -> G+1` for `mcp`/`shim`/`watch`; allow proof-required `POST /v1/sessions/succeed` for `iris mcp` | Alternative Policy (Keep proofless `LOST -> G+1` fallback alongside `#408`'s `ENDED` guard) |
|---|---|---|
| **Proof-holding `iris mcp` after >120 s outage or sleep** | **Improves availability:** gains automatic one-hop `LOST -> G+1` succession on `POST /v1/sessions/succeed` using exact `(prior_generation, prior_execution_id)` and `/clear` checks (fixing idle Cursor/Grok/Claude shims that currently stay `lost`). | Either stays stuck `lost` until a hook runs (status quo on `main`), or uses `POST /v1/sessions/succeed` only when local proof exists. |
| **Proofless Rung 2 / `watch` over a `lost` hook-registered Execution** | **Withdrawn:** refused (`409 lost` with `highest_generation`); session stays `lost` until a registering hook (`SessionStart`, `PreTool`, or registering prompt) runs. `iris watch` never creates `G+1` over hook history. | **Kept:** fresh `iris mcp` or `iris watch` missing `<hash>`/`<hash>.ended` continues creating `G+1` via `orRegister` (`generation = 0`). |
| **Unbound Rung 3 (`cursor-agent`, `antigravity`, `kimi`, `codex exec`) after `ex_1` expired (`lost`) or ended (`ended`)** | **Withdrawn over hook process history:** refused (`409 lost`) when hooks for `(host_id, client, pid, start_ms)` already reached the hub (`hook_seen_ms > 0`), preventing stray `"mcp-<pid>-<start_ms>"` splits. | **Kept:** unbound Rung-3 shim falls back after 15 s grace to synthetic `"mcp-<pid>-<start_ms>"` (`G = 1`). |
| **Truly hook-less / untrusted folder (`hook_seen_ms == 0` everywhere)** | **Unchanged:** D6 untrusted-folder fallback (`orRegister` + `adopt()`) continues working for sessions whose hooks never reached the hub. | **Unchanged.** |
| **User-visible effect on Cursor and Grok users** | Cursor and Grok prompts do not register (`docs/api.md`). If a Cursor or Grok session had a hook-registered Execution that expired to `lost` **and** its local proof files (`<hash>` / `<hash>.execution`) are absent/unreadable (or a fresh `cursor-agent` MCP shim starts after expiry without `<hash>.execution`), sending prompts will **not** recover it automatically—the user must trigger an edit (`PreTool`) or `SessionStart` / resume. By contrast, normal Cursor and Grok sessions *with* intact proof gain automatic one-hop `iris mcp` succession on tool calls or 20 s ticks. | A Cursor or Grok session without local proof (or an unbound `cursor-agent` MCP child) still gets a `generation = 0` fallback over `lost` (or synthetic `mcp-<pid>-<start_ms>`), at the cost of bypassing restore-floor lineage and `/clear` guards whenever proof is omitted. |
| **Security / restore integrity** | Closes the self-bypass where omitting proof (`generation = 0`) skips restore monotonicity (`highest_generation`, `prior_execution_id`) and `/clear` checks. | Any proofless caller (`generation = 0`) bypasses restore monotonicity and `/clear` guards over `lost` sessions; `ended` remains blocked by `#408`. |

**Recommendation:** Adopt **Choice A** (withdraw proofless `LOST -> G+1` for `mcp`/`shim`/`watch` over hook-registered history, and enable guarded one-hop `POST /v1/sessions/succeed` for `iris mcp` with required proof). Keeping proofless `LOST -> G+1` undermines the ADR's restore and `/clear` invariants whenever local proof is absent or an unupgraded caller uses `generation = 0`, while proof-holding Cursor and Grok sessions *gain* post-window recovery they lack today. **Implementation must not proceed until the owner explicitly confirms this behavior trade-off.**

## Decision

Introduce an explicit, one-hop **post-window succession** protocol on a dedicated route (`POST /v1/sessions/succeed`) requiring exact `(prior_generation, prior_execution_id)`, paired with **Choice A: server-side refusal of `generation = 0` non-hook fallbacks over any prior hook-registered session or single-session process** (keeping `#408`'s `ended` guard intact) and client-side per-process `/clear` checks under `Sessions.Lock`.

An `iris mcp` process may create a fresh Execution `ex_2` (`generation = G+1`, zero inherited claims) for its own expired (`state = 'lost'`) hook-registered session **only** when every client precondition, server guard, `/clear` supersession check, restore guard, and concurrency rule below holds. Any missing, stale, ambiguous, or unreadable check **fails closed** with `lost` and never falls back to `generation = 0`.

### 1. Dedicated route (`POST /v1/sessions/succeed`)

Post-window succession uses a dedicated HTTP endpoint `POST /v1/sessions/succeed` (store operation `"succeed"`) rather than an optional flag on `POST /v1/sessions`.

- **Fail-closed upgrade safety:** `internal/store/sessions.go` ignores unknown JSON fields on `POST /v1/sessions`. Adding a succession flag alongside `generation = 0` to `POST /v1/sessions` would let an older hub silently ignore the flag and execute an unguarded `generation = 0` registration. A dedicated route returns `404 not_found` (`hubclient.OlderHub`) on older hubs, so upgraded shims fail closed with `lost`.
- **Fixed context preservation:** `POST /v1/sessions/succeed` takes `{client, host, session_id, prior_generation, prior_execution_id, client_pid, client_pid_start_ms}` (with `prior_generation > 0` and non-empty `prior_execution_id` both **required**) and omits working-directory / git-discovery fields. Succession preserves the prior Execution's repository (or `outside:<host>` context) rather than re-running repository resolution, allocating repositories, or mutating aliases.

### 2. Attribution and protection against accidental races (not authority against a token holder)

`source`, `session_id`, `prior_generation`, `prior_execution_id`, and `client_pid` are self-declared inputs from a client holding the host token. Consistent with D13 (`docs/architecture.md`) and `#408`'s `ended` guard (`docs/adr/006-retention-and-restore.md`), **these checks provide attribution and protection against accidental races or stale unmodified clients, not authority against a malicious host-token holder** (who can already send `source: "hook"` and register sessions for that host).

Within that D13 boundary, succession is permitted **only** when the shim presents evidence matching what D6 expects for that session:

- **Eligible clients and bindings:**
  - **Rung 2 (`claude`, `copilot`, `grok` via client-owned session environment variable) and Codex id-mode root (`_meta.sessionId == _meta.threadId`):** Identified by `(host, client, session_id)` plus local `(prior_generation, prior_execution_id)`. Consistent with Rung 2 in-window reattachment (`internal/binding/binding.go`, `internal/store/sessions.go`), Rung 2 and Codex id-mode root omit `(client_pid, client_pid_start_ms)` unless the shim's retained `r.last` originally bound through Rung 3 (`r.last.Rung == 3`).
  - **Rung 3 (`cursor-agent`, `antigravity`/`agy`, `kimi`/`kimi-code`, `codex exec`, and top-level `claude` bound via Rung 3 such as `--continue`):** Requires positive `proc.SingleSession(anc, client)` (or `r.last.Rung == 3` with a `claude` top-level session). The shim re-verifies its nearest client ancestor `(client_pid, client_pid_start_ms)` via `proc.Nearest` before and after reading local state (preventing PID reuse) and sends `(client_pid, client_pid_start_ms)` on the request.
  - **Fix `singleSession` over-classification before enabling Rung-3 succession:** Update `internal/hook/runner.go` so `SingleSession` is `true` only when `proc.SingleSession(anc, string(ev.Client))` holds (excluding Cursor IDE `cursor`, which can host multiple chats in one process and matches `internal/proc/proc.go`). Cursor IDE (`single_session = 0`) and multiplexed launchers without per-session env identity remain ineligible for Rung-3 post-window succession.
- **Explicitly excluded:** Codex id-mode sub-agents and foreign-session lookups (ADR 017), `iris watch` (`HeartbeatSource == "watch"`), Antigravity/Kimi wake helpers (`r.wakeSession != ""`), live `IRIS_EXECUTION` overrides, explicit handle arguments, and any Execution whose `hook_seen_ms == 0`.
- **Live OS parent check:** Before attempting succession, `iris mcp` verifies `parentAlive()` (`internal/binding/binding.go`).

### 3. `SessionEnd` and `/clear` supersession guards

Checking only "no other `active` Execution exists" is insufficient: after `/clear`, a newer session `S2` on the same single-session process may itself have expired (`lost`) or ended (`ended`), leaving no `active` Execution while `S1` remains superseded. ADR 035 enforces monotonicity on both sides:

- **Persisted execution identity (`<hash>.execution`) under `Sessions.Lock`:**
  - To ensure even a freshly launched or previously unbound `iris mcp` has `prior_execution_id`, hooks store the current `execution_id` in `<hash>.execution` alongside `<hash>` (`G`) under `Sessions.Lock` (`Remember` writes `<hash>.execution` atomically whenever `generation >= current`; `Forget` deletes `<hash>.execution` alongside `<hash>` only when `current == generation`, keeping both if `.ended` fails; `RecallGeneration` refreshes both mtimes). `<hash>` remains a single integer so `readGeneration` stays compatible with older binaries.
  - A bound shim also retains `r.last.ExecutionID` (`internal/binding/binding.go`); if `r.last.ExecutionID != ""`, it must match `<hash>.execution`.
- **Client-side local precheck (under lock, bounded by `hookLockBudget`, never held across HTTP I/O):**
  1. Read `<hash>`, `<hash>.execution`, and `<hash>.ended` (`internal/hook/memory.go`) under `Sessions.Lock`:
     - `<hash>` must exist, parse as a positive integer, and equal `prior_generation` (and `>= r.processGenerationHeld(session)`).
     - `<hash>.execution` must exist, be readable as a non-empty `ex_...` ID, and match `r.last.ExecutionID` if `r.last.ExecutionID != ""`. Any missing, unreadable, or malformed `<hash>.execution` fails closed (recording `damaged generation proof at <path>` when unreadable/malformed, including dangling symlinks).
     - `<hash>.ended` must be absent or parse strictly below `prior_generation`. If `<hash>.ended` is unreadable or malformed (including a dangling symlink) or holds `ended >= prior_generation`, fail closed (and record `damaged generation proof at <path>` when damaged).
  2. For single-session processes (`proc.SingleSession`), verify local per-process current-session state:
     - **Antigravity:** `antigravity-helpers/<pid>-<start_ms>.json` (`internal/hook/antigravity_wake.go`) must be readable under its lock, list `current_session_id == session_id`, and not list `session_id` in `superseded_session_ids`.
     - **Kimi:** `kimi-wake/sessions/<pid>-<start_ms>.json` (`internal/kimiwake/kimiwake.go`) must be readable under its lock and map `(pid, start_ms)` to `session_id`.
     - **Other `proc.SingleSession` launchers (`cursor-agent`, `codex exec`):** Hooks persist a per-process current-session witness (`sessions/proc-<pid>-<start_ms>`, written atomically under `Sessions.Lock` at `SessionStart` before registration). Succession proceeds only if that file exists, is readable, and names `session_id`; any missing, malformed, locked, or mismatched witness fails closed.
     - Single-session clients that do not yet record a per-process current-session witness at `SessionStart` remain ineligible for post-window succession until they do.
- **Server-side supersession checks inside the `BEGIN IMMEDIATE` transaction:**
  1. Require `r.PriorGeneration > 0` and `r.PriorExecutionID != ""` (returning `bad_request` if missing/non-positive).
  2. Fetch the single highest-generation Execution `prev` for `(host_id, client, session_id)` (`ORDER BY generation DESC LIMIT 1`).
     - Refuse with `lost` (`t.registrationLost` with `highest_generation`) if none exists, `prev.Generation != r.PriorGeneration`, `prev.ID != r.PriorExecutionID`, `prev.HookSeenMS == 0`, or `prev.State != "lost"` (`"active"` and `"ended"` are never succeeded).
  3. If `r.ClientPID != 0 || r.ClientPIDStartMS != 0 || prev.Single`:
     - Require `r.ClientPID > 0 && r.ClientPIDStartMS > 0` and `prev.PID == r.ClientPID && prev.PIDStart == r.ClientPIDStartMS` (with `prev.PID > 0 && prev.PIDStart > 0` required whenever `prev.Single` holds).
  4. If `prev.Single || (prev.Parent == NULL && prev.Client == "claude")` and `prev.PID > 0 && prev.PIDStart > 0`:
     - Refuse with `lost` if **any** hook-registered Execution (`hook_seen_ms > 0`) exists for `(host_id, client, prev.PID, prev.PIDStart)` with `session_id <> prev.Session` and `rowid > prev.rowid` in **any** state (`active`, `lost`, or `ended`).

### 4. Closing the remaining `orRegister` `generation = 0` gaps over `LOST` (Choice A)

`#408` already closed `generation = 0` non-hook registrations over `ended` Executions (`previous.State == "ended" && r.Source != "hook"` on the hub, plus `.ended` detection in `RecallBindingGeneration`). Under Choice A, ADR 035 extends the server-side refusal symmetrically to `lost` and `active` hook-registered history and Rung-3 process history:

1. **Hub guard:** On `POST /v1/sessions` and `POST /v1/sessions/outside`, when `r.Generation == 0` and `r.Source` is `"mcp"`, `"shim"`, `"watch"`, or `""` (any non-hook source):
   - Refuse with `409 lost` (via `t.registrationLost` so `highest_generation` is returned) if **any** prior hook-registered Execution (`hook_seen_ms > 0`) exists for `(host_id, client, session_id)` in **any** state (`active`, `lost`, or `ended`) — extending `#408`'s `ended` guard to `lost` and `active`.
   - In addition, when `r.ClientPID > 0 && r.ClientPIDStartMS > 0` and (`r.SingleSession` or `r.Client == "claude"`), refuse with `409 lost` if **any** prior hook-registered Execution (`hook_seen_ms > 0`) exists for `(host_id, client, client_pid, client_pid_start_ms)` in **any** state (`active`, `lost`, or `ended`). This blocks the Rung-3 synthetic `"mcp-<pid>-<start_ms>"` fallback once the client process has any hook-registered history (after either `lost` or `ended`).
2. **Client guard:** `RecallBindingGeneration` (`internal/hook/sessions.go`) already blocks `orRegister` when `<hash>` or `<hash>.ended` is present or damaged (`#408`); if `<hash>.execution` is present and damaged/unreadable, `RecallBindingGeneration` likewise treats it as damaged proof so `orRegister` never falls through to `generation = 0`.
3. **Preserved D6 untrusted-folder fallback:** A fresh `iris mcp` or `iris watch` in a folder whose hooks never reached the hub has zero prior `hook_seen_ms > 0` rows and no `<hash>`, `<hash>.execution`, or `<hash>.ended` marker, so initial untrusted-folder bootstrap (`TestFirstFallbackChecksGenerationProofAfterGrace` and `adopt()` handover in `internal/binding/binding.go`) continues working unchanged.

**Limits of Choice A:**
- If local `<hash>` or `<hash>.execution` proof is deleted, damaged, or pruned after 30 days on a host whose hook-registered session later expires past 120 s, neither ADR 035 succession nor `orRegister` recovers automatically; one registering hook must re-establish authority.
- If ADR 006 retention purges every unreferenced Execution of an expired or ended session (after 30 days without mail, claims, or child Executions), the hub retains no historical `hook_seen_ms > 0` or `ended` row for that session, so neither `#408`'s `ended` guard nor Choice A's `generation = 0` refusal has retained state left to check.
- Against an older hub deployed without Choice A, local proof / `.ended` / `.execution` checks block Rung-2/`watch` fallbacks on upgraded clients, while the Rung-3 synthetic fallback over prior hook history requires a hub upgrade for enforcement.

### 5. Restore lineage, mark-restored databases, stream rotation, and fail-closed bounds

Succession preserves ADR 006 backup-restore safety across four restore cases:

1. **Local proof ahead of restored hub (counter rollback):** If local `<hash>` is `G+2` while the restored hub holds `G` (or no row), `prev.Generation != r.PriorGeneration` fails with `409 lost` and `highest_generation` (`t.registrationLost`), creating no row.
2. **Equal-generation, different-history restore (`prior_execution_id` required):** Per ADR 006 (`docs/adr/006-retention-and-restore.md`), equal generation counters carry no restore provenance. Suppose backup `B` holds `ex_1` (`G=1`), the live host moves to `ex_2_live` (`G=2`), and a restored copy of `B` later creates a distinct `ex_2_restored` (`G=2`) that expires to `lost`. Requiring non-empty `r.PriorExecutionID` and enforcing `prev.ID == r.PriorExecutionID` catches this diverged history (`ex_2_live != ex_2_restored`) and refuses with `409 lost`.
3. **Mark-restored databases and stream rotation (`RestoreMarker`, `Meta.stream_id`):**
   - `iris admin backup` and `iris admin mark-restored` (`internal/store/restore.go`) write `RestoreMarker`, which `Open()` consumes on its first startup transaction (`internal/store/store.go`), setting `s.restored = true` and rotating `Meta.stream_id`.
   - **Stream rotation gives no generation lineage and no succession authority.** Rotating `stream_id` only resets event cursors and attention identity (ADR 006 amendment of 2026-11-13); an unmarked restore or a second restart does not rotate `stream_id`, and `RestoreMarker` is deleted at startup. `POST /v1/sessions/succeed` never consults `s.Restored()` or `Meta.stream_id` to waive `prev.Generation == r.PriorGeneration`, `prev.ID == r.PriorExecutionID`, or `prev.State == 'lost'`.
   - On a `mark-restored` hub whose `ex_1` expired to `'lost'`, if local proof is ahead or names a different Execution ID, succession is refused with `409 lost`. Only if local proof names the exact same `(ex_1, G)` (meaning neither `G` nor `Execution.ID` changed on that host after the backup was taken) does succession advance `ex_1` to `G+1` with zero claims and `hook_seen_ms = 0`.
4. **Bounded residual ambiguity & no client erasure/retry:**
   - **Explicit bound:** If a host created `ex_1` (`G=1`) before a backup and never replaced `ex_1` before restoring that exact backup (with or without `RestoreMarker` / stream rotation), both the live history and the restored backup share `(ex_1, G=1)`, so neither `prior_execution_id` nor `stream_id` can distinguish them once `ex_1` expires past 120 s (the same counter-based provenance limit noted in ADR 006).
   - **No silent erasure or `generation = 0` / `retry_generation` retry by `iris mcp`:** On any refusal (`lost`, `conflict`, `olderHub`, timeout, network error), `iris mcp` never deletes or downgrades `<hash>`, `<hash>.execution`, or `<hash>.ended` and never retries with `generation = 0` or `retry_generation`. Only registering hooks retain ADR 006's bounded `retry_generation` floor check (`internal/hook/runner.go`).
   - **Recovery window precedence:** Inside the 120 s recovery window (`prev.State == "active" && prev.Recovery.Valid`), `POST /v1/sessions/succeed` refuses with `lost`; in-window reattachment handles matching generations.

### 6. Exact server transaction and resulting Execution (`ex_new`)

Inside one `Store.transact` (`BEGIN IMMEDIATE` under `Store.mu`, after `t.expire()`, without touching `readDB`):

1. Validate `r.Client != ""`, `r.SessionID != ""`, `r.Host != ""`, `r.Source == "shim"`, `r.PriorGeneration > 0`, and `r.PriorExecutionID != ""`.
2. Run the `prev` lookup, state/hook checks, Rung 2/3 process-identity checks, `/clear` supersession checks, and `prev.ID == r.PriorExecutionID` check (§3, §5). On any domain refusal, return via `t.registrationLost` (`409 lost` with `highest_generation`), leaving `Agent`, `Execution`, `Delivery`, and `Claim` rows completely unchanged.
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
  - `hook.Sessions` adds `AdvanceGeneration(client, session, expectedExec string, expectedOld, newGen int, newExec string) (current int, updated bool, err error)`:
    - Takes `Sessions.Lock(hookLockBudget)` without calling `directory()` inside the lock (avoiding Windows ACL traversal latency under the lock, `internal/hook/memory.go`).
    - Reads `<hash>`, `<hash>.execution`, and `<hash>.ended`.
    - Writes `newGen` to `<hash>` and `newExec` to `<hash>.execution` only when `<hash>` is readable with `cur == expectedOld`, `<hash>.execution` matches `expectedExec`, `newGen == expectedOld + 1`, `newExec != ""`, and `<hash>.ended` is absent or `< expectedOld`.
    - If `cur > expectedOld` (a hook already advanced `<hash>` while the HTTP call was in flight), leaves `<hash>` and `<hash>.execution` untouched and returns `(cur, false, nil)`.
    - If `<hash>` or `<hash>.execution` is absent or `<hash>.ended >= expectedOld` (a concurrent `SessionEnd` ran `Forget`), does not recreate them and returns `(0, false, nil)`.
- **Concurrent attempts adopt the valid winner:**
  - Because `Store.mu` + `BEGIN IMMEDIATE` serializes every registration and succession, if two shims or a shim and a hook race on `(session_id, G)`, exactly one commits `G+1`.
  - A losing shim receives `409 lost`, performs one read-only `POST /v1/bind` lookup for that session (by `session_id` on Rung 2 / Codex root, or `(client_pid, client_pid_start_ms)` on Rung 3), and adopts the live winner if `winner.Generation >= G+1` and `!r.gone[winner.ExecutionID]` (checking `checkGenerationHeld`).
- **Hook priority and zero D3 deadline impact:**
  - `Sessions.Lock` is held only for local file operations within `hookLockBudget` (50 ms), never across network I/O (`internal/hook/memory.go`). Shims never stall hooks' 1.5 s D3 deadline.
  - If `ex_new` (`G+1`) commits just before a hook reads `<hash>`:
    - If `AdvanceGeneration` wrote `(G+1, ex_new)` first, the hook sends `generation = G+1` and attaches directly to `ex_new` (setting `hook_seen_ms > 0`).
    - If the hook had already read `G`, the hub refuses `G` with `409 lost` and `highest_generation = G+1`. The hook's existing restore-floor retry (`internal/hook/runner.go`) checks `G+1 >= G`, retries once with `generation = 0, retry_generation = G`, attaches to active `ex_new` (setting `hook_seen_ms > 0`), and calls `Remember(..., G+1, ex_new)` without creating `G+2`.
- **MCP guidance and docs update:** Once implemented, update `lostMessage` in `internal/mcpserver/server.go` and `docs/api.md` ("MCP tools answering `lost`") to describe the bounded one-hop succession behavior and when a registering hook is still required.

## Invariants affected

Implementing this ADR amends or extends the following invariants in `docs/architecture.md` (D6, D8, D9, Section 5) and `docs/adr/006-retention-and-restore.md`:

- **Expired and ended generations remain non-revivable (D9, ADR 006 decision 7, `#408`):** `ex_1` stays `'lost'` and `'ended'` stays terminal. `#408`'s `ended` guard stays intact for all non-hook registrations; post-window succession creates `ex_2` (`generation = G+1`) only from `'lost'`, never from `'ended'` or `'active'`.
- **Zero inherited claims and no pre-expiry fence carry-over (D9, D10):** `ex_1`'s claims were revoked at expiry; `ex_2` starts with zero claims, and stale fence tokens remain rejected.
- **Strict one-hop bound (`hook_seen_ms = 0`, D8/ADR 017):** `ex_2` starts with `hook_seen_ms = 0`, preserving wake arming semantics and preventing chained shim successions without an intervening hook.
- **Restore lineage and refusal non-mutation (ADR 006 floor addendum):** Neither succession nor `generation = 0` fallback can succeed when `prev.Generation != prior_generation`, `prev.ID != prior_execution_id`, when `<hash>`, `<hash>.execution`, or `<hash>.ended` is missing/damaged/ended, or when local proof exceeds the hub's retained `highest_generation`; stream rotation grants no lineage or succession authority; any refusal leaves `Agent`, `Execution`, `Delivery`, and `Claim` state unchanged.
- **Generation-change observability and stall-credit baseline (D8, D9, migration 028):** `succeed` emits `GenerationChange` after commit and initializes `lease_renewed_tick_ms = tick` on `ex_new` so bounded stall credit applies from creation while `recovery_deadline` remains immutable.
- **Rung 2 session vs Rung 3 process pinning (D6):** Rung 2 succession remains pinned to `(host_id, client, session_id)` without requiring ancestor PID equality; Rung 3 requires exact `(client_pid, client_pid_start_ms)` match and verified `proc.SingleSession` launcher evidence.
- **Console read isolation and hook deadline safety (D3, D9 item 5):** All succession state transitions run inside `Store.transact` (`BEGIN IMMEDIATE`), leaving `readDB` snapshots unaffected, and `Sessions.Lock` never wraps HTTP calls so D3's 1.5 s hook deadline stays unblocked.

## Alternatives considered

1. **Keep today's `main` (`#408`) recovery model (proofless `generation = 0` `mcp` and `watch` `LOST -> G+1` + hook registration for proof-bearing sessions, with `ENDED` refused).**
   - *Rejected (subject to Owner decision above):* Leaves proof-holding shims (including Cursor and Grok sessions with intact `<hash>`) locked in `lost` after >120 s outages or sleep, while permitting proofless `mcp` and `watch` callers (missing `<hash>`/`<hash>.ended`, or Rung-3 `mcp-<pid>-<start_ms>`) to bypass restore-floor lineage and `/clear` checks after `lost`.
2. **Extend `recovery_deadline` when shims/watches call, or revive `'lost'` Executions in place.**
   - *Rejected:* Breaks D9 and ADR 006 invariants that `recovery_deadline` is persisted once per restart, claims expire cleanly after 120 s, and `'lost'` Executions are never revived.
3. **Send `succeed: true` on `POST /v1/sessions` or make `prior_execution_id` optional.**
   - *Rejected:* Unknown JSON fields on `POST /v1/sessions` are ignored by older hubs (`internal/store/sessions.go`), risking unguarded `generation = 0` creation; optional `prior_execution_id` fails to detect equal-generation, different-history restores (`ex_2_live` vs `ex_2_restored`).
4. **Disable `orRegister` unconditionally in `iris watch` now.**
   - *Not taken here:* `cmd/iris/watch.go` already installs `RecallBindingGeneration`, Choice A's server guard + `#408` blocks `watch` from registering `generation = 0` over any hook-registered `active`/`lost`/`ended` session, and `watch` is excluded from succession. Preserving `watch`'s untrusted-folder fallback keeps ADR 034's bootstrap (`TestFirstFallbackChecksGenerationProofAfterGrace` and `TestAMonitorFollowsAdoptionBeforeHandoverIsConfirmed`) working; retiring untrusted-folder fallback for `watch` can be decided separately if desired.

## Limits (what this mechanism cannot do)

- **Not a security boundary against a host-token holder (D13).** Under D13 (`docs/architecture.md`), the trust boundary is the OS user on a host and one token per host; `source`, `prior_generation`, `prior_execution_id`, and `client_pid` are self-declared evidence for attribution and accidental-race protection, not authentication against a token holder (who can already send `source: "hook"`). Session memory, `.execution`, `.ended`, and per-process witnesses sit beside `hub.token` with owner-only permissions.
- **Cannot distinguish a restored backup holding the exact same `(ex_1, G)` row before any replacement occurred.** When both the restored copy (with or without `RestoreMarker` / stream rotation) and the host hold identical `(ex_1, G)` and `ex_1` expires to `'lost'`, counter+ID proof cannot detect the restore; diverged histories (`ex_2_live != ex_2_restored`) and ahead local proofs (`G+2 > G`) are detected and refused.
- **Cannot revive claims or pre-expiry fences.** Claims of `ex_1` were revoked when `ex_1` expired; `ex_2` starts with zero claims until pre-edit hooks or explicit `claim` calls take new ones.
- **Cannot recover clients lacking positive `(session_id, single-session/process)` evidence across `/clear`.** Multi-chat launchers without per-session env identity (such as Cursor IDE once `singleSession` is restricted to `proc.SingleSession`) and single-session clients without a recorded current-session witness remain ineligible.
- **Cannot recover a hook-registered session whose local `<hash>` or `<hash>.execution` proof is missing or damaged (Choice A).** With `generation = 0` non-hook fallbacks blocked over hook-registered history, a session missing local proof stays `lost` after 120 s until a registering hook fires (on Cursor and Grok, that requires an edit or `SessionStart`, not a prompt alone).
- **Cannot run when the OS parent/client process is dead or after `SessionEnd`.** `ended` and `<hash>.ended` are terminal; dead parents terminate the shim before sending requests.
- **One automatic hop without a hook.** Because `ex_new` has `hook_seen_ms = 0`, a second prolonged outage (>120 s) with zero intervening hooks requires a registering hook.
- **Retention removes history after 30 days.** Once ADR 006 retention sweeps an unreferenced expired or ended Execution, neither `#408`'s `ended` guard nor Choice A's `generation = 0` refusal has retained hub history left to check.
- **Read-only console snapshots remain one commit behind concurrent succession.** Under D9 item 5 (`readDB` `BEGIN DEFERRED`), the console observes `ex_new` on subsequent WAL snapshots.

## Verification plan (required before shipping implementation)

Any PR implementing this ADR must achieve 100% Go statement coverage (`make cov-check`), pass `make race`, keep `internal/restorecheck` green, and add deterministic tests for:

1. **Deadline, state, and refusal-atomicity boundaries (`store` & `binding`):**
   - Inside 120 s (`t = 119 s`, `prev.State == 'active' && prev.Recovery.Valid`): `POST /v1/sessions/succeed` is refused (`409 lost`); in-window reattachment handles it instead.
   - Past 120 s (`t = 121 s`) or past 60 s lease / stall-credit cap (`prev.State == 'lost'`): succession commits `ex_new` with `generation = G+1`, `hook_seen_ms = 0`, `lease_renewed_tick_ms = tick`, zero inherited claims, same `Agent`, emits `GenerationChange` (`Source: "shim"`), and requeued unacked deliveries become fetchable and ackable on `ex_new`.
   - Second consecutive outage past 120 s without an intervening hook: refused (`prev.HookSeenMS == 0`).
   - `ex_new` created by succession subsequently receives stall credit across a >5 s hub stall while `sweepArmed == true`, bounded by its initialized `lease_renewed_tick_ms + 60 s + 60 s`.
   - **Refusal non-mutation invariant:** For every refusal case below (`active`, `ended`, Choice A, restore rollback, equal-generation mismatch, `mark-restored` mismatch, `/clear` supersession, wrong PID), verify that `Agent`, `Execution` count/states, `Delivery` assignment/attempts/attention, and `Claim` rows (zero claims created/restored) are unchanged (`databaseState`).
2. **Every `#408` `ENDED` case preserved + Choice A `LOST` withdrawal:**
   - **Every `#408` `ENDED` case:** Client-side readable and damaged `<hash>.ended` block Rung-2/`watch` proofless fallback (`errGenerationFloor` / `errMalformedGeneration` + `damaged generation proof at <path>`); hub refuses `generation = 0` from `"mcp"`, `"shim"`, `"watch"`, and `""` (with `retry_generation` `0` or `> 0`) over an `ended` newest row with `409 lost` + `highest_generation`; unknown `source` and negative `retry_generation` return `bad_request`; hook `SessionStart` still obtains `G+1`; concurrent `SessionEnd` vs non-hook registration leaves `ended` untouched; and `POST /v1/sessions/succeed` refuses `prev.State == 'ended'`.
   - **Proofless `mcp` and `watch` after `LOST` (Choice A):** Both `source = "mcp"` and `source = "watch"` (and `""`) with `generation = 0` over a `lost` hook-registered Execution (both matching `session_id` and unbound Rung-3 synthetic `"mcp-<pid>-<start_ms>"`) are refused with `409 lost` + `highest_generation`, leaving the mailbox, deliveries, claims, and `Execution` count unchanged (replacing `TestFloorBypassProofLessMCPAndWatchAfterLossKeepsCurrentSuccession` under Choice A).
   - **Preserved untrusted-folder fallback & older-hub fail-closed:** Fresh `iris watch` or `iris mcp` with zero prior `hook_seen_ms > 0` rows and no `<hash>`/`<hash>.execution`/`<hash>.ended` still succeeds D6 fallback and `adopt()`; an older hub returning `404 not_found` on `POST /v1/sessions/succeed` causes the shim to fail closed with `lost` and send zero `generation = 0` requests.
3. **Restore lineage, equal-generation different-history, and `mark-restored` databases:**
   - **Ahead local proof (counter rollback):** Hub backed up at `G = 1`, host advances to `G = 3`, backup restored and `G = 1` expires to `lost`: shim presenting `(G = 3, ex_3)` is refused with `409 lost` (`highest_generation = 1`), local `<hash>`, `<hash>.execution`, and `<hash>.ended` stay untouched, zero `generation = 0` / `retry_generation` retries are sent, and `internal/restorecheck` passes.
   - **Equal-generation, different-history restore:** Hub backed up at `G = 1` (`ex_1`), host advances to `ex_2_live` (`G = 2`); restored copy advances to distinct `ex_2_restored` (`G = 2`) and expires to `lost`: shim presenting `(prior_generation = 2, prior_execution_id = ex_2_live)` is refused with `409 lost` (`highest_generation = 2`) because `ex_2_live != ex_2_restored`.
   - **Mark-restored database and stream rotation:** Backup copy marked with `iris admin mark-restored` (`RestoreMarker`) opens on a rotated `stream_id` and advances past 120 s so `ex_1` expires to `lost`: verify stream rotation does not waive `(prior_generation, prior_execution_id)` checks (ahead or mismatched proof is refused `409 lost`; exact matching `(ex_1, G = 1)` succeeds to `G = 2` with `hook_seen_ms = 0` and zero claims).
   - Missing or damaged `<hash>.execution` fails closed locally without calling the hub.
4. **Rung 2 vs Rung 3 identity, Cursor IDE exclusion, parent death, and PID reuse:**
   - Rung 2 (`CLAUDE_CODE_SESSION_ID`, `COPILOT_AGENT_SESSION_ID`, `GROK_SESSION_ID`) succeeds when the shim's ancestor PID differs from `prev.PID` or `prev.PID == 0`, provided `parentAlive()` is true.
   - Rung 3 (`cursor-agent`, `antigravity`, `kimi`, `codex exec`) sends verified `(client_pid, client_pid_start_ms)`; simulated PID reuse (`start_ms` mismatch) or wrong PID is refused on both client and hub.
   - Cursor IDE (`cursor`, `proc.SingleSession == false`) records `single_session = 0` in hooks and is ineligible for Rung-3 succession.
   - Excluded callers (`parentAlive() == false`, Codex sub-agents, `iris watch`, wake helpers, live `IRIS_EXECUTION`, explicit handles, `hook_seen_ms == 0`) are refused.
5. **`/clear` supersession (`active`, `lost`, `ended`):**
   - Single-session process starts `S1`, then `/clear` starts `S2` on the same `(pid, start_ms)`.
   - With `S2` in each of `active`, `lost`, and `ended`: `S1`'s shim is refused locally by the per-process current-session witness (Antigravity `antigravity-helpers`, Kimi `kimi-wake/sessions`, or `sessions/proc-<pid>-<start_ms>`) and independently refused on the hub (`rowid > prev.rowid`).
   - Missing, corrupted, or locked per-process witness fails closed without calling the hub.
6. **Concurrency, `AdvanceGeneration` CAS, and hook/shim races:**
   - Concurrent hook registration and shim succession on `(S1, G)`: exactly one commits `G+1`; losing shim adopts the live winner via `/v1/bind`; no `-2` agent split occurs; hook 1.5 s D3 deadline is unblocked.
   - Concurrent shim successions: one wins `G+1`, loser adopts winner.
   - Hook fires right after shim commits `ex_new` (`G+1`): both when hook reads `(G+1, ex_new)` directly and when hook read `G` and retries with `retry_generation = G` against `highest_generation = G+1`, hook attaches to `ex_new`, sets `hook_seen_ms > 0`, and creates no `G+2`.
   - Concurrent `AdvanceGeneration(1 -> 2)` vs hook `Remember(3)` and vs `Forget` (`.ended` written) under `Sessions.Lock`: never downgrades `3` and never recreates a forgotten `<hash>` / `<hash>.execution`.
