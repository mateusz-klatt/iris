# Claude Code report (2.1.318)

Measured on Linux amd64 on 2026-11-05, in isolated throwaway workspaces, with
`--setting-sources project,local` and all inherited `CLAUDE*` variables removed. The runs
used Haiku and cost about $0.003 each. Evidence labels follow the consultation brief:
`[MEASURED]`, `[DOCS url]`, `[UNVERIFIED]`.

Sources: hooks reference <https://code.claude.com/docs/en/hooks>, plugins reference
<https://code.claude.com/docs/en/plugins-reference>, components
<https://code.claude.com/docs/en/plugins/components>.

## A. Facts

### A1 Hooks

33 events exist [DOCS]. Those Iris needs fired in `-p` mode: `SessionStart`,
`UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `SessionEnd` [MEASURED]. Locations
are user, project, local, managed and plugin `hooks/hooks.json` [DOCS].

Handler types are `command`, `http`, `mcp_tool`, `prompt` and `agent` [DOCS]. A `command`
with `args` is spawned directly with no shell; on Windows it must be a real `.exe` [DOCS].
Without `args` it runs through `sh -c`, or Git Bash and then PowerShell on Windows [DOCS].

Common stdin: `session_id`, `transcript_path`, `cwd`, `hook_event_name`, plus `agent_id` and
`agent_type` inside a subagent [DOCS]. `PreToolUse` adds `tool_name`, `tool_input`,
`tool_use_id` [DOCS].

`PreToolUse` deny, proved live [MEASURED]:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",
 "permissionDecisionReason":"..."}}
```

`updatedInput` can rewrite tool input [DOCS]. `SessionStart` and `UserPromptSubmit` return
`hookSpecificOutput.additionalContext`; the `SessionStart` context was seen by the model in
a live run [MEASURED]. Context is capped at 10,000 characters [DOCS].

Default timeout is 600 s for `command`, `http` and `mcp_tool`, 30 s on `UserPromptSubmit`,
and `SessionEnd` shares a 1.5 s budget [DOCS]. Exit 0 applies JSON, exit 2 blocks where
blocking is possible, any other exit is a non-blocking error unless `onFailure: "block"` is
set [DOCS]. A timeout on `PreToolUse` lets the tool proceed [DOCS].

Project hooks need folder trust in interactive mode. The dialog's default selection is
"No, exit" [MEASURED]. `-p` skips the dialog [MEASURED].

### A2 Spawn mechanics

Hook and stdio MCP server are both direct children of the same `claude` pid [MEASURED].
`CLAUDE_CODE_SESSION_ID` is set in both, and equals stdin `session_id` [MEASURED]. Hooks also
see `CLAUDE_PROJECT_DIR`, and `CLAUDE_ENV_FILE` on `SessionStart` [MEASURED].

These variables are inherited by nested tools. A Copilot or Antigravity process started
from a Claude session carries the parent's `CLAUDE_CODE_SESSION_ID` [MEASURED]. Their
presence alone therefore does not prove that Claude is the immediate client.

### A3 MCP

Hooks fire for MCP tools, as `mcp__<server>__<tool>` [MEASURED]. Configuration is `.mcp.json`
at project scope, `claude mcp add` for user scope, or a plugin's `.mcp.json` [DOCS].

Tool annotations do not change permissions [MEASURED 2026-11-07, Claude Code 2.1.319, WSL]. In
`claude -p` without an allow rule, `claims`, which carries `readOnlyHint: true`, was refused like
`send`. With `--allowedTools mcp__<server>__claims` the same call reached the hub. So a
headless run still needs the Iris tools allowed by name, as `iris doctor --live` does.

### A4 Packaging

A plugin has `.claude-plugin/plugin.json` and can bundle `hooks/hooks.json`, `.mcp.json`,
`skills/`, `monitors/monitors.json` and `bin/` [DOCS]. Install with `claude plugin marketplace
add` and `claude plugin install`, both non-interactive [MEASURED `claude plugin --help`].

### A5 Wake-up: the plugin monitor

This is the strongest wake-up path of any client measured.

`monitors/monitors.json` is an array of `{name, command, description, when?}` [DOCS]:

```json
[{"name":"iris","command":"iris watch --client claude","description":"Iris attention"}]
```

Measured in an interactive session started with `--plugin-dir`:

- The monitor starts with the session and needs no re-arming. It is killed at session end.
- A line on stdout woke a **fully idle** session. `UserPromptSubmit` fired 0.3 s after the
  line was written, and `Stop` followed 3.9 s later once the model had answered.
- The process gets `CLAUDE_CODE_SESSION_ID`, the same value as the hooks.
- `${CLAUDE_PLUGIN_ROOT}` is substituted in `command`, but the variable is **not** in the
  process environment.
- Its parent is a `bash` wrapper, and its cwd is the session directory.
- A crash of the monitor also reaches the model and wakes it.
- Without further instruction the model said the notification "came from a background
  monitor, not from you" and took no action. Iris must therefore tell the model, through
  session context or a skill, what an Iris notification means.

Monitors do not start under `-p` [DOCS].

Each wake-up costs a model turn, so `iris watch` must coalesce events and report only what
needs attention.

The `Monitor` tool is the alternative: `ws: {url, protocols}`, one notification per text
frame, or a `command`. It has no custom headers, expires after at most 30 minutes, and the
model must arm and re-arm it. The project owner reports this as a proven pattern. It suits
clients of the plugin-less kind and ad-hoc use; the plugin monitor needs no model action.

`asyncRewake` on a command hook is a one-shot alternative: a background hook that exits 2
wakes the model [DOCS].

Channels push through MCP, but they are a research preview that requires development flags
[DOCS https://code.claude.com/docs/en/channels], so Iris does not use them yet.

`--resume <id>` and `--bg` exist for controlled sessions [MEASURED `claude --help`].

### A6 Session identity

`session_id` on stdin and `CLAUDE_CODE_SESSION_ID` in the environment match in hooks, stdio
MCP servers and monitors on a new session and on `--resume <id>` [MEASURED]. `SessionStart.source`
is `startup`, `resume`, `clear`, `compact` or `fork` [DOCS], so context can be re-injected after
compaction.

**They differ on `--continue`.** The MCP server is started with a fresh `CLAUDE_CODE_SESSION_ID`
before the session to continue has been chosen, and the hooks then get the continued session's
id [MEASURED, 2.1.319, macOS, `claude -p --continue` with a probe MCP server and a SessionStart
hook: MCP `3d150e72-...`, hook `58ff154c-...` with `source: resume`]. Both have the same parent
process, the Claude Code CLI. The same split was seen in interactive sessions restarted after a
power loss on two hosts: the hub held each MCP's Execution under a session id no hook ever sent
(2026-11-08). So the session id cannot tie an MCP server to its hooks; the client process can,
and Claude hosts one top-level session per process, its subagents registering under a parent
(D6). On every start measured, new, `--resume` and `--continue`, the MCP server started before
the SessionStart hook ran, so a binding at the MCP's first call can come before the hook has
registered the session.

## B. Minimal Iris integration

A plugin is required, because monitors exist only in plugins. Hooks in the plugin use exec
form and rely on `iris` being on `PATH`.

## C. Capability matrix

| Capability | Value | Evidence |
|---|---|---|
| session-start-context | yes | MEASURED |
| prompt-context | yes | DOCS; event MEASURED |
| pre-tool-deny | yes | MEASURED |
| post-tool | yes | MEASURED |
| stop-block | yes | DOCS |
| session-end | yes | MEASURED, 1.5 s budget |
| edit-path-visible | yes | MEASURED, `tool_input.file_path` |
| shell-command-visible | yes | MEASURED, `tool_input.command` |
| subagent-events | yes | DOCS |
| fires-headless | yes | MEASURED |
| async-handler | yes | DOCS, `async`, `asyncRewake` |
| idle-wake | yes | MEASURED, plugin monitor, 0.3 s, interactive only |

## G. Unknowns

- Windows behaviour of exec-form hooks and monitors.
- Whether a monitor survives `claude --resume` and `/clear`.
- The rate at which monitors are automatically stopped for producing too many events.
