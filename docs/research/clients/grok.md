# Grok CLI report (1.0.77)

> Run in two turns; the first hit a 20-minute deadline after the experiments were complete.

Binary `grok 1.0.77 (50ca579fa996) [stable]` [MEASURED `grok --version`]. Vendor docs for this build are `~/.grok/docs/user-guide/`. Payloads are in `fixtures/`. Trust used an isolated `GROK_HOME`. User config was not edited.

## A. Facts

### A1 Hooks

Hooks exist and follow Claude's shape [DOCS ~/.grok/docs/user-guide/10-hooks.md]. Measured, every one a direct child of `grok`: `SessionStart` (`source: new`), `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop` (`end_turn`, then `shutdown`), `SessionEnd` (`reason: shutdown`), `SubagentStart`, `SubagentStop`, and a child `SessionEnd` with `subagentType` [MEASURED `fixtures/log.jsonl`]. Also documented, not fired here: `PostToolUseFailure`, `PermissionDenied`, `Notification`, `StopFailure`, `StopCancelled`, `PreCompact`, `PostCompact`. `SessionStart` does not fire for a subagent and compaction fires only the compact events. Its matcher is `new` or `load`, not Claude's `startup`/`resume`/`clear`/`compact` [DOCS same].

Load order [DOCS same; DOCS ~/.grok/docs/user-guide/26-config-reference.md]: `~/.grok/hooks/*.json`; `~/.claude/settings.json` and `settings.local.json` (`compat.claude.hooks` default true, `GROK_CLAUDE_HOOKS_ENABLED`); `~/.cursor/hooks.json`. Project, git root down to cwd: `.grok/hooks/*.json`, `.claude/settings.json` (+ `.local`), `.cursor/hooks.json`. Also `~/.grok/config.toml`, managed `managed_config.toml` and signed `requirements.toml` (`$GROK_HOME` and `/etc/grok`; Windows has no `/etc/grok`), and plugin `hooks/hooks.json`. Merge is additive. `allow_managed_hooks_only` / `allowManagedHooksOnly` (including Claude `managed-settings.json`) is tighten-only and drops user, project, plugin, and compat hooks.

Handlers are `command` and `http`. `async`, `asyncRewake`, `shell`, `statusMessage`, and `headers` load and run in the foreground [DOCS 10-hooks.md]. HTTP is POST JSON to `https`, or to loopback `http` (`127.0.0.0/8`, `::1`, `localhost`). Other non-public ranges are refused, redirects are not followed, and loopback bypasses the proxy. Four `SessionStart` POSTs came from `127.0.0.1` with `accept`, `accept-encoding`, `content-length`, `content-type`, `host` [MEASURED `fixtures/http-SessionStart.json`].

Stdin is camelCase plus Claude snake_case (`session_id`, `tool_name`, `tool_input`, `hook_event_name`). Shared fields include `sessionId`, `cwd`, `workspaceRoot`, `timestamp`, `permissionMode` [MEASURED `fixtures/SessionStart.json`, `fixtures/PreToolUse.json`]. Every command hook receives `GROK_HOOK_EVENT` (snake_case, e.g. `pre_tool_use`), `GROK_HOOK_NAME`, `GROK_SESSION_ID`, `GROK_WORKSPACE_ROOT`, `CLAUDE_PROJECT_DIR` [MEASURED `fixtures/log.jsonl`].

Decisions [DOCS 10-hooks.md]: exit 0 allows; exit 2 denies `PreToolUse` (stderr is the reason if JSON has none); other codes fail open unless the JSON denies or the handler sets `failClosed`. `PreToolUse` also honors `ask` (ignored under `bypassPermissions`), `defer`, `updatedInput`, `additionalContext`, and `continue: false`. `PostToolUse` cannot block the call. `Stop` can block `end_turn` (8 continuations). The exit-time Stop (`shutdown` or `channel_closed`, 1.5s) ignores its decision. An allowing `UserPromptSubmit` drops stdout and `additionalContext`. `SessionStart` reads only `systemMessage`.

Defaults: 5s, or 600s for `Stop`/`SubagentStop`/`PostToolUse`, 30s for `UserPromptSubmit`, 1.5s for `SessionEnd`. `timeout: 0` keeps the default. Timeout, crash, and bad JSON fail open. An explicit deny, an oversized reply (8 MiB / 524288 values), a bad `updatedInput`, and `failClosed` / `onFailure: block` still deny. `failClosed` applies only to `PreToolUse` and `UserPromptSubmit`. Stop cannot fail closed.

Headless `grok -p` fires hooks [MEASURED]. Project hooks need `grok --trust` or `/hooks-trust`, recorded in `$GROK_HOME/trusted_folders.toml` (mode 0600). Until then they are skipped. Headless counts the folder trusted for the user's own hooks; project hooks still need trust [DOCS 10-hooks.md]. An isolated `~/.claude/settings.json` `SessionStart` ran at `permissionMode: default` while `projectTrusted` was false and the project log was empty [MEASURED `fixtures/claude-canary-untrusted.json`]. After `--trust`, project hooks ran in this `/tmp` repo [MEASURED `fixtures/log.jsonl`]. The doc also says a repo rooted at `/tmp`, or a launch in `/tmp`, loads no project hooks. This binary did not suppress them once trusted.

Deny: the hook printed `{"decision":"deny","reason":"iris-probe denied this edit"}` and exited 2. `PreToolUse` for tool `write` was denied under `--always-approve` (`permissionMode: bypassPermissions`). No `PostToolUse` followed. Stdout mentioned the hook and the block. `note.txt` was absent. The grok process exited 0 [MEASURED `fixtures/PreToolUse-deny.json`, `fixtures/outcomes.json`]. A later run wrote `note.txt` containing `hello` [MEASURED `fixtures/PostToolUse.json`]. A timeout was not induced. By the doc, a crash or timeout lets the write proceed unless that handler sets `failClosed`.

### A2 Spawn mechanics

Without `args`, Unix uses `sh -c` and Windows uses pwsh, Windows PowerShell, or Git Bash [DOCS 10-hooks.md]. An `args` array, including `[]`, execs `command` with no shell. A direct script needs a shebang. This probe's absolute path had parent `comm=grok` [MEASURED]. Cwd was the workspace. The hook saw 70 environment names (parent plus the five contract names). The project stdio MCP child saw 67, the full parent set, including credential-shaped names, plus `GROK_SESSION_ID`, and not the hook-only names [MEASURED `fixtures/mcp-process.json`]. `shell_environment_policy` scrubs bash, not stdio MCP [DOCS 26-config-reference.md].

Hook and MCP were direct children of the same grok pid and start ticks (allow2: both ppid 787984). A further ancestor was a different grok [MEASURED `fixtures/outcomes.json`]. `~/.grok/leader.sock` and `leader.sock.daemon` were absent [MEASURED `ls`]. Leader mode is opt-in [DOCS ~/.grok/docs/user-guide/03-keyboard-shortcuts.md].

Sandbox is off by default: Landlock and seccomp on Linux, Seatbelt on macOS, where child-network blocking is a no-op. In-process HTTP hooks still send HTTPS under `restrict_network`; plain loopback `http` outside managed policy is then refused [DOCS ~/.grok/docs/user-guide/18-sandbox.md, 10-hooks.md]. This run left the sandbox off.

### A3 MCP

Transports: stdio, streamable HTTP, SSE [DOCS ~/.grok/docs/user-guide/07-mcp-servers.md]. User `~/.grok/config.toml`; project `<repo>/.grok/config.toml` (cwd to git root; same name replaced wholly). Add with `grok mcp add --scope user|project -t stdio|http|sse`. `${VAR}` expands. `bearer_token_file` is absolute or `~/`. OAuth sits in `~/.grok/mcp_credentials.json` mode 0600 [DOCS same; DOCS ~/.grok/docs/user-guide/02-authentication.md]. Startup 30s, tool timeout 6000s, output cap 20000 bytes. Catalog key `server__tool`; the model calls `search_tool` / `use_tool`. Claude `~/.claude.json` (`compat.claude.mcps` default on), then Cursor `mcp.json` and project `.mcp.json`. Priority: config.toml, Claude, Cursor, `.mcp.json`. Approve with `MCPTool(server__*)` [DOCS ~/.grok/docs/user-guide/22-permissions-and-safety.md]. Headless without `--always-approve` refuses a prompted tool and exits 0 [DOCS 14-headless-mode.md]. `--always-approve` still honors a hook deny [MEASURED]. Untrusted inspect listed `iris_probe` (hook count 0) and named `$GROK_HOME/config.toml`, which did not define it; the project file did [MEASURED `fixtures/outcomes.json`]. The child appeared on trusted runs. Server notifications and elicitation were not exercised [UNVERIFIED].

### A4 Packaging

Plugins bundle skills, commands, agents, `hooks/hooks.json`, `.mcp.json`, and `.lsp.json`, and cannot ship a native binary [DOCS ~/.grok/docs/user-guide/09-plugins.md]. Discovery includes `.claude/plugins/`. User plugins are auto-trusted; project plugins need trust. Fake-home inspect had Claude and Cursor skills, rules, agents, MCP, hooks, and sessions, plus Codex sessions, all default-on [MEASURED]. Trusted projects also load `AGENTS.md`, `CLAUDE.md`, and the `.grok`/`.claude`/`.cursor` rule dirs [DOCS ~/.grok/docs/user-guide/12-project-rules.md, 08-skills.md]. Claude hooks, plugins, MCP, skills, and rules therefore run inside grok until `compat.claude.*` or `GROK_CLAUDE_*` is turned off. Real-home inspect, folder untrusted, listed plugin hook files and Claude MCP servers, including one HTTP server. `~/.claude/settings.json` (mode 0600) holds SessionStart ×2, PreToolUse ×1, PostToolUse ×2, SessionEnd ×1, SubagentStart ×1, SubagentStop ×1; those handlers were not in the untrusted inspect list and were not executed [MEASURED counts]. The canary shows that user-hook path does run.

### A5 Wake-up

No keystroke injection. `grok -p -r <id>` resumes in a new process [DOCS 17-sessions.md]. ACP: `grok agent stdio` and `grok agent serve --bind 127.0.0.1:2419 --secret` [DOCS ~/.grok/docs/user-guide/15-agent-mode.md]. `features.auto_wake` defaults true for grok's own background work; `active_agent_messages` defaults false [DOCS 26-config-reference.md]. Notifications only poke the UI [DOCS 05-configuration.md]. `/remote` was not run [DOCS 14-headless-mode.md].

### A6 Session identity

Stdin `sessionId`, `GROK_SESSION_ID`, and the directory `$GROK_HOME/sessions/<encoded-cwd>/<id>/` are the same id [MEASURED]. `summary.json` has no session-id field; it does have `agent_id`, `attempt_id`, `git_root_dir`, `head_commit` [MEASURED before the isolated home was removed]. Grok mints a UUIDv7. `-s` creates and errors if the id exists. `-r`/`-c` resume. `--fork-session` mints a new id [DOCS ~/.grok/docs/user-guide/17-sessions.md]. Resume was not re-run. Do not truncate ids; nearby ones share a long prefix. One headless run had two ids, parent and subagent, same grok pid, with `subagentId` and `subagentType: general-purpose` [MEASURED `fixtures/SubagentStart.json`]. `SubagentStop.phase` was `gate`.

## B. Minimal Iris integration

Direct-exec `iris hook` in `<repo>/.grok/hooks/iris.json`, plus project stdio `iris mcp`, joined on `GROK_SESSION_ID`. Explicit timeout. `failClosed` only if the repo opts in. Git guard is the hard stop. Adapter: Short. Doctor trust check: Quick.

Linux and macOS: `~/.grok`. Windows: `%USERPROFILE%\.grok` [DOCS 05-configuration.md], no `/etc/grok` [DOCS 10-hooks.md]. `args` skips the shell:

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "/usr/local/bin/iris", "args": ["hook", "--client", "grok", "--event", "SessionStart"], "timeout": 3}]}],
    "PreToolUse": [{"matcher": "write|search_replace|Edit|Write|MultiEdit|hashline_edit", "hooks": [{"type": "command", "command": "/usr/local/bin/iris", "args": ["hook", "--client", "grok", "--event", "PreToolUse"], "timeout": 3, "failClosed": false}]}],
    "PostToolUse": [{"hooks": [{"type": "command", "command": "/usr/local/bin/iris", "args": ["hook", "--client", "grok", "--event", "PostToolUse"], "timeout": 3}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "/usr/local/bin/iris", "args": ["hook", "--client", "grok", "--event", "SessionEnd"], "timeout": 1}]}]
  }
}
```

`SessionStart` returns `{"systemMessage":"..."}` [DOCS 10-hooks.md]. Deny with `{"decision":"deny","reason":"..."}` and exit 2. Measured edit tool is `write` (`file_path`, `content`) [MEASURED `fixtures/PreToolUse.json`]; also match `Edit`/`Write`/`search_replace`. Ignore `Stop` `reason: shutdown`. Shell is `run_terminal_command` plus `command`, no parsed paths [MEASURED `fixtures/PreToolUse-shell.json`]. Reconcile with `git status`.

Project `<repo>/.grok/config.toml`:

```toml
[mcp_servers.iris]
command = "/usr/local/bin/iris"
args = ["mcp"]
enabled = true

[permission]
allow = ["MCPTool(iris__*)"]
```

Set a minimal `env`; the child otherwise inherits the parent [MEASURED]. The folder stays inert until `grok --trust`. Doctor reads `grok inspect --json` and reports `projectTrusted: false`. Write grok files only, because a Claude user hook double-fires. Loopback HTTP posts the whole tool input, so it stays optional [MEASURED `fixtures/http-SessionStart.json`].

## C. Capability matrix

- session-start-context: partial. Return `systemMessage` [DOCS 10-hooks.md]. The inbound payload has no context field [MEASURED]. Model injection was not re-tested.
- prompt-context: no. Allowing `UserPromptSubmit` stdout is discarded [DOCS].
- pre-tool-deny: yes. Exit 2 and `decision: deny` blocked `write` under `--always-approve` [MEASURED]. Timeout and crash fail open unless `failClosed` [DOCS].
- post-tool: yes, observe-only [MEASURED]. Cannot block the call [DOCS].
- stop-block: partial. `end_turn` can block [DOCS]. A block was not returned. `shutdown` is observe-only [MEASURED `fixtures/Stop-shutdown.json`].
- session-end: yes [MEASURED `fixtures/SessionEnd.json`]. Cannot block [DOCS].
- edit-path-visible: yes. Tool `write`, key `file_path` [MEASURED].
- shell-command-visible: yes. Tool `run_terminal_command`, key `command` [MEASURED]. Paths inside the string are not parsed.
- subagent-events: yes [MEASURED `fixtures/SubagentStart.json`].
- fires-headless: yes after trust for project hooks. User Claude hooks fire headless without project trust [MEASURED].
- async-handler: no. Those fields run in the foreground [DOCS].
- idle-wake: partial. Resume by id, ACP serve, and grok's own `auto_wake` [DOCS]. No push into an idle TUI was measured.

## D. Lens findings

SECURITY, by practical impact. One control each.

1. Malicious repo hook or MCP. `grok --trust` enables project hooks, MCP, instructions, and skills together [DOCS 10-hooks.md], and untrusted inspect still lists the MCP server [MEASURED]. Control: never auto-trust; doctor fails the folder while `projectTrusted` is false. Enterprise pin: `allow_managed_hooks_only` plus signed `requirements.toml`.

2. Claude cross-load. Headless grok runs `~/.claude/settings.json` hooks with the project untrusted, and loads Claude plugins, skills, rules, and MCP [MEASURED; DOCS 26-config-reference.md]. A Claude user hook installed for Iris would also run here and double-fire. Control: write only `.grok/hooks/*.json` and project `.grok/config.toml`, and have doctor name `GROK_CLAUDE_HOOKS_ENABLED=0` and `GROK_CLAUDE_MCPS_ENABLED=0`.

3. Prompt injection. `systemMessage`, `additionalContext`, and `updatedToolOutput` reach the model, and a Stop reason comes back as a user message [DOCS 10-hooks.md]. Stdio MCP inherits credential-shaped parent env names [MEASURED `fixtures/mcp-process.json`]. Control: delimit digests with a size cap and sender id, give Iris MCP a minimal env, and keep digest text out of Stop reasons.

4. Identity spoofing. Hook and stdio child both see `GROK_SESSION_ID`. The nearest grok pid matched; the ancestor walk found a second grok [MEASURED]. Subagents share the pid. Control: session id plus a daemon nonce, one Execution per subagent id. Pid plus start time is only the nearest-client fallback.

5. Fail-open bypass. A timed-out or crashed `PreToolUse` allows the tool [DOCS 10-hooks.md]. An explicit deny still held under `--always-approve`, and the process exited 0 [MEASURED; DOCS 14-headless-mode.md]. Truncation can hide an input suffix past 128 KiB [DOCS]. Control: 2–3s handler timeout, opt-in `failClosed` on `PreToolUse` only, git guard as the hard stop, and doctor ignores exit 0.

6. Loopback CSRF, DNS rebinding, and the wake socket. Grok's HTTP hooks and `grok agent serve` bind loopback with a secret, skip the proxy, and do not follow redirects [DOCS 10-hooks.md, 15-agent-mode.md; MEASURED]. Claude Code Monitor accepts only a url and subprotocols, so that value is model-visible. It must be a wake-only ticket: random, one Execution, expired with it, unable to grant claims or call any other route. Put it in a subprotocol, not a query string. Bind `127.0.0.1`, check `Host` and `Origin`, no cookie. Clients that can set `Authorization` or grok `--secret` keep the daemon bearer in that header.

7. Token storage. Unix `auth.json` and `trusted_folders.toml` are mode 0600 [MEASURED; DOCS 02-authentication.md]. Windows and macOS ACLs are [UNVERIFIED]. Control: 0600 bearer file on Linux; DPAPI and Keychain later; no tokens in hook env.

8. Claim exhaustion. One held claim set is a daemon lease bug, and a Stop hook can stall for 600s [DOCS 10-hooks.md]. Control: claim TTL, and a 2–3s timeout on every Iris handler.

9. Hub-to-host. No cross-machine grok hub was measured; `/remote` is docs only [DOCS 14-headless-mode.md]. Control: authenticate the host. A remote session id grants nothing.

10. Installer. Grok installs with `curl -fsSL https://x.ai/cli/install.sh | bash` [DOCS ~/.grok/README.md]. Control: package managers and signed releases. `iris integrate` writes config only.

## E. Verdict on P1 to P12

P1. AGREE. MCP, hooks, and a git guard. Hooks fail open, so the guard is the hard stop.

P2. AGREE. One `iris hook --client grok --event` via direct `args` exec. HTTP loopback stays optional.

P3. CHANGE. Keep the p95 budget as an Iris target. Set `timeout` yourself: grok's defaults are 5s and 600s, and `0` keeps them. Fail-closed is per-handler `failClosed` on `PreToolUse` and `UserPromptSubmit` only.

P4. AGREE. Loopback HTTP plus a bearer file. The wake socket uses a separate wake-only ticket (D6).

P5. CHANGE. Join grok on `GROK_SESSION_ID`, which both the hook and the stdio child receive [MEASURED]. Claude Code and Copilot do the same; Cursor and Antigravity put the id on hooks only, so they keep a nearest-pid fallback. Walking past that pid bound a second grok here. Subagents share the pid and need their own execution.

P6. AGREE. One hub, and a thin agent only where wake needs it.

P7. AGREE. The shell payload is the command string. Reconcile with `git status`.

P8. AGREE. `systemMessage`, `additionalContext`, resume by id, ACP, a desktop notification. Wake ticket as in D6.

P9. AGREE. Session storage is already keyed by an encoded cwd.

P10. AGREE. Delimit the digest in `systemMessage` and `additionalContext`. `UserPromptSubmit` cannot carry it.

P11. CHANGE. `iris integrate grok`, then doctor. Doctor must report `projectTrusted: false`. Do not write Claude user hooks; grok runs them too.

P12. AGREE. Signed package-manager releases. Do not copy grok's `curl | bash` path.

## F. From scratch

Ship two config files and a trust check. Join on the session id grok already stamps on both children, and issue a daemon nonce so a hostile hook cannot mint an Execution. Use a 3s timeout and leave `failClosed` off until a repo opts in. Treat shell text as a hint and git as enforcement. Make the wake credential a scoped ticket, because a model that can read a Monitor URL must learn nothing it can reuse. Skip plugins: they cannot ship the Go binary, and a Claude plugin would turn itself on inside grok. That is about a day after `iris hook` exists, plus a short doctor.

## G. Unknowns

- Windows and macOS spawn, ACLs, and Seatbelt were not run. Windows token storage is [UNVERIFIED].
- A returned Stop block, `systemMessage`, and `UserPromptSubmit` context were not watched in model output.
- Resume, fork, `/remote`, leader mode, sandbox-on, and redirects were not run. Sockets were absent; the doc says no redirects.
- A later build may apply the `/tmp` suppression this binary skipped.
- MCP notifications, resources, and elicitation reaching the model are [UNVERIFIED].
- Cursor flat hook entries and Cursor-shaped denies were not tested.
