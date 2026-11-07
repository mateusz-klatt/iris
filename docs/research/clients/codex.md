# Codex report (0.166.0)

> Added by the orchestrator: pre-edit denial was later proved live with real authentication on `gpt-6-luna`. The hook received `tool_name: apply_patch` with the patch text in `tool_input.command`, the deny blocked it, the file was not created, and the model reported the refusal. `codex exec` also blocks on an open stdin, so canaries must close it.

2026-11-05, Linux. **Recommendation: explicit Execution binding plus MCP; native app-server wake-up; SQLite transactions and leases (Medium, 1–2d).**

## A. Facts

**Evidence scope.** [MEASURED] `codex --version`, saved help, `python3 probe_runs.py`, `python3 scripted_probe.py`, `python3 appserver_probe.py`; commands/results in `fixtures/`. Four child launches: real-provider exec (401, isolated home/no credentials); scripted-provider exec; rejected socket startup; scripted-provider stdio app-server. Scripted Responses drive real client behavior, not genuine Luna reasoning. All requested model settings were `gpt-6-luna`, low.

**A1 Hooks.** [MEASURED] Project `.codex/hooks.json` fired SessionStart, UserPromptSubmit, PreToolUse/PostToolUse for Bash, Stop, SessionEnd under headless exec. Context reached the captured provider request; Stop block caused another request. PreToolUse exit 1, two-second timeout and invalid JSON all allowed shell execution. **Edit denial was not proved:** incompatible scripted tool payloads failed before execution. Do not make those failures golden denial tests.

[DOCS https://developers.openai.com/codex/hooks/] Additional events: PermissionRequest, Pre/PostCompact, SubagentStart/Stop, Interrupt, Pre/PostModelRequest. Command and `mcp_tool` handlers work; `commandWindows` overrides Windows command text; prompt/agent handlers are skipped. Regex matchers cover tool aliases, start source, compaction trigger, end reason and subagent type. Async handlers cannot control operations or start idle turns. Defaults: 600s, except SessionEnd/Interrupt 1s, maximum 3s. Project trust **and** content-hash hook trust are required; `/hooks` reviews hooks. Hook sources merge across active layers and plugins; managed hooks are trusted by policy.

[SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/events/pre_tool_use.rs] Exit 0 JSON supports allow/deny and context; exit 2 with stderr blocks. Other errors fail open. `ask` and `continue:false` are unsupported here. SessionStart/UserPromptSubmit context uses `hookSpecificOutput.additionalContext`; Stop continuation uses `decision:block,reason`.

[DOCS https://developers.openai.com/codex/config-basic/] Ordinary settings precedence: CLI, nearest trusted project, selected profile, user, cloud defaults, system, built-ins; managed constraints bound settings. Hooks merge rather than following replacement precedence.

**A2 Spawn/sandbox.** [MEASURED] Exec hooks and project stdio MCP shared the Codex PID as direct parent; hook shell evidently exec-replaced itself. Cwd was the workspace. Both reached loopback with workspace-write/network=false; model shell socket creation failed with EPERM. Thus hook/MCP networking and model-shell networking differ. Payload `permission_mode` misleadingly said bypassPermissions; saved turn context and shell result establish the sandbox.

[SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/engine/command_runner.rs] Hooks use a configured shell; fallback Unix `$SHELL` or `/bin/sh -lc`, Windows COMSPEC or `cmd.exe /C`, with Windows raw quoting. Session environment snapshot is replayed with restricted names removed. [SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/engine/discovery.rs] Plugin hooks add PLUGIN_ROOT/DATA and compatibility aliases. macOS/Windows are source-only, not measured.

[SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/rmcp-client/src/stdio_server_launcher.rs] Local MCP launches a resolved executable with an argument vector and configured/fallback cwd; it is not a shell-command string. [MEASURED] MCP environment was sparse and had no CODEX_THREAD_ID after inherited outer identity variables were removed. Full environment **names only** and ancestry are captured.

**A3 MCP.** [MEASURED] `codex mcp add --help` supports `codex mcp add iris -- iris mcp` and `--url URL --bearer-token-env-var NAME`; add was not executed against user configuration. Initialization advertised elicitation; tool listing succeeded.

[DOCS https://developers.openai.com/codex/mcp/] Supported transports: stdio, Streamable HTTP. Trusted project `.codex/config.toml` or user config defines servers. `env` supplies literal values, `env_vars` forwards named variables; `http_headers`, `env_http_headers`, bearer-token env and header helper configure HTTP authentication. `default_tools_approval_mode="approve"` or per-tool approval overrides suppress ordinary MCP prompts, subject to policy. No general `${...}` interpolation should be assumed. Resources are explicitly read; elicitation requests user input.

[MEASURED 2026-11-07, Codex 0.167.0, WSL] Tool annotations decide `default_tools_approval_mode="writes"`. With `approval_policy="never"`, `claims`, which carries `readOnlyHint: true`, ran without approval and reached the hub, while `send` was refused with "MCP tool call requires approval, but approval was unavailable". Before Iris annotated its tools, `writes` refused even `whoami`, as measured on macOS. Iris keeps `approve` for its own server, because under `writes` a headless run still cannot `send`, `claim` or `release`.

[SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/rmcp-client/src/logging_client_handler.rs] Ordinary resource-change/log/progress notifications are logged, not an arbitrary model wake-up channel. [UNVERIFIED] Newer experimental event-stream APIs need separate qualification.

**A4 Packaging.** [MEASURED] `codex plugin --help` exposes install/remove and marketplace management. [DOCS https://developers.openai.com/plugins/build/plugins] Plugins bundle skills, MCP configuration, assets and lifecycle hooks; portable `plugin.json` and compatibility `.codex-plugin/plugin.json` exist. Hook installation does not grant trust; public-directory eligibility differs. [UNVERIFIED] Portable binary installation/signing is not established: distribute Iris separately. [DOCS https://developers.openai.com/codex/guides/agents-md/] Global and project AGENTS.md/AGENTS.override.md load automatically, root toward cwd.

**A5 Wake-up.** [MEASURED] Run 4: external controller initialized app-server, resumed a persisted thread, started an idle turn, and successfully steered an active turn using `expectedTurnId`. Generated schemas and raw RPCs are saved. `queue --help` offers `queue --thread ID --message TEXT --resume`; resume failure only warns. Queue itself was not exercised. Socket startup rejected a replaceable parent directory.

[DOCS https://developers.openai.com/codex/app-server/] stdio JSONL, Unix WebSocket and TCP WebSocket transports exist; initialize first. Use the owning/shared server to steer live threads; an unrelated server is not evidence of attachment to an existing live exec process. [UNVERIFIED] Production TUI attachment, remote-control and queue race behavior need canaries.

**A6 Identity.** [MEASURED] Hook `session_id` equals exec's emitted thread_id; app-server resume kept its thread id. Two thread-specific MCP children shared one app-server PID, neither carrying thread identity: P5 is ambiguous. [SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/core/src/hook_runtime.rs] Compaction uses session identity; subagent hook context includes agent_id/type alongside session identity. [UNVERIFIED] No live compaction/subagent probe was performed.

## B. Minimal Iris integration

**Short (1–4h), proposed adapter.** [UNVERIFIED] Add a launcher, `iris run codex`, supplying an Execution ID/capability to the child; bind native session_id through SessionStart. Shared app-server threads require explicit per-thread binding, not process environment alone.

[SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/engine/discovery.rs] User files are `$CODEX_HOME/{config.toml,hooks.json}`; default Unix/macOS `~/.codex/`; project files `<repo>/.codex/`. [DOCS https://learn.chatgpt.com/docs/enterprise/connect-to-a-gateway] Native Windows config is `%USERPROFILE%\.codex\config.toml`, hooks alongside it; Linux/macOS config is `~/.codex/config.toml`. CODEX_HOME overrides the directory on every OS [SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/utils/home-dir/src/lib.rs].

Proposed `config.toml` (Iris installed on PATH):

```toml
[mcp_servers.iris]
command = "iris"
args = ["mcp"]
env_vars = ["IRIS_EXECUTION_ID", "IRIS_EXECUTION_TOKEN"]
default_tools_approval_mode = "approve"
required = true
```

Proposed `hooks.json` (same JSON on each OS):

```json
{"hooks":{
 "SessionStart":[{"hooks":[{"type":"command","command":"iris hook --client codex --event session-start","timeout":2}]}],
 "PreToolUse":[{"matcher":"^(apply_patch|Bash)$","hooks":[{"type":"command","command":"iris hook --client codex --event pre-tool","timeout":2}]}],
 "SessionEnd":[{"hooks":[{"type":"command","command":"iris hook --client codex --event session-end","timeout":2}]}]
}}
```

[SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/schema.rs] Start output: `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"<bounded inbox data>"}}`. Denial: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"claim conflict"}}`. End: `{}`/exit 0. Parse patch text in `tool_input.command`, including rename endpoints. [UNVERIFIED] Production install needs project trust plus `/hooks` review; never silently install the test bypass flag. Failed canaries must remain visible.

## C. Capability matrix

Values describe supported interfaces, with measurement limits explicit.

| Capability | Verdict/evidence |
|---|---|
| session-start-context | yes [MEASURED] run 2 provider input. |
| prompt-context | yes [MEASURED] run 2 provider input. |
| pre-tool-deny | yes [SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/events/pre_tool_use.rs], not live-proved. |
| post-tool | yes [MEASURED] run 2. |
| stop-block | yes [MEASURED] run 2, one continuation. |
| session-end | yes [MEASURED] exec shutdown. |
| edit-path-visible | partial [DOCS https://developers.openai.com/codex/hooks/], patch parsing required. |
| shell-command-visible | yes [MEASURED] Bash payload. |
| subagent-events | yes [SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/core/src/hook_runtime.rs]. |
| fires-headless | yes [MEASURED] exec. |
| async-handler | yes [DOCS https://developers.openai.com/codex/hooks/]. |
| idle-wake | partial [MEASURED] app-server controlled thread; arbitrary live exec attachment unproved. |

## D. Lens findings

**Medium (1–2d): minimal authoritative model.** All proposed behavior below is [UNVERIFIED] design until implemented. Proposed DDL, also executable as `schema.sql`; application checks below are part of the specification, not guaranteed by DDL alone.

```sql
PRAGMA foreign_keys=ON;
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
CREATE TABLE Meta (
 id INTEGER PRIMARY KEY CHECK(id=1), stream_id TEXT NOT NULL,
 boot_epoch INTEGER NOT NULL CHECK(boot_epoch>0)
);
CREATE TABLE Repository (
 id TEXT PRIMARY KEY, case_policy TEXT NOT NULL
 CHECK(case_policy IN ('sensitive','folded'))
);
CREATE TABLE Agent (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE Execution (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES Agent(id),
 repository_id TEXT NOT NULL REFERENCES Repository(id),
 host_id TEXT NOT NULL, client TEXT NOT NULL, session_id TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation>0),
 worktree TEXT NOT NULL, branch TEXT,
 boot_epoch INTEGER NOT NULL, deadline_tick_ms INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','ended','lost')),
 UNIQUE(host_id,client,session_id,generation)
);
CREATE UNIQUE INDEX execution_live ON Execution(host_id,client,session_id)
 WHERE state='active';
CREATE TABLE Claim (
 fence INTEGER PRIMARY KEY AUTOINCREMENT,
 execution_id TEXT NOT NULL REFERENCES Execution(id),
 repository_id TEXT NOT NULL REFERENCES Repository(id),
 kind TEXT NOT NULL CHECK(kind IN ('exact','prefix')),
 path TEXT NOT NULL, boot_epoch INTEGER NOT NULL,
 deadline_tick_ms INTEGER NOT NULL, revoked_ms INTEGER,
 CHECK(kind='prefix' OR path<>'')
);
CREATE INDEX claim_scan ON Claim(repository_id,revoked_ms,deadline_tick_ms);
CREATE TABLE Message (
 id TEXT PRIMARY KEY, sender_execution_id TEXT NOT NULL REFERENCES Execution(id),
 recipient_agent_id TEXT NOT NULL REFERENCES Agent(id),
 body BLOB NOT NULL CHECK(length(CAST(body AS BLOB))<=16384),
 created_ms INTEGER NOT NULL
);
CREATE TABLE Delivery (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL UNIQUE REFERENCES Message(id),
 execution_id TEXT REFERENCES Execution(id),
 attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt>=0),
 retry_tick_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE Ack (
 delivery_id TEXT PRIMARY KEY REFERENCES Delivery(id),
 execution_id TEXT NOT NULL REFERENCES Execution(id),
 attempt INTEGER NOT NULL CHECK(attempt>0), acked_ms INTEGER NOT NULL
);
CREATE VIEW DeliveryState AS
 SELECT d.*,CASE WHEN a.delivery_id IS NOT NULL THEN 'acked'
 WHEN d.execution_id IS NULL THEN 'pending' ELSE 'inflight' END AS state
 FROM Delivery d LEFT JOIN Ack a ON a.delivery_id=d.id;
CREATE TABLE Event (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL,
 agent_id TEXT REFERENCES Agent(id), execution_id TEXT REFERENCES Execution(id),
 object_id TEXT NOT NULL, created_ms INTEGER NOT NULL
);
CREATE TABLE Cursor (
 consumer_id TEXT PRIMARY KEY, execution_id TEXT NOT NULL REFERENCES Execution(id),
 seq INTEGER NOT NULL CHECK(seq>=0)
);
CREATE TABLE Operation (
 principal_agent_id TEXT NOT NULL REFERENCES Agent(id), key TEXT NOT NULL,
 request_hash BLOB NOT NULL, result_json TEXT NOT NULL,
 PRIMARY KEY(principal_agent_id,key)
);
```

**Atomicity/fencing.** Proposed: one daemon owns an OS lock and writer connection. BEGIN IMMEDIATE; authenticate principal and look up Operation; for new operations validate Execution/epoch/deadlines; expire old claims; scan overlapping claims; insert grant, Event and cached result; COMMIT. SQLite permits one writer [DOCS https://www.sqlite.org/lang_transaction.html]. Same principal/key with different canonical request hash returns 409; exact retry returns original result/expiry without reviving expired rights. Validate current authority before permitting edits, even after a cached grant. Include operation and Execution in the hash. Retain keys through the retry horizon.

Proposed TTL: Execution/claim 60s, renewal every 20s; claim deadline cannot exceed Execution deadline. Fence=(stream_id,boot_epoch,fence,execution_id); check it on renewal/release and every mediated action, with deadlines strictly greater than daemon time. Resuming an expired session creates a new Execution generation; compaction does not. Never release on Stop (a turn boundary) or depend on SessionEnd cleanup. Monotonic daemon ticks govern leases, client clocks never do. Startup increments epoch, loses old executions, revokes claims and requeues unacked deliveries transactionally. Invalidate leases on detected suspend/time discontinuity. Keep database on hub-local disk: WAL is unsuitable for cross-host network filesystems [DOCS https://www.sqlite.org/wal.html]. Restore rotates stream identity and invalidates capabilities.

**Conflict semantics.** Proposed: repository-wide across worktrees and branches; branch/worktree are context, never implicit exemption. Validate claim repository against Execution context. Paths are relative POSIX, reject traversal/escapes, pin case policy per repository and reject folding collisions. Exact/prefix overlaps use component boundaries (`src` excludes `src2`); empty prefix covers repository. Check both rename paths and symlink escapes. Reject arbitrary globs initially; optionally map `dir/**` to prefix. Do not expand against today's files: future paths must conflict. Replace URL+root-commit identity with hub-assigned repository ID plus explicitly confirmed aliases.

**Delivery/watch.** Proposed: Message, pending Delivery and Event commit together. Fetch assigns authenticated recipient Execution and increments attempt; retry until explicit ack. Ack validates current live recipient Execution/attempt and inserts once; duplicate fetches deduplicate by delivery ID, not body. Ack means accepted, not model obedience. Failed/expired Execution returns unacked deliveries to pending; acknowledged deliveries stay complete. Event cursor is independent of message ack.

Proposed watch frame: `{"stream":"uuid","cursor":123,"kind":"inbox_changed"}`; no content. Authorize per Execution, replay ordered seq>cursor, accept duplicates, persist monotonic consumer cursor. Initial subscription also signals outstanding inbox work. Capture snapshot/high-water consistently; expired cursor returns reset plus snapshot, never silent truncation. For Monitor, issue scoped expiring URL/subprotocol capabilities; CLI watch prints the same frames. Commit precedes notification; event log is the outbox. Reconnect resumes; wake-up never acknowledges a message.

**Required invariants:** concurrent overlapping grants have one winner; prefix/case/rename boundaries work across worktrees; expired/released/wrong-owner/old-epoch tokens fail; lost leases cannot renew; retries cannot double-send or resurrect claims; partial transactions expose nothing; deliveries survive crashes and deduplicate; stale ack fails and repeated ack is harmless; cursors never regress; reconnect neither loses committed events nor exposes other recipients/content; clock skew cannot extend leases; restart/restore fences zombies. [UNVERIFIED] Architectural boundary: Iris tokens fence mediated operations, not arbitrary filesystem writers. Git guards are cooperative workflow controls, not file locks.

[MEASURED] `python3 domain_checks.py`: 23 reference checks passed, including 50 simultaneous contenders, expiry/replacement, rollback, ack, cursor and restart. Python SQLite, **not** modernc/Go or exhaustive crash testing; results in fixtures. AUTOINCREMENT prevents reuse of committed deleted IDs [DOCS https://www.sqlite.org/autoinc.html].

## E. Verdict on P1 to P12

P1 **CHANGE:** MCP baseline; claims are advisory outside mediated writes. Git guards are workflow enforcement, not an absolute boundary.
P2 **AGREE:** keep one adapter; native MCP hooks optional. [DOCS https://developers.openai.com/codex/hooks/]
P3 **CHANGE:** explicit two-second handler timeout, exit 0 valid fallback; latency targets need Go benchmarks. [MEASURED] failure tests.
P4 **CHANGE:** loopback works locally [MEASURED]; separate host network namespaces require configured endpoints/bridges, not assumed equivalence.
P5 **REJECT:** explicit scoped Execution binding; shared app-server PID cannot distinguish threads. [MEASURED] run 4.
P6 **CHANGE:** retain hub; add host agent only for actual local wake routing. [UNVERIFIED] lazy start/service timing needs canaries.
P7 **CHANGE:** reconcile with NUL-delimited porcelain, include untracked/renames; reconciliation detects changes, cannot undo unauthorized writes.
P8 **CHANGE:** native Codex app-server wake/steer adapter plus cursor watch. [MEASURED] controlled thread; queue remains help-only.
P9 **REJECT:** assign repository UUID; confirm aliases. Remotes/root commits are hints, not authoritative identity.
P10 **AGREE:** bounded provenance-labelled data; never promote message bodies into executable decisions.
P11 **CHANGE:** idempotent installation plus explicit trust review and truthful doctor; headless trust bypass is not installation. [DOCS https://developers.openai.com/codex/hooks/]
P12 **AGREE:** proposed distribution target; [UNVERIFIED] signing/package feasibility was outside this consultation.

## F. From scratch

Start with one hub, explicit session binding, exact/prefix claims, durable inbox and replayable events. Add one launcher and the measured Codex app-server adapter. Avoid process ancestry, automatic repository merging, arbitrary globs and inferred message acknowledgements. Qualify each adapter independently; give doctor a truthful partial-capability result. Estimated first coherent slice: Medium (1–2d).

## G. Unknowns

Section H below settles several of these on macOS with 0.167.0: edit and deny, MCP calls, failure
semantics and the hook review. What it leaves open is said there.

[UNVERIFIED] Genuine Luna edit/deny and MCP tool-call metadata: rerun authenticated canary with correctly routed Code Mode custom calls. Auth was unavailable; four launches exhausted. [MEASURED] App-server hooks did not fire. [SOURCE https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/cli/src/main.rs] Its app-server branch does not propagate the exec hook-bypass override; review/persist trust before retesting. [UNVERIFIED] No claims of a successful edit denial, live subagent/compaction/async behavior, HTTP MCP loopback, queue execution, TUI attachment, Windows/macOS sandbox reachability or p95 latency. Verify those on target platforms with real Iris binary and Go/modernc tests. See fixture provenance before selecting golden tests.

## H. Measured on macOS, 0.167.0 (2026-11-07)

Real headless runs against a local hub on an Intel Mac. `HOME` pointed at a shadow home whose entries
link to the real ones, except that the Iris configuration names the local hub. No run could reach
another hub. The Codex configuration was compared byte for byte with a backup afterwards.

**Hook review.** [MEASURED] Until the hooks are reviewed, `codex exec` runs none of them. No session
reached the hub, and edits went unclaimed. `--dangerously-bypass-hook-trust` runs them for one
invocation without writing anything, and it emits a warning item. A fresh TUI start shows Codex's own
dialog:

```
Hooks need review
6 hooks are new or changed. Hooks can run outside the sandbox after you trust them.
› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)
```

Choosing 2 wrote one table per hook to `config.toml`:

```toml
[hooks.state."<hooks.json path>:session_start:0:0"]
trusted_hash = "sha256:<64 hex digits>"
```

- The key is the `hooks.json` path as Codex builds it from `HOME` or `CODEX_HOME`, without resolving
  links, then the event in snake_case, the matcher group index and the hook index.
- A different `HOME` or `CODEX_HOME` makes a different key, so it needs its own review.
- Codex calls a changed hook "new or changed" and asks again, so the hash covers the hook's content.
- The hash could not be reproduced. SHA-256 over about 14,000 candidate serializations matched none:
  the hook and group objects as written, every key order, serde field order with and without
  defaults, event-name and key prefixes, and the raw command.
- An older integration on the same machine had left `trusted_hash` tables for index `0:0` under the
  real path. A table can therefore exist for a hook that is no longer the one reviewed.

Doctor therefore reports a recorded review without claiming that the hash was verified. Only
`iris doctor --live --client codex` proves that the hooks run. `iris integrate` never writes
`hooks.state`.

**Headless runs.** [MEASURED] `codex exec <prompt>` with stdin left open waits forever on "Reading
additional input from stdin"; stdin must be closed. Every `exec` in a new directory appends
`[projects."<dir>"] trust_level = "trusted"` to `config.toml`, with `danger-full-access` and with
`workspace-write` alike. `-c sandbox_mode="workspace-write" -c approval_policy="never"` allowed an
`apply_patch` edit in the working directory, and the hooks still reached the hub.

**MCP server.** [MEASURED] Codex passes the full environment to hooks and a filtered one to MCP
servers. With `client.json` on a dead port and `IRIS_HUB_URL` on the live hub, the hooks claimed on
the live hub, while `whoami` dialed the dead port. `env_vars` naming the Iris variables fixed it
(#86). Under `approval_policy="never"` every MCP call was refused with "MCP tool call requires
approval, but approval policy is never". `default_tools_approval_mode="approve"` in the Iris table
fixed that. The other value tried, `writes`, approves only tools marked read-only and asks for the
rest; it refused even `whoami`, because the Iris tools carried no read-only annotation.
On macOS automatic binding is not available for Codex. The first `whoami` answered `unbound`, and the
model then passed the SessionStart handle itself as the tool's `execution` argument, the explicit
handle rung of D6 in `docs/architecture.md`.

**Failure semantics.** [MEASURED] A hook that exits 1, one that sleeps past the 3 s timeout, and one
whose binary is missing all allowed the edit, matching the failure table in D3 of
`docs/architecture.md`. A claimed file was denied with
"Command blocked by PreToolUse hook: Iris: …". The free file was claimed automatically, and the
claim was released at session end.

**Edit wording.** [MEASURED] "Replace the whole content of the file" made the model write a patch
that deletes and adds the same path, which `apply_patch` rejects. That happened three times out of
three. "Append one line" produced an `Update File` hunk that worked.

**Code Mode writes.** [MEASURED] `gpt-6-luna` wrote a file through the `js` tool
(`mcp__node_repl__js`, `{"code": "…fs.appendFile(…)", "title": "…"}`) instead of `apply_patch`, so
no pre-edit claim was taken. Codex fires PreToolUse and PostToolUse for that tool; PostToolUse also
carries `tool_response`. Iris reconciles it like a shell: detection, not prevention.
`gpt-6.1-terra` used `apply_patch` in every run.

**Still open after H.** Not measured here: attention and wake in the TUI, subagent, compaction and
async hooks, app-server sessions, Windows and Linux, and hook latency in Codex itself.
