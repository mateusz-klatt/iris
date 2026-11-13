# Antigravity CLI (`agy`) report (1.5.1)

> Corrections: hooks receive `ANTIGRAVITY_CONVERSATION_ID` only; the other `ANTIGRAVITY_*` variables (`ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_CSRF_TOKEN`, `ANTIGRAVITY_AGENTAPI_EXE`, `ANTIGRAVITY_TRAJECTORY_ID` and the rest) reach `run_command` subprocesses only. The 1.5.1 experiment saw them in hooks because an inner `agy --print` was started inside an outer Antigravity `run_command` shell (re-measured on 1.5.8 with outer `ANTIGRAVITY_*` stripped; see section I and [ADR 028](../../adr/028-antigravity-idle-wake.md)). The stdio MCP process receives no `ANTIGRAVITY_*` variables, as `mcp_process.json` shows, which contradicts A6, D9 and P5 below. `CLAUDE_DELEGATOR_AGY_DEPTH` belongs to the delegation bridge, not to agy. One run consumed 28% of the weekly Gemini quota and the reply 69% of the other group.

> Measured on: Linux x86-64, agy 1.5.1 (Go 1.28-20261102-RC00, dynamically linked).  
> Workspace: `.agents/hooks.json` and `.agents/plugins/iris-test/` created for this experiment.

---

## A. Facts

### A1 Hooks

**Existence:** Yes. Full lifecycle hook system. `[MEASURED agy --help + strings /path/to/agy]`

**Events (all 6 measured firing in `--print` headless mode):**

| Event | When | Matcher |
|-------|------|---------|
| `SessionStart` | Once at new session start | N/A |
| `PreToolUse` | Before every tool call | regex on tool name |
| `PostToolUse` | After every tool call | regex on tool name |
| `PreInvocation` | Before the model is called | N/A |
| `PostInvocation` | After tool calls finish per turn | N/A |
| `Stop` | When execution loop terminates | N/A |

`[MEASURED: agy --print=... --dangerously-skip-permissions --model gemini-3.8-flash-low; fixtures/log.jsonl 57 entries]`

**Config locations and precedence:** Project `.agents/hooks.json` > plugin `.agents/plugins/<n>/hooks.json` > global `~/.gemini/config/hooks.json`. Multiple named hooks across layers merge sequentially. `[DOCS https://antigravity.google/docs/hooks; MEASURED: .agents/hooks.json loaded without trust prompt in -p mode]`

**Handler types:** Only `type: "command"` (sh). No HTTP, no prompt handler. `[DOCS agy-customizations/docs/hooks.md: "Current Limitations"; MEASURED]`

**Matchers:** Regex on lowercased tool name. `".*"` matches all. `[MEASURED: matcher ".*" triggered on write_file and run_command]`

**Stdin payload (camelCase):** Common: `conversationId`, `workspacePaths`, `transcriptPath`, `artifactDirectoryPath`, `modelName`, `lastUserInput`. Tool events add `stepIdx`, `toolCall.{name,args}`, `error`. Stop adds `terminationReason`, `executionNum`, `fullyIdle`. `[MEASURED fixtures/]`

**Output contract:**
- `PreToolUse`: `{"decision":"allow"|"deny"|"ask"|"force_ask","reason":"...","overwrite":{...}}` `[MEASURED deny confirmed]`
- `PostToolUse`: `{}` `[DOCS]`
- `SessionStart`: `{"injectSteps":[...]}` with `systemMessage`, `userMessage`, `ephemeralMessage`, `errorMessage` step types `[MEASURED]`
- `PreInvocation`: `{"injectSteps":[...],"terminationBehavior":""|"terminate"}` `[DOCS]`
- `PostInvocation`: same + `"force_continue"` `[DOCS]`
- `Stop`: `{"decision":"continue"|<other>}` `[MEASURED]`

**Exit-code semantics / fail-open:** Non-zero exit on `PreToolUse` **blocks** the tool — fail-closed. No per-hook fail-open flag exists. `[MEASURED: IRIS_HOOK_FAIL=1 → "JSON hook failed: command failed: exit status 1" → write_file blocked; no on-failure setting in docs or --help]`

**Default timeout:** 30 s. `[DOCS]`

**Fires headless:** Yes, all 6 events observed in `--print` mode. `[MEASURED]`

**Trust step:** None required for `.agents/hooks.json` in `--print` mode. `[MEASURED]`

---

### A2 Spawn mechanics

**Shell:** `sh -c` on Unix; `cmd /c` on Windows. `[DOCS hooks.md]`

**Hook CWD:** Directory containing `hooks.json` (`.agents/` for project hooks). `[MEASURED fixtures/log.jsonl cwd=".../workspace/project/.agents"]`

**Process tree:** `agy → sh → iris/python3`. Hook grandparent = main agy process. `[MEASURED fixtures/log.jsonl parent_cmdline="sh -c ...", grandparent_cmdline="agy --print=..."]`

**MCP server process:** Direct child of agy (not via sh). CWD = plugin directory. Same agy PID as hook's grandparent — MCP and hook share the agy parent. `[MEASURED fixtures/mcp_process.json mcp_ppid = hook grandparent_pid]`

**Key env vars injected (key names only, never values):** `ANTIGRAVITY_CONVERSATION_ID`, `ANTIGRAVITY_TRAJECTORY_ID`, `ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_CSRF_TOKEN`, `CLAUDE_DELEGATOR_AGY_DEPTH`, `CLAUDE_CODE_MESSAGING_TOKEN`, `CLAUDE_CODE_SESSION_ID`, plus full inherited shell env. `[MEASURED fixtures/log.jsonl env_keys + mcp_process.json env_keys; CORRECTED in 1.5.8 clean-env run: hooks receive ANTIGRAVITY_CONVERSATION_ID only, MCP receives none, and the remaining ANTIGRAVITY_* vars are injected into run_command subprocesses only]`

**Sandbox and loopback:** `--sandbox` restricts terminal but loopback HTTP remains accessible. `[MEASURED: agy --sandbox curl http://127.0.0.1:9 succeeded]`

---

### A3 MCP

**Transports:** Stdio (local `command`+`args`+`env`) and Streamable HTTP (`url`). Legacy SSE (2024-11-05) not supported. `[DOCS agy-customizations/docs/mcp_servers.md; MEASURED agy mcp add --help]`

**Config locations:** Global `~/.gemini/config/mcp_config.json`; plugin `.agents/plugins/<n>/mcp_config.json`. `[DOCS; MEASURED plugin loaded successfully]`

**Non-interactive add:**
```bash
agy mcp add <name> <cmd-or-url> [args...]
agy mcp add --env KEY=VAL --header "Authorization: Bearer T" <name> <url>
```
`[MEASURED agy mcp add --help]`

**Tool auto-approval:** `--dangerously-skip-permissions` for headless. Selective: `permissionOverrides` in hook output or settings allowlist. `[DOCS app.md "Command Allowlist"]`

**Server-initiated notifications / resources:** `[UNVERIFIED — `ListMcpResources` RPC found in binary; verify by implementing an MCP notification server]`

---

### A4 Packaging

Plugin (`plugin.json` + `hooks.json` + `mcp_config.json` + `skills/` + `rules/`) in `.agents/plugins/<name>/`. Binaries referenced by path, not embedded. `agy plugin validate` checks all files. `[DOCS agy-customizations/docs/plugins.md; MEASURED agy plugin validate .agents/plugins/iris-test → passed]`

GEMINI.md / AGENTS.md load automatically as rules from every directory on the cwd→root path. `[DOCS rules.md]`

---

### A5 Wake-up

`agy acp` — stdio JSON-RPC ACP server for programmatic session control. `[MEASURED agy acp --help]`  
`agy remote-control start` — registers `antigravity-cli-daemon.service` (systemd --user on Linux). `[MEASURED agy remote-control status: "journalctl --user -u antigravity-cli-daemon.service"]`  
`agy --conversation <id>` — resume by ID. `[MEASURED agy --help]`  
No direct prompt injection into an idle interactive TUI without ACP. `[MEASURED]`

---

### A6 Session identity

`conversationId` UUID in every hook stdin payload + `ANTIGRAVITY_CONVERSATION_ID` env var in hooks and MCP processes. Stable within a session; `--conversation <id>` resumes it. `ANTIGRAVITY_TRAJECTORY_ID` = per-execution run ID. `CLAUDE_DELEGATOR_AGY_DEPTH` = subagent nesting depth. `[MEASURED fixtures/session_start.json, mcp_process.json]`

`stepIdx` in PreToolUse/PostToolUse uniquely identifies the tool step within a conversation. `conversationId` + `stepIdx` = unique hook event join key.

---

## B. Minimal Iris integration

**Plugin drop (project-scoped, works for all three events):**

`.agents/plugins/iris/plugin.json`:
```json
{ "name": "iris", "version": "1.0.0" }
```

`.agents/plugins/iris/hooks.json`:
```json
{
  "iris-session": {
    "SessionStart": [{ "type": "command", "command": "iris hook --client agy --event session-start", "timeout": 10 }],
    "PreToolUse": [{ "matcher": "write_file|edit_file|run_command|move_file|delete_file",
      "hooks": [{ "type": "command", "command": "iris hook --client agy --event pre-tool-use", "timeout": 3 }] }],
    "PostToolUse": [{ "matcher": "write_file|edit_file|run_command|move_file|delete_file",
      "hooks": [{ "type": "command", "command": "iris hook --client agy --event post-tool-use", "timeout": 3 }] }],
    "Stop": [{ "type": "command", "command": "iris hook --client agy --event stop", "timeout": 10 }]
  }
}
```

`.agents/plugins/iris/mcp_config.json`:
```json
{ "mcpServers": { "iris": { "command": "iris", "args": ["mcp"] } } }
```

**OS paths:**

| OS | Plugin root | Global MCP config |
|----|------------|-------------------|
| Linux / WSL2 / macOS | `<repo>/.agents/plugins/iris/` | `~/.gemini/config/mcp_config.json` |
| Windows native | `<repo>\.agents\plugins\iris\` | `%USERPROFILE%\.gemini\config\mcp_config.json` |

`[DOCS hooks.md, mcp_servers.md; MEASURED on Linux]`

**Session-start:** `SessionStart` hook → `iris hook` reads stdin → registers session → returns `{"injectSteps":[{"systemMessage":{"systemMessage":"[Iris] session <id> registered, peers: [...]"}}]}`. `[MEASURED fixtures/session_start.json]`

**Pre-edit deny:** `PreToolUse` on `write_file|edit_file` → `iris hook` checks claim → returns `{"decision":"deny","reason":"locked by <session>"}`. File is not written. `[MEASURED: IRIS_HOOK_DENY=1 → file not created, model received denial]`

**Session end:** `Stop` hook → `iris hook` releases claims → returns `{}`. `[MEASURED fixtures/stop.json terminationReason="NO_TOOL_CALL"]`

---

## C. Capability matrix

| Capability | Value | Evidence |
|-----------|-------|----------|
| session-start-context | **yes** | `[MEASURED SessionStart.injectSteps systemMessage fires in -p mode]` |
| prompt-context | **yes** | `[MEASURED PreInvocation.injectSteps fires before each model call]` |
| pre-tool-deny | **yes** | `[MEASURED decision:deny → write_file blocked; file not created]` |
| post-tool | **yes** | `[MEASURED PostToolUse fires after every tool]` |
| stop-block | **yes** | `[DOCS Stop decision="continue" re-enters loop; MEASURED Stop fires]` |
| session-end | **yes** | `[MEASURED Stop fires, terminationReason+fullyIdle present]` |
| edit-path-visible | **yes** | `[MEASURED toolCall.args.Path in PreToolUse for write_file]` |
| shell-command-visible | **yes** | `[MEASURED toolCall.args.CommandLine in PreToolUse for run_command]` |
| subagent-events | **partial** | `[MEASURED CLAUDE_DELEGATOR_AGY_DEPTH env key; hooks fire in -p; inner-session hook isolation unverified]` |
| fires-headless | **yes** | `[MEASURED all 6 events fired in --print mode without TTY]` |
| async-handler | **no** | `[DOCS "no async execution"; hooks block agent loop]` |
| idle-wake | **partial** | `[MEASURED agy acp and agy remote-control available; no direct TUI prompt injection]` |

---

## D. Lens findings — PORTABILITY

**D1 Fail-closed by default contradicts P3 (highest priority)**  
Hook non-zero exit blocks the tool — there is no per-hook `on-failure: continue`. `[MEASURED: IRIS_HOOK_FAIL=1 blocked write_file]` If Iris daemon is down, every tool call fails. **`iris hook` must always exit 0, returning `{"decision":"allow"}` on any error.** This must be documented as a hard adapter rule.

**D2 `sh -c` vs `cmd /c` — no quoting bridge**  
Hook commands run via `sh -c` on Unix and `cmd /c` on Windows. `[DOCS hooks.md]` A Windows path with spaces in `iris.exe`'s location will break cmd /c quoting. **`iris integrate` must write OS-appropriate absolute paths** in hooks.json commands; never rely on PATH or `~` expansion (tilde works in sh but not reliably in cmd). `[DOCS: "~ is expanded to the home directory" — only on Unix]`

**D3 Hook CWD is hooks.json directory, not workspace root**  
Relative commands like `./iris` resolve against `.agents/`, not the project root. `[MEASURED cwd=".../workspace/project/.agents"]` **Use absolute paths or require `iris` on PATH in hook commands.**

**D4 SQLite WAL on drvfs/9p mounts**  
agy uses WAL mode itself (`PRAGMA journal_mode = WAL` in binary). `[MEASURED strings]` WAL relies on POSIX advisory locks (`fcntl F_SETLK`) unsupported by 9p/drvfs in WSL2. If a git worktree lives on `/mnt/c/...`, Iris must keep its SQLite database on a Linux native FS (e.g. `~/.local/share/iris/iris.db`). `[UNVERIFIED: test WAL on drvfs; DOCS WSL2 limitations]`

**D5 Loopback reachability across WSL2/Windows boundary**  
In-sandbox loopback works on Linux. `[MEASURED: curl 127.0.0.1:9 from --sandbox succeeded]` In WSL2, `127.0.0.1` does not reach the Windows host. **`iris integrate` must detect `WSL_DISTRO_NAME` env var** `[MEASURED in hook env_keys]` and write the Windows host IP from `/etc/resolv.conf` instead of `127.0.0.1`. Containers need `--network=host` or explicit port mapping.

**D6 PATH differences in GUI-launched agy**  
`agy install` adds the binary to shell profiles. GUI-launched agy may not source `.bashrc`. **`iris integrate` must write absolute paths** (e.g. `/home/user/.local/bin/iris`) in hooks.json, not bare `iris`. `[DOCS agy install --help; UNVERIFIED: test from Antigravity 2.0 desktop app]`

**D7 Windows ACL for bearer token file**  
`chmod 0600` does not enforce Windows ACLs. `[UNVERIFIED: measure on Windows]` **Use Windows Credential Manager or set ACLs explicitly via `icacls`** in the Iris installer. `[DOCS https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/icacls]`

**D8 Antivirus overhead on hook spawn**  
Every `iris hook` spawn on Windows triggers AV scanning. With a 150ms p95 budget and typical Defender latency of 50-100ms per new executable, the budget is tight. `[UNVERIFIED: measure on Windows with Defender]` **Code-sign `iris.exe`** (P12) and have hook delegate to a persistent daemon via loopback HTTP rather than doing real work in the spawned process itself — amortises spawn cost.

**D9 Process-tree join unreliable on Windows**  
No `/proc` on Windows. `cmd /c` adds an intermediary. **Use `ANTIGRAVITY_CONVERSATION_ID` + `ANTIGRAVITY_TRAJECTORY_ID` as the join key** between hooks and MCP sessions — both are injected into hook env and MCP process env. `[MEASURED: both present in env_keys]` Discard pid+start_time for cross-platform join.

---

## E. Verdict on P1 to P12

**P1** AGREE. All three layers (MCP tools, hooks, git guard) are supported and compose correctly. `[MEASURED]`

**P2** AGREE. `type: "command"` is the only hook handler; `iris hook --client agy --event e` via stdin/stdout is the correct universal path. No native HTTP handler exists. `[MEASURED; DOCS "Only type: command is supported"]`

**P3** CHANGE. The timing budget is achievable. But **fail-open is not agy's default** — a crashing `PreToolUse` hook blocks the tool. Replace "fail-open by default" with: **"`iris hook` must catch all errors and always exit 0, returning `{"decision":"allow"}` on failure.**" Fail-closed opt-in can be provided by omitting the try/catch in a stricter hook variant. `[MEASURED]`

**P4** AGREE with caveats. Loopback works from hooks and sandbox. `[MEASURED]` WSL2 boundary and Windows ACLs require platform-specific handling in `iris integrate` (see D5, D7).

**P5** CHANGE. Drop pid+start_time as join key. **Use `ANTIGRAVITY_CONVERSATION_ID` (env + stdin payload) as the primary join key**; `ANTIGRAVITY_TRAJECTORY_ID` as the per-execution secondary key. MCP server is a direct child of agy (same parent PID as hook's grandparent), so both share `conversationId` in env. This is simpler, cross-platform, and already measured. `[MEASURED: conversationId in all hook payloads + mcp_process.json env_keys]`

**P6** AGREE. `agy remote-control start` creates a systemd --user service on Linux. `[MEASURED agy remote-control status]` macOS (launchd) and Windows (Task Scheduler) `[UNVERIFIED — docs expected; pattern confirmed by binary strings]`. Lazy start from hook aligns with P6.

**P7** AGREE. `run_command` PreToolUse only exposes the command string, not which files it will touch. `[MEASURED toolCall.args.CommandLine]` Git status reconciliation post-command is the right approach.

**P8** AGREE. No direct idle TUI injection; ACP is the structured mechanism. `[MEASURED]`

**P9** AGREE. `workspacePaths` array in every hook payload is the raw workspace path. Iris must derive repo identity from git separately; agy provides no normalised repo ID. `[MEASURED fixtures/session_start.json workspacePaths]`

**P10** AGREE. No agy-specific finding changes this.

**P11** AGREE with addendum. `iris integrate` for agy should write to `.agents/plugins/iris/` (project-scope, reversible by deleting directory) or `~/.gemini/config/plugins/iris/` (user-scope). Do not edit `~/.gemini/config/mcp_config.json` directly — the plugin mechanism is cleaner and idempotent. `[MEASURED plugin system works; agy plugin validate passes]`

**P12** AGREE and ESCALATE. Code-signing on Windows is not optional — it directly affects hook latency (D8) and prevents AV quarantine of the hook binary. Notarization on macOS prevents Gatekeeper from blocking `iris hook` during the hook's time budget. These should be on the first-release critical path, not deferred. `[MEASURED; UNVERIFIED on Windows with AV]`

---

## F. From scratch

If designing for agy alone: skip hooks for read-only tool visibility and use MCP only. The model calls `iris.claim(path)` explicitly before writing; `iris.release(path)` after. Use one `PreToolUse` hook matching `write_file|edit_file` as the enforcement backstop — it checks the claim and denies if not held. This reduces hook calls to only write-gating (not every command), cuts latency load by ~80%, requires no shell/quoting gymnastics on Windows, and leverages agy's first-class MCP. The git pre-commit hook remains the hard guard for shell-based edits invisible to the agent.

---

## G. Unknowns

| Unknown | How to verify |
|---------|---------------|
| Fail-open behaviour for SessionStart/Stop/PreInvocation on non-zero exit | Run hook that exits 1 for each; check if session aborts |
| `ANTIGRAVITY_LS_ADDRESS` value and protocol | Print the env var value from a hook |
| Subagent hook isolation (inner agy inheriting outer project hooks?) | Run agy inside run_command; check if outer .agents/hooks.json fires |
| Hook env on Windows (`cmd /c` env inheritance) | Measure on Windows with `echo %ANTIGRAVITY_CONVERSATION_ID%` as hook |
| SQLite WAL on drvfs 9p mount from WSL | Test WAL db at `/mnt/c/...` with concurrent readers |
| Windows token file ACLs | Create file, check with `icacls`, verify user-only |
| ACP wake-up protocol schema | Probe `agy acp` with JSON-RPC `initialize` message |
| MCP server-initiated notifications reaching model | Implement MCP server sending `notifications/message`; observe model |
| `conversationId` stability through compaction | Resume compacted session with `--continue`, compare IDs before/after |
| Hook timing on Windows with Defender | Measure PreToolUse hook round-trip with AV enabled |

## H. Measured on macOS, 1.5.3 (2026-11-07)

Real headless runs (`agy -p … --dangerously-skip-permissions --disable-slash-commands`) against a
local hub on an Intel Mac, with the Iris integration installed by `iris integrate antigravity`.
Another session held an explicit claim on one file.

- **Deny and claims.** A claimed file was refused with "tool call denied by pre-tool hook: Iris: …".
  A free file was claimed automatically.
- **MCP and context.** `whoami` bound by the process ancestor (D6, the nearest-process rung).
  `claims` and `inbox` worked. The SessionStart context reached the model, which reported its Iris
  handle.
- **Failure semantics.** A hook that exits 1 and a missing binary both became the guard's allow.
  A hook **timeout denies**: Antigravity kills the hook ("JSON hook … failed: command failed:
  signal: killed") and fails the tool call, for every tool including MCP tools. With the timeout
  at 10 s, a hook that took 5 s was waited for, and one that slept 15 s was killed after about
  11 s. Iris therefore sets 10 s for Antigravity, well above its own 1.5 s deadline. A hook that
  stalls before that deadline can act, or a hung process, still denies every tool, and no guard
  can change that.
- **PATH.** The earlier guard checked `command -v iris` and ran the recorded path. With the
  client's PATH lacking that directory, every hook was skipped without a trace (#87 fixed it).
- **ack.** With a five-step prompt the model called `ack` with an empty `delivery_id` in every
  run. With a short prompt, the transport carried the right id and the ack succeeded. Iris now
  refuses an empty id with a message that names the field (#90).
- **Not measured:** session end (Antigravity has Stop only), Windows, and hook latency.

## I. Measured on Linux/WSL2, 1.5.8 (2026-11-13)

Clean-environment PTY runs (`agy -i … --model gemini-3.8-flash-low --disable-slash-commands`) with
all outer `ANTIGRAVITY_*` variables stripped before launch:

- **Environment isolation.** Top-level `agy` and `iris mcp --client antigravity` have no
  `ANTIGRAVITY_*` variables in `/proc/<pid>/environ`. All six lifecycle hooks (`SessionStart`,
  `PreInvocation`, `PreToolUse`, `PostToolUse`, `PostInvocation` and `Stop`) receive
  `ANTIGRAVITY_CONVERSATION_ID` alone. Only `run_command` subprocesses receive
  `ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_CSRF_TOKEN`, `ANTIGRAVITY_AGENTAPI_EXE`,
  `ANTIGRAVITY_LS_VERSION`, `ANTIGRAVITY_APP_DATA_DIR`, `ANTIGRAVITY_PROJECT_ID`,
  `ANTIGRAVITY_SOURCE_METADATA`, `ANTIGRAVITY_TRAJECTORY_ID` and `ANTIGRAVITY_AGENT` alongside
  `ANTIGRAVITY_CONVERSATION_ID`.
- **`agy agentapi send-message`.** Dials `ANTIGRAVITY_LS_ADDRESS` with `ANTIGRAVITY_CSRF_TOKEN` and
  accepts `conversationId` (`ANTIGRAVITY_CONVERSATION_ID`). Sent to an idle session, it exited `0`
  in `634–905 ms`, appended a `SYSTEM_MESSAGE` step to `transcript.jsonl` (present on disk by
  `PostInvocation`, not yet in `PreInvocation`), and started a model turn. Sent while `run_command`
  was busy, it exited `0` in `713 ms` without interrupting the tool and was folded into the active
  turn before the next model call. A wrong CSRF token or dead Language Server port exited `1`.
- **`Stop` continuation and sandbox.** Returning `{"decision":"continue","reason":"..."}` from
  `Stop` (`fullyIdle: true`, `terminationReason: "NO_TOOL_CALL"`, `executionNum: 0`) started a
  continuation turn, whereas `continuationActive` remained `false` across `executionNum` `0..4`
  despite the vendor documentation. Under `agy --sandbox`, `run_command` runs in a PID, mount and
  network namespace where `~/.config/iris` is absent and `127.0.0.1:<LS_PORT>` refuses connections;
  setting `BypassSandbox: true` with an exact `command(...)` allow rule in
  `~/.gemini/antigravity-cli/settings.json` ran unsandboxed without prompting on a personal account
  ([ADR 028](../../adr/028-antigravity-idle-wake.md)).
