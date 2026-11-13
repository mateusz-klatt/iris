# 028: Antigravity idle wake through a model-armed helper

Status: proposed for opt-in implementation on the owner's direction, 2026-11-13. Changes D7 for
Antigravity only. Extends the single-wake policy of ADR 017 and ADR 023 without changing the other
clients.

## Problem

Antigravity CLI (`agy`) hooks and its stdio MCP child (`iris mcp --client antigravity`) cannot
start a model turn while a session is idle, so a loaded session learns of new Iris mail or freed
claims only on a later turn.

`agy` provides `agy agentapi send-message <conversation_id> <message>`, which dials the session's
in-process Language Server (`ANTIGRAVITY_LS_ADDRESS`) using a per-process secret
(`ANTIGRAVITY_CSRF_TOKEN`). Clean-environment measurements on `agy 1.5.8` showed why neither
`SessionStart` nor `iris mcp` can run that command directly:

- Top-level `agy` has no `ANTIGRAVITY_*` variables in its initial process environment, writes no
  port or token file to disk, and does not start workspace plugin sidecars in CLI mode (`1.5.8`).
- `iris mcp --client antigravity` receives no `ANTIGRAVITY_*` variables.
- All six lifecycle hooks (`SessionStart`, `PreInvocation`, `PreToolUse`, `PostToolUse`,
  `PostInvocation` and `Stop`) receive `ANTIGRAVITY_CONVERSATION_ID` alone.
- Only subprocesses started by the `run_command` tool receive `ANTIGRAVITY_LS_ADDRESS`,
  `ANTIGRAVITY_CSRF_TOKEN` and `ANTIGRAVITY_AGENTAPI_EXE` alongside `ANTIGRAVITY_CONVERSATION_ID`.
  Earlier experiments saw those variables in hooks because an inner `agy --print` had been launched
  from an outer Antigravity `run_command` shell, inheriting the outer shell's environment.

## Measurements

All measurements ran on Linux/WSL2 with `agy 1.5.8` (`gemini-3.8-flash-low`) in isolated PTY
sessions after stripping every `ANTIGRAVITY_*` variable from the outer environment.

1. **Delivery while idle.** Sent 2.0 s after `Stop` (`fullyIdle: true`), `agy agentapi send-message`
   exited `0` in `905 ms` and started a new model turn about `45 ms` later. In `transcript.jsonl`,
   the message was recorded as a `SYSTEM_MESSAGE` step (`source: "SYSTEM"`,
   `type: "SYSTEM_MESSAGE"`), not `USER_INPUT`. When `PreInvocation` ran for that woken turn,
   `lastUserInput` still held the earlier user prompt and the `SYSTEM_MESSAGE` step had not yet
   been flushed to `transcript.jsonl`; by `PostInvocation` and `Stop` it was on disk.
2. **Delivery while busy.** Sent 1.0 s into a 4 s `run_command`, `send-message` exited `0` in
   `713 ms` without interrupting the running tool. `agy` queued it and injected it as a
   `SYSTEM_MESSAGE` step right after `PostToolUse`, before the next model call (`invocationNum: 1`)
   of the same turn, finishing in a single `Stop`.
3. **Conversation identity.** `ANTIGRAVITY_CONVERSATION_ID` in hook and `run_command` environments
   equaled `conversationId` in hook stdin JSON and the `<recipient_id>` accepted by
   `agy agentapi send-message`.
4. **Detached helper lifetime.** A helper detached into a new session with standard streams closed
   (`/dev/null`) survived `run_command` exit and all Turn 1 hooks, woke the idle session (`634 ms`
   in the non-bypass sandbox run) and exited when the host `agy` process ended.
5. **Stale credentials and dead server.** Against a live Language Server with a wrong or stale
   `ANTIGRAVITY_CSRF_TOKEN`, `send-message` exited `1` (`Unauthenticated: invalid CSRF token`;
   missing token: `missing CSRF token`). After `agy` exited, dialing the old
   `ANTIGRAVITY_LS_ADDRESS` exited `1` (`Unavailable: connect: connection refused`).
6. **Arming point and `continuationActive` drift.** Without `--dangerously-skip-permissions`, a
   `PreInvocation` `injectSteps` `systemMessage` asking the model to start the helper produced zero
   tool calls (`gemini-3.8-flash-low` answered the prompt and stopped), whereas `Stop` returning
   `{"decision":"continue","reason":"..."}` caused the model to call `run_command` right after
   Turn 1's answer. Antigravity's hook documentation states that `continuationActive` is `true`
   when `executionNum > 0`, but on `agy 1.5.8` `continuationActive` remained `false` across
   `executionNum` `0` through `4`.
7. **Terminal sandbox.** Under `agy --sandbox` (and `auto` mode), `run_command` executes in a
   namespace sandbox that sees only namespace PIDs (`[1, 31]`), kills detached children when the
   foreground command exits, cannot access `~/.config/iris`, and gets `connection refused` dialing
   `127.0.0.1:<LS_PORT>`. When the `Stop` continuation asked `run_command` to set
   `BypassSandbox: true`, the exact rule `command(<iris-path> watch --client antigravity --wake)`
   in `~/.gemini/antigravity-cli/settings.json` (`permissions.allow`) approved the unsandboxed
   command without prompting on a personal account.

## Decision

### Opt-in configuration and exact permission rule

Wake for Antigravity is opt-in and off by default. `iris integrate antigravity --wake` writes a
local marker (`on`) beside the Iris configuration and adds the exact allow rule
`command(<iris-path> watch --client antigravity --wake)` — using the absolute `iris` path and exact
arguments, with no wildcards — to `~/.gemini/antigravity-cli/settings.json` (`permissions.allow`),
keeping `mcp(iris/*)` and every user setting intact.

Fresh `iris integrate antigravity` leaves wake off and writes no wake command allow rule. Plain
`iris integrate antigravity` and `iris setup` preserve an existing explicit `on` or `off` marker
(updating Iris's wake command rule if `on` and the binary path changed).
`iris integrate antigravity --no-wake` writes `off` and removes Iris's wake command allow rule from
`settings.json`. `iris integrate --remove antigravity` removes the marker and Iris's wake command
allow rule along with `mcp(iris/*)`, the hooks and the MCP server entry; a rule the user already
had before Iris wrote it stays theirs.

### Local helper state and `Stop` arming request

Per-process Antigravity wake state lives in a `0700` directory beside `sessions/` (mode `0600`
files, owner-only DACL on Windows), not inside `sessions/`, so generation pruning does not remove
active helper records. Each record is keyed by `(agy pid, start_ms)` of the nearest `agy` ancestor
(`proc.Nearest`) and holds either the detached helper's `(helper_pid, helper_start_ms, session_id)`
or a `failed` mark for that `agy` process. All reads and atomic replacements stay within the hook
lock budget and never call the hub.

Only when the local wake marker is `on` does `Stop` consider asking for the helper, and only when
every gate holds:

- `fullyIdle` is `true`;
- `terminationReason` is `"NO_TOOL_CALL"` (shutdown, cancellation, errors and missing reasons never
  ask);
- `executionNum` is `0` and `continuationActive` is `false`, which enforces at most one ask per
  turn despite the `agy 1.5.8` `continuationActive` drift and remains sound if a later `agy`
  release sets `continuationActive: true`;
- the nearest `agy` ancestor `(pid, start_ms)` is known, its local record is not marked `failed`,
  and no live process matching `(helper_pid, helper_start_ms)` for this `conversationId` exists
  (`proc.Lookup`, rejecting dead and zombie states).

When all gates pass, `Stop` returns `{"decision":"continue","reason":"..."}` asking the model once
to run the exact `<iris-path> watch --client antigravity --wake` command via `run_command` with
`BypassSandbox: true` (when `run_command` exposes `BypassSandbox`). With `--wake`, the helper runs
outside Antigravity's terminal sandbox. Any read, lock or parse failure in `Stop` allows (`{}` with
exit `0`, D3) and stayed well within the hook deadline in measurement.

### No ask loop on failed setup

A failing setup costs at most one continuation for that `agy` process, never one per turn:

- If the helper runs on the host and fails preflight — missing `ANTIGRAVITY_CONVERSATION_ID`,
  `ANTIGRAVITY_LS_ADDRESS` or `ANTIGRAVITY_CSRF_TOKEN`, a non-loopback `ANTIGRAVITY_LS_ADDRESS`,
  an unreachable Language Server, or failure to establish its lock — it writes `failed` to the
  `(agy pid, start_ms)` record and exits `1`.
- If the helper starts inside Antigravity's sandbox (where `~/.config/iris` is unmounted and host
  PIDs are hidden), cannot write its record, or has `BypassSandbox` refused, the helper exits `1`
  and `PostToolUse` on the host — seeing the exact `<iris-path> watch --client antigravity --wake`
  `run_command` finish with no live helper recorded for `(agy pid, start_ms)` — writes `failed` for
  that `(agy pid, start_ms)`.

Once `(agy pid, start_ms)` is marked `failed`, `Stop` stops asking for the lifetime of that `agy`
process.

### Detached helper (`iris watch --client antigravity --wake`)

Before detaching, `iris watch --client antigravity --wake` resolves its host `agy` ancestor
`(pid, start_ms)`, validates `ANTIGRAVITY_LS_ADDRESS` as a literal loopback `host:port` reachable
by a short TCP dial alongside non-empty `ANTIGRAVITY_CONVERSATION_ID` and
`ANTIGRAVITY_CSRF_TOKEN`, and checks the `(agy pid, start_ms)` record. If a live helper already
owns that `(agy pid, start_ms)` for the same `conversationId`, the command exits `0` at once.
Otherwise it spawns a detached background `iris` child in a new session with `stdin`, `stdout` and
`stderr` attached to `os.DevNull` so `run_command` returns immediately.

The detached child records `(helper_pid, helper_start_ms, session_id)` under `(agy pid, start_ms)`,
binds only to the hook-registered Execution of that single-session `agy` process whose `session_id`
matches `ANTIGRAVITY_CONVERSATION_ID` (never registering a fallback Execution), heartbeats that
binding while checking `proc.Lookup(agy_pid)` against `start_ms`, and exits as soon as that `agy`
process ends, its bound `session_id` no longer matches, or another session's helper replaces the
`(agy pid, start_ms)` record (such as after `/clear`).

`ANTIGRAVITY_CSRF_TOKEN` stays in memory only: it is never written to disk, never placed in a log
and never passed in `argv`. It reaches the Language Server client only through the child process
environment; under D12, processes of the same OS user share the host boundary and can already
inspect `/proc/<pid>/environ` of `agy` and its children.

### Hook idle state, wake quota and `PostInvocation` guard consumption

Hooks track session idle state locally per `(client, conversationId)` beside the helper record:

- `PreInvocation` clears idle (`false`) before each model call.
- `Stop` sets idle (`true`) only when `fullyIdle` is `true` and `Stop` is not returning
  `decision: "continue"`; any other `Stop` clears idle.

When `iris watch` observes pending attention for the bound Execution, admission runs through
`internal/wake` (`antigravity` client state store). The helper requires local idle to be `true` and
the `agy` process to be live, then re-reads authoritative attention and told state under the
shared sessions lock before directly executing `<ANTIGRAVITY_AGENTAPI_EXE> agentapi send-message
<conversationId> <prompt>` (or `agy agentapi send-message` from `PATH` when
`ANTIGRAVITY_AGENTAPI_EXE` is unset, never through a shell) with the fixed, content-free Iris
notification and its correlation token. Busy, missing or unreadable idle state defers without
spending wake quota. One automatic wake per attention identity remains the limit, with the shared
single retry on a known refusal (`send-message` exit `1`) and an unspent/retained guard on unknown
outcomes or the 30-second queue deadline.

Because `agy` records `send-message` in `transcript.jsonl` as a `SYSTEM_MESSAGE` step that is on
disk by `PostInvocation` (and not in `PreInvocation`'s `lastUserInput`), `PostInvocation` reads a
bounded tail of `transcriptPath` and consumes the outstanding `antigravity` wake guard when a
`SYSTEM_MESSAGE` step contains that exact Iris notification and correlation token.

## Limits

- **No wake before arming, and one shell step per session.** Neither `SessionStart` nor `iris mcp`
  has the Language Server address or token, so nothing can wake a session before its first turn
  reaches `Stop` and arms the helper. Arming costs one `Stop` continuation and one `run_command`
  step per `agy` process.
- **Request, not enforcement.** `Stop` asks the model to start the helper; a model may ignore the
  request or omit `BypassSandbox: true`. If the model never calls `run_command`, no helper runs.
- **Runs outside Antigravity's terminal sandbox; enterprise accounts may refuse it.** With
  `--wake`, the helper runs unsandboxed on the host so it can reach `~/.config/iris`, the hub, host
  process state and `127.0.0.1:<LS_PORT>`. The `command(...)` allow rule covers `BypassSandbox` on
  personal accounts, whereas Antigravity enterprise and business policies enforce separate
  `unsandboxed(...)` review or disable sandbox bypass, so `--wake` does not work where policy
  refuses it.
- **Busy-turn race.** Idle observation in hooks and `send-message` submission in the helper are not
  atomic. If the user starts a turn between the idle check and submission, `agy` queues the message
  and injects it as a `<SYSTEM_MESSAGE>` into that running turn (measurement 2), so the model may
  answer it as part of the current turn or run a follow-up tool call after the user's work. Iris
  cannot promise zero billable empty turns.
- **Live `agy` process required and stale credentials fail.** The helper works only while the `agy`
  process that spawned it stays alive. After an `agy` exit or restart, the previous Language Server
  address or CSRF token fails with exit `1` (measurement 5); a resumed session in a new `agy`
  process must arm a fresh helper.
- **Platform scope.** The clean-environment lifecycle, `Stop` continuation, `BypassSandbox`
  behavior and `transcript.jsonl` `SYSTEM_MESSAGE` shape were measured on Linux/WSL2 with
  `agy 1.5.8`; macOS and Windows require separate live verification.

## Verification

Unit and integration tests at 100% statement coverage will verify:

- `iris integrate antigravity` opt-in (`--wake`), `--no-wake`, plain reintegration preservation,
  exact `command(<iris-path> watch --client antigravity --wake)` insertion and removal in
  `settings.json`, user-owned rule preservation, and `--remove`;
- `Stop` arming gates (`fullyIdle`, `terminationReason == "NO_TOOL_CALL"`, `executionNum == 0`,
  `!continuationActive`, wake marker `on`/`off`, live helper liveness via `proc.Lookup`, and D3
  fail-open on errors);
- single-attempt failure recording on both host preflight failure and `PostToolUse` after a failed
  or sandboxed helper command, confirming `Stop` never loops;
- helper deduplication on `(agy pid, start_ms)`, detached startup, positive single-session binding
  with `session_id == ANTIGRAVITY_CONVERSATION_ID`, exit on `agy` termination, and absence of
  `ANTIGRAVITY_CSRF_TOKEN` from disk, logs and `argv`;
- hook idle transitions (`PreInvocation` clear, `Stop` set only when `fullyIdle` without
  continuation), `internal/wake` quota and retry accounting against a fake `agentapi` binary, and
  `PostInvocation` guard consumption from real-shape `transcript.jsonl` `SYSTEM_MESSAGE` lines
  while rejecting `USER_INPUT` and non-matching steps.

Run the full local WSL gates (`make test`, `make race`, `make cov`, `make cov-check`,
`make check-all` and `make fixtures`) before implementation review.
