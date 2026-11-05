# cursor-agent report (2026.11.05-86047df)

Measured on Linux amd64 Free plan (`cursor-agent about`: Subscription Tier Free; binary `/home/<USER>/.local/bin/cursor-agent` → `…/versions/2026.11.05-86047df/cursor-agent`). Alias `agent` is the same binary. [MEASURED: `cursor-agent --version`; `cursor-agent about`]

**CLI vs IDE:** Facts below are for headless `cursor-agent` unless marked IDE/docs-only. A hook that exists only in the IDE must not be assumed in the CLI adapter.

## A. Facts

### A1 Hooks

Hooks exist. Config is JSON `hooks.json` with `version: 1` and a `hooks` map of event → `[{command, timeout?, matcher?, failClosed?, type?}]`. [DOCS https://cursor.com/docs/hooks]

**Locations / precedence** (highest → lowest): Enterprise MDM → Team (Enterprise cloud) → Project `/.cursor/hooks.json` → User `~/.cursor/hooks.json`; then plugin hooks. [DOCS https://cursor.com/docs/hooks]

| OS | Enterprise path |
| --- | --- |
| Linux/WSL | `/etc/cursor/hooks.json` [DOCS] |
| macOS | `/Library/Application Support/Cursor/hooks.json` [DOCS] |
| Windows | `C:\ProgramData\Cursor\hooks.json` [DOCS] |

User: `~/.cursor/hooks.json` (scripts cwd `~/.cursor/`). Project: `<repo>/.cursor/hooks.json` (scripts cwd = project root; paths like `.cursor/hooks/…`). [DOCS]

**Handler types:** `command` (default) and `prompt` (LLM). Plugin hooks: command only. [DOCS]

**Matchers:** regex; tool hooks match tool type (`Shell`, `Write`, `MCP:<name>`, …); shell hooks match command text. [DOCS]

**Agent events (docs):** `sessionStart`, `sessionEnd`, `preToolUse`, `postToolUse`, `postToolUseFailure`, `subagentStart`, `subagentStop`, `beforeShellExecution`, `afterShellExecution`, `beforeMCPExecution`, `afterMCPExecution`, `beforeReadFile`, `afterFileEdit`, `beforeSubmitPrompt`, `preCompact`, `stop`, `afterAgentResponse`, `afterAgentThought`. Tab-only: `beforeTabFileRead`, `afterTabFileEdit` (IDE Tab). App: `workspaceOpen`. [DOCS https://cursor.com/docs/hooks]

**Headless CLI measured** (`cursor-agent -p --force --trust --approve-mcps --sandbox disabled --model auto`): fired `sessionStart`, `sessionEnd`, `preToolUse`, `postToolUse`, `postToolUseFailure`, `beforeShellExecution`, `afterShellExecution`, `beforeReadFile`, `afterFileEdit`, `beforeMCPExecution`, `afterMCPExecution`, `afterAgentThought`. Did **not** fire in these runs: `stop`, `beforeSubmitPrompt`, `afterAgentResponse`, `subagentStart`/`Stop`, `preCompact`, `workspaceOpen`. Fixtures under `fixtures/`. [MEASURED]

**Stdin:** JSON including common fields `conversation_id`, `generation_id`, `model`, `hook_event_name`, `cursor_version`, `workspace_roots`, `user_email`, `transcript_path`, plus event-specific fields (`tool_name`/`tool_input`, `command`, `file_path`, …). [MEASURED: `fixtures/sessionStart.json`, `fixtures/preToolUse.json`, `fixtures/beforeShellExecution.json`] [DOCS]

**Outputs:**
- Permission hooks (`preToolUse`, `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile`, …): `{permission:"allow"|"deny"|"ask", user_message?, agent_message?, updated_input?}`. Exit `2` ≡ deny. Invalid JSON on permission hooks blocks. Other non-zero exits fail-open unless `failClosed: true`. [DOCS]
- `sessionStart`: `{additional_context?, env?}` (fire-and-forget; `continue:false` not enforced). [DOCS] `env` injection observed on later hooks (`IRIS_HOOK_PROBE`). [MEASURED]
- `stop` / `subagentStop`: `{followup_message?}` (docs; `stop` not seen in CLI print runs). [DOCS]
- `postToolUse`: `{additional_context?, updated_mcp_tool_output?}`. [DOCS]

**Timeouts:** per-hook `timeout` seconds; default “platform default”. [DOCS] Async: no native async hook type; process must finish. [DOCS]

**Deny proof:** `preToolUse` + `permission:deny` on `Write` → `postToolUseFailure` `failure_type=permission_denied`, file unchanged (`fixtures/deny-write.json`, `fixtures/deny-target.txt` still `seed`). [MEASURED]

**Trust:** Project hooks require trusted workspace. Headless without `--trust` hung until timeout (exit 124); with `--trust` hooks ran. [MEASURED] Docs: project hooks run in trusted workspaces. [DOCS]

**Subagents:** docs + changelog say subagent hooks exist; not exercised here. [DOCS] [UNVERIFIED for this binary without a Task-tool run]

**IDE-only risk:** Tab hooks and some prompt-lifecycle hooks (`beforeSubmitPrompt`, possibly `stop`/`afterAgentResponse` in `-p`) must be treated as partial. Cloud agents omit `sessionStart`/`sessionEnd`/`beforeMCPExecution` per docs. [DOCS]

### A2 Spawn mechanics

**Hooks:** spawned as processes; on this Linux CLI the hook’s immediate parent is `/bin/bash` (wrapper), not `cursor-agent`. [MEASURED: `fixtures/log.jsonl` `parent_is_bash`] Desktop Windows: hooks via PowerShell, JSON on stdin/`$input`. [DOCS https://cursor.com/docs/hooks] cwd: project root for project hooks. [DOCS] [MEASURED]

**Hook env (names):** includes `CURSOR_PROJECT_DIR`, `CURSOR_VERSION`, `CURSOR_USER_EMAIL`, `CURSOR_CONVERSATION_ID`, `CURSOR_AGENT`, plus sessionStart-injected keys. [MEASURED] Docs also list `CURSOR_TRANSCRIPT_PATH`, `CLAUDE_PROJECT_DIR`, `CURSOR_PLUGIN_ROOT`. [DOCS]

**MCP stdio:** direct child of `cursor-agent`; env sparse (`HOME`,`PATH`,`SHELL`,`TERM`,`USER`,`LOGNAME`,`LC_CTYPE` only in probe). No `CURSOR_*`. [MEASURED: `fixtures/mcp_spawn.json`]

**Sandbox:** `--sandbox enabled|disabled`; hooks themselves are host processes; shell tool may be sandboxed (`sandbox` bool on shell hook payload). [MEASURED: `beforeShellExecution.json`] [DOCS https://cursor.com/docs/cli/reference/parameters]

### A3 MCP

**Transports:** stdio, SSE, streamable HTTP. [DOCS https://cursor.com/docs/mcp]

**Config:** project `.cursor/mcp.json`, user `~/.cursor/mcp.json`; interpolation `${env:}`, `${workspaceFolder}`, `${userHome}`, … [DOCS] CLI discovers same files. [DOCS https://cursor.com/docs/cli/mcp] [MEASURED: project mcp listed/used]

**Non-interactive:** `agent mcp enable|disable|list|list-tools|login`; run-time `--approve-mcps`. [MEASURED: `cursor-agent mcp --help`] Global `~/.cursor/mcp.json` servers auto-approved; project servers need approval. [DOCS changelog https://cursor.com/docs/cli/changelog]

**Tools:** appear as `MCP:<server_tool>` in `preToolUse`. [MEASURED] No Iris-specific auto-approval beyond enable/`--approve-mcps` / allowlists. [DOCS]

**Server→model:** tools/resources/prompts/elicitation/apps supported in Cursor MCP docs; elicitation is user-facing. [DOCS] Reachability into `-p` model loop for notifications: [UNVERIFIED]

### A4 Packaging

Cursor Plugins (`.cursor-plugin/plugin.json`) can bundle hooks, MCP, rules, skills, agents, commands. [DOCS https://cursor.com/docs/reference/plugins] CLI: `--plugin-dir`, `agent plugin marketplace add|list|…`. [MEASURED] Auto-loading instruction files: `.cursor/rules`, `AGENTS.md`, `CLAUDE.md`. [DOCS https://cursor.com/docs/cli/using]

### A5 Wake-up

- `--resume [chatId]`, `--continue`, `agent resume`, `agent ls`. [MEASURED: `--help`]
- `agent create-chat` returns a new id. [MEASURED]
- `agent persist` / `persist attach|stop` for disconnect-surviving sessions. [MEASURED: `persist --help`]
- `agent acp` — ACP server over stdio (hidden). [MEASURED] [DOCS https://cursor.com/docs/cli/using]
- `stop` hook `followup_message` (docs; not seen in `-p`). [DOCS]
- Desktop notifications when turn finishes / blocked (CLI changelog). [DOCS changelog]
- No keystroke injection API found in `--help`. [MEASURED]

### A6 Session identity

`conversation_id` ≡ `session_id` (UUID) on hooks; stable for the run; also in `-p` JSON `session_id`. [MEASURED] `generation_id` present (same as conversation at sessionStart in our captures). Resume uses chat id via `--resume`. Subagent ids: `subagent_id` / separate transcript path in docs. [DOCS] Compaction: `preCompact` (not seen here). [DOCS]

## B. Minimal Iris integration

**Primary path (user-level, fewest prompts)** — Short:

1. Ensure `iris` on `PATH`.
2. Write `~/.cursor/hooks.json` (backup first) so every needed event runs one command, e.g. `iris hook --client cursor-agent` (adapter reads `hook_event_name` from stdin; optional `--event` override).
3. Write `~/.cursor/mcp.json` entry `iris` → `iris mcp` (stdio) so MCP auto-approves. [DOCS changelog]
4. `iris doctor` canary: `cursor-agent -p --trust --force --approve-mcps --sandbox disabled --model auto "…"`.

**Project-level (team repo)** — Medium:

`.cursor/hooks.json` + `.cursor/mcp.json` as above with commands `.cursor/…` or absolute `iris`. Requires workspace trust (`--trust` in CI/headless) and `agent mcp enable iris` or `--approve-mcps` once per machine. [MEASURED] [DOCS]

**Event mapping for Iris:**

| Iris need | cursor-agent hook |
| --- | --- |
| Session bind + inbox digest | `sessionStart` → `additional_context` + `env` (e.g. execution id) |
| Pre-edit claim / deny | `preToolUse` matcher `Write\|Delete` (path in `tool_input.file_path`); also `beforeShellExecution` for shell mutators |
| Post-edit observe | `afterFileEdit` (`file_path`, `edits`) |
| Shell visibility | `beforeShellExecution` / `afterShellExecution` |
| Session end / release | `sessionEnd` (CLI); do not rely on `stop` in `-p` |
| MCP tool gate | `beforeMCPExecution` / `preToolUse` `MCP:…` |

There is **no** `beforeFileEdit`; denying writes via `afterFileEdit` is too late. [DOCS] [MEASURED]

**Exact skeleton (project):**

```json
{
  "version": 1,
  "hooks": {
    "sessionStart": [{ "command": "iris hook --client cursor-agent" }],
    "sessionEnd": [{ "command": "iris hook --client cursor-agent" }],
    "preToolUse": [{ "command": "iris hook --client cursor-agent", "matcher": "Write|Delete|Shell" }],
    "beforeShellExecution": [{ "command": "iris hook --client cursor-agent" }],
    "afterFileEdit": [{ "command": "iris hook --client cursor-agent" }],
    "postToolUse": [{ "command": "iris hook --client cursor-agent" }],
    "postToolUseFailure": [{ "command": "iris hook --client cursor-agent" }]
  }
}
```

```json
{
  "mcpServers": {
    "iris": { "command": "iris", "args": ["mcp"] }
  }
}
```

Windows note: prefer a real executable on `PATH` over `.sh`; desktop hooks run through PowerShell. [DOCS]

## C. Capability matrix

| Capability | Value | Evidence |
| --- | --- | --- |
| session-start-context | yes | [MEASURED] `sessionStart` + `additional_context`/`env` |
| prompt-context | partial | [DOCS] `beforeSubmitPrompt`; not seen in `-p` [MEASURED] |
| pre-tool-deny | yes | [MEASURED] `fixtures/deny-write.json` |
| post-tool | yes | [MEASURED] `postToolUse` / `afterFileEdit` / shell after |
| stop-block | partial | [DOCS] `stop`/`beforeSubmitPrompt`; absent in measured `-p` |
| session-end | yes | [MEASURED] `sessionEnd` |
| edit-path-visible | yes | [MEASURED] Write `tool_input.file_path`; `afterFileEdit.file_path` |
| shell-command-visible | yes | [MEASURED] `beforeShellExecution.command` |
| subagent-events | partial | [DOCS] yes; [UNVERIFIED] this run |
| fires-headless | yes | [MEASURED] with `--trust` |
| async-handler | no | [DOCS] sync process only |
| idle-wake | partial | [MEASURED] resume/persist/acp; no push into live idle TTY |

## D. Lens findings (FIRST-RUN UX)

1. **Seamlessness breaks at trust + MCP approval, not at binary install.** Package install of `iris` can be invisible; `iris integrate cursor-agent` writing **user** `~/.cursor/hooks.json` + `~/.cursor/mcp.json` avoids project trust and project MCP prompts. Project-only integrate feels broken until the human trusts the folder and approves MCP. **Rec (Short):** default integrate → user scope; offer `--project` for repos. Fail doctor if `cursor-agent -p` without `--trust` hangs.

2. **Installer questions that earn their keep:** (a) hub URL / “this machine is hub?”, (b) fail-open vs fail-closed for claims, (c) which detected clients to enable. **Never ask:** Cursor API key (use existing `agent login`), model name, email/SMTP, per-event toggles on first run.

3. **Two-vendor collision demo (minute 4–5):** Agent A (cursor-agent) `sessionStart` shows digest; `preToolUse Write` claims file; Agent B (other CLI) denied with `user_message`/`agent_message` naming holder + how to wait. If daemon down: hooks fail-open by default (Cursor already fail-opens on hook crash) but MCP tools error loudly — doctor must distinguish “hooks ok, daemon down”. **Rec (Medium):** typed errors: `iris: daemon unreachable (loopback http://127.0.0.1:…)`, `iris: cursor-agent not on PATH`, `iris: hooks.json schema unknown keys (client updated?)` with `iris integrate --repair`.

4. **Web UI first screen:** empty state = “Install a client hook” checklist with copy-paste `iris integrate --all` and a live “waiting for first sessionStart…” indicator, not a blank agent table. Uninstall: `iris integrate cursor-agent --remove` restores backed-up hooks/mcp fragments only (never wipe whole `hooks.json`), then `iris service uninstall`.

5. **CLI/IDE split is a silent footgun.** Shipping an Iris feature on `beforeSubmitPrompt` or Tab hooks “works in Cursor” and does nothing in `cursor-agent -p`. **Rec (Quick):** capability matrix in doctor output per client binary, not per vendor brand.

## E. Verdict on P1–P12

- **P1 AGREE** — MCP + hooks + git guard fits cursor-agent; pre-edit is `preToolUse`/`beforeShellExecution`, not a dedicated before-edit file hook.
- **P2 AGREE** — Keep `iris hook --client cursor-agent`; prefer deriving event from stdin `hook_event_name` so one command covers all events.
- **P3 AGREE** — Matches Cursor fail-open default; use Cursor `failClosed` only when Iris repo opt-in is on.
- **P4 AGREE** — Loopback HTTP is fine; hooks can call it (host process). Avoid depending on MCP env for auth (sparse env).
- **P5 CHANGE** — Do **not** join primarily by walking to client pid/start time: hooks are bash-wrapped; MCP is a direct child with almost no env. **Replacement:** bind on `conversation_id`/`session_id` from hook JSON; have `sessionStart` set `env` execution id for later hooks; pass the same id into MCP tools as an argument or via Iris-side session table keyed by workspace+time as fallback.
- **P6 AGREE** — Single-host `iris serve` enough; lazy start from hook on daemon miss.
- **P7 AGREE** — Shell writes visible to shell hooks; still reconcile + git guard.
- **P8 CHANGE** — For this client: resume/persist/ACP + next-turn `additional_context` / optional `followup_message`; desktop notification; no keystroke injection. Do not design as if idle CLI can be poked in-place without resume.
- **P9 AGREE** — Path identity as drafted; hook payloads give absolute paths to normalise.
- **P10 AGREE** — Inject digests as delimited data in `additional_context`.
- **P11 CHANGE** — Two steps stay; but for Cursor, **user-level** hooks+MCP is the seamless default; project-level is opt-in because of trust/MCP approval. Doctor must pass `--trust`/`--approve-mcps` in canaries.
- **P12 AGREE** — Prefer `irisd` / unambiguous package names given Homebrew/Scoop collisions noted in the brief.

## F. From scratch

Ship a Cursor Plugin plus user-level integrate as the default (hooks+MCP+alwaysApply rule pointing agents at Iris tools). Key Executions by `conversation_id`, not process ancestry. Treat `cursor-agent` and the IDE as two capability profiles behind one `--client cursor`. Skip building on `stop`/`beforeSubmitPrompt` until doctor proves them on the installed CLI. Prefer fail-open hooks with a loud web-UI “unprotected session” badge over bricking Free-plan CLI users.

## G. Unknowns

- Whether `stop`, `afterAgentResponse`, `beforeSubmitPrompt` fire in interactive CLI (not `-p`) — verify with a PTY harness.
- Subagent hook payloads on this build — run a Task-tool prompt.
- Windows PowerShell hook quoting and `.exe` PATH behaviour — need a Windows host [DOCS only here].
- Whether plugin-installed hooks load in headless CLI the same as IDE — try `--plugin-dir` canary.
- MCP elicitation / server notifications inside `-p` — protocol probe.
- Enterprise team hooks sync on CLI-only machines — [DOCS] implies desktop sync; CLI [UNVERIFIED].
