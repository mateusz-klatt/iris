# Iris: architecture consultation brief (round 1)

Read this whole file before doing anything else.

## 1. What Iris is

Iris is a coordination layer for concurrent coding agents. Many agent sessions, from
different vendors' CLIs, on different machines and operating systems, often in different
git worktrees of one repository, need to:

- know who else is working and on what;
- exchange durable messages with acknowledgement;
- claim files or other resources before editing, so that they do not collide;
- notice when something needs their attention;
- be watched and steered by a human from a web UI.

Primary user: one developer or a small team, up to about 50 concurrent sessions across up
to about 5 machines. It is a greenfield project and nothing is implemented yet. "Messages"
are agent-to-agent records in Iris's own store. Iris has nothing to do with SMTP or IMAP.

## 2. Decisions already made

Do not relitigate these unless you find a blocking reason, and if you do, say so plainly.

- Backend, daemon, hooks and CLI: Go, one static binary, `CGO_ENABLED=0`.
- Storage: SQLite in WAL mode through `modernc.org/sqlite`; the daemon is the only writer;
  FTS5 for search; explicit SQL and migrations.
- MCP: the official Go SDK, `modelcontextprotocol/go-sdk`.
- Web UI: existing first-party React/TypeScript embedded with `go:embed`. Node is a
  build-time dependency only.
- Domain shape: durable Agent, concrete Execution (one session or run), Claims held by an
  Execution, with the worktree as execution context. A claim is granted atomically on the
  server; a local cache must never decide that a resource is free.
- Durable event log with ids, resume from a cursor, deduplication.
- Any git or Markdown export is optional and asynchronous, never on the write path.
- Licence: Apache-2.0.
- Clean-room development: see section 8.

## 3. Draft command surface

```
iris serve                              coordination server, HTTP API, MCP, web UI
iris agent                              optional per-host process for push and wake-up
iris mcp                                stdio MCP server for clients that prefer stdio
iris hook --client <c> --event <e>      short-lived hook adapter, stdin to stdout
iris integrate <client>|--all           install, repair or remove client configuration
iris doctor                             diagnostics with a live end-to-end canary
```

## 4. Target clients

Claude Code, Codex CLI, Google Antigravity CLI (`agy`), Kimi Code, Grok CLI, Cursor Agent
(`cursor-agent`), GitHub Copilot CLI. They differ widely, and Iris must degrade gracefully
when a client lacks a capability.

## 5. Target platforms

Windows native (amd64, arm64), WSL2, macOS (arm64, amd64), Linux (amd64, arm64), and
containers. Real setups mix these, for example the hub on Linux with agents on Windows,
in WSL and on a Mac. Repositories can sit on a Windows drive and be reached from WSL.

## 6. Distribution goal

Installation must feel seamless and must come from sources people already trust: package
managers first (Homebrew, winget, Scoop, apt/dnf repositories), signed GitHub Releases,
GHCR for the server image. No `curl | sh` as the primary path, no Node or Python at
runtime, no shell scripts as application logic.

Name facts, checked today:

- Homebrew has a cask named `iris` (a blue-light filter); no formula of that name.
- Scoop Extras has `iris`. Scoop Main does not.
- AUR and npm both have `iris`.
- `github.com/kataras/iris` is a well-known Go web framework (about 25k stars).
- The name `irisd` is free in Homebrew, Scoop, npm and AUR.

## 7. Draft positions to attack

P1. Three layers. MCP tools are the universal explicit API. Client hooks add automation:
    session binding, inbox digest, pre-edit claim check, release at the end. A git
    pre-commit or pre-push guard is the only hard enforcement.

P2. One hook code path. Every client calls `iris hook --client <c> --event <e>`; an adapter
    converts client JSON into a canonical event and a canonical decision back into that
    client's JSON. Native HTTP or MCP-tool hook handlers, where they exist, are an optional
    optimisation and not the baseline.

P3. Hook budget: p95 under 50 ms on Linux and macOS, under 150 ms on Windows, hard timeout
    2 to 3 s. Fail-open by default, because coordination must never brick an agent
    session. Fail-closed is an explicit per-repository opt-in.

P4. Hook-to-daemon transport is loopback HTTP with a bearer token in a user-only file, not
    Unix sockets or named pipes, so that Windows, WSL and containers behave alike.

P5. Binding hook events and MCP calls to one Execution: a per-session stdio `iris mcp`
    child, with hook and stdio server each walking up to the nearest client process and
    using its pid plus start time as a join key, plus the client's session id wherever it
    is exposed. Attack this hard: it is the least proven idea here.

P6. Topology: one `iris serve` hub on a host or in a container; a thin `iris agent` per
    host only where push or wake-up needs it. On a single machine `iris serve` alone is
    enough. Autostart with launchd, `systemd --user` or Task Scheduler through
    `iris service install`, plus lazy start from a hook.

P7. Shell-based edits are invisible to file-edit hooks, so reconcile after tool use with
    `git status --porcelain` in the worktree, and hard-stop at the git guard.

P8. There is no universal way to push into an idle session. Design by capability: next-turn
    injection through hook context, a client-native mechanism where one exists, a desktop
    notification to the human, and no keystroke injection unless explicitly opted in.

P9. Repository identity comes from the normalised remote URL plus the root commit, not from
    an absolute path, so that `/mnt/c/x`, `C:\x` and `/Users/me/x` match. Claims use
    repository-relative POSIX paths with an explicit case-folding policy.

P10. Messages between agents are untrusted input. They are injected as clearly delimited
    data with size caps and sender provenance, never as instructions.

P11. Install UX is two steps: a package manager command, then `iris integrate --all`, which
    is idempotent and reversible, writes user-level configuration with backups, and is
    followed by `iris doctor` running a live canary per client.

P12. Release pipeline: GoReleaser; Homebrew tap, Scoop bucket, winget, deb and rpm, GHCR;
    cosign keyless signatures, GitHub attestations, SBOM; Apple notarization and Windows
    code signing as soon as they can be funded.

## 8. Clean-room rules (absolute)

Iris is an independent implementation written from its own requirements. Two public
projects in this space carry a licence rider that makes them unusable here.

You must not open, fetch, search for, read, summarise or quote code, documentation or
READMEs from:

- `github.com/Dicklesworthstone/mcp_agent_mail`
- `github.com/Dicklesworthstone/mcp_agent_mail_rust`
- `github.com/mateusz-klatt/mcp_agent_mail`
- any local path whose name contains `mcp_agent_mail`, `mcp-agent-mail` or `iris.git`,
  including plugin caches under `~/.claude/plugins`.

If a search result points there, skip it. If you notice tools from such a project in your
own session, do not call them and do not reuse their names. Use only your CLI's official
documentation, its installed binary and `--help` output, its public source if it is open
source, the MCP specification, and Go documentation. Do not propose names copied from
those projects. Anything you write must be your own words.

## 9. Evidence rules

Every factual claim about a client carries one label:

- `[MEASURED]`: you observed it on the binary installed here; give the command.
- `[DOCS url]`: official vendor documentation; give the url.
- `[SOURCE url]`: vendor source code; give the url.
- `[UNVERIFIED]`: memory or inference. Say what would verify it.

Prefer measuring. Your training data about these fast-moving CLIs is probably out of date,
so a claim from memory is worth little. "This client has no such feature" needs evidence
too. This host is Linux; you have no Windows or macOS here, so per-OS behaviour on those
systems comes from docs or source and must be labelled accordingly.

## 10. Report format

Write `REPORT.md` in your workspace root, at most 2500 words excluding fixtures:

```
# <client> report (<version>)
## A. Facts
A1 Hooks: do they exist; events; config locations and precedence (user, project, plugin,
   enterprise); handler types; matchers; stdin payload; output contract for allow, deny,
   ask, context injection and stop-blocking; exit-code semantics; default timeouts;
   behaviour on error or timeout (fail-open or fail-closed); async support; whether they
   fire in headless mode and in subagents; whether project-level hooks need a trust step.
A2 Spawn mechanics per OS: shell or direct exec; which shell on Windows; quoting; cwd; env
   vars given to hooks and to stdio MCP servers; process tree (is the hook a direct child,
   is the stdio MCP server a child of the same process); does a sandbox restrict hook
   commands, loopback network access or MCP.
A3 MCP: transports; config location; non-interactive add command; env and header expansion;
   tool naming and auto-approval so Iris tools do not prompt; whether server-initiated
   notifications, resources or elicitation reach the model or the user.
A4 Packaging: plugin, extension or marketplace mechanism; can it bundle hooks, MCP servers,
   skills and binaries; instruction files that load automatically.
A5 Wake-up: how an external process can make an idle or running session act, for example
   resume by id, an app server, ACP or another RPC, remote control.
A6 Session identity: where the id appears, stability across resume and compaction, how
   subagents are distinguished.
## B. Minimal Iris integration
Exact configuration, with exact paths per OS, for session-start context injection, a
pre-edit deny decision and session end.
## C. Capability matrix
One line each, value yes, no or partial, with an evidence label: session-start-context,
prompt-context, pre-tool-deny, post-tool, stop-block, session-end, edit-path-visible,
shell-command-visible, subagent-events, fires-headless, async-handler, idle-wake.
## D. Lens findings
Your assigned lens, top findings first, each with a concrete recommendation.
## E. Verdict on P1 to P12
For each: AGREE, CHANGE (with the replacement) or REJECT (with the reason), at most 2 lines.
## F. From scratch
At most 150 words: what would you do differently if you were designing this alone?
## G. Unknowns
What you could not verify, and how to verify it.
```

## 11. Live hook experiment

If your client has hooks, prove it instead of describing it:

1. In your workspace, create a project-level hook configuration that runs a small logging
   script, which appends the hook's stdin, argv, cwd, environment variable names (names
   only, never values), its parent process id and the parent's command line to
   `fixtures/log.jsonl`. Write the script in the workspace.
2. Run your own CLI headless with the shortest possible prompt that triggers a file edit
   and a shell command in the workspace, using the cheapest available model.
3. Save the raw payloads as `fixtures/<event>.json`, with secrets and personal paths
   replaced by placeholders. Then prove a deny: make the script return a deny decision
   for one edit and record what the client did.
4. Also check whether a stdio MCP server configured at project level shares an ancestor
   process with the hook. This tests P5.

If project-level hooks need a trust step you cannot complete headless, or if user-level
configuration would be required, stop there and report that as a finding. Never edit
user-level configuration.


# Addendum to BRIEF.md: measurements so far and new input

This file amends BRIEF.md. Where they differ, this file wins.

## Correction to section 8

The "use only" sentence was too narrow. The ban covers only the named projects and paths.
You may and should use any other primary source: SQLite documentation, database and
distributed-systems literature, Go documentation, the OpenAI Codex documentation and source.

## Measured facts about session binding (P5)

| Client | Hook session id | Id in stdio MCP env | Hook process tree | MCP process tree |
|---|---|---|---|---|
| Claude Code 2.1.318 | `session_id` on stdin, `CLAUDE_CODE_SESSION_ID` env | yes, `CLAUDE_CODE_SESSION_ID`, same value | direct child of claude | direct child of the same claude pid |
| Copilot CLI 1.0.105 | `sessionId` on stdin, no env var | yes, `COPILOT_AGENT_SESSION_ID`, same value | direct child of copilot | direct child of the same copilot pid |
| Cursor Agent 2026.11.05 | `conversation_id` on stdin, `CURSOR_CONVERSATION_ID` env | no, MCP env is sparse | bash, then cursor-agent | direct child of cursor-agent |
| Antigravity 1.5.1 | `conversationId` on stdin, `ANTIGRAVITY_CONVERSATION_ID` env | no | sh, then agy | direct child of the same agy pid |

All measured on Linux. Hooks fire for MCP tool calls in Claude Code, Copilot and Cursor.

## Measured failure semantics of a pre-tool hook

| Client | Hook exits 1 | Hook times out |
|---|---|---|
| Claude Code | tool proceeds (docs) | tool proceeds (docs) |
| Copilot CLI | tool is DENIED (measured) | tool proceeds (docs) |
| Antigravity | tool is DENIED (measured) | unknown |
| Cursor | tool proceeds unless `failClosed` (docs) | unknown |

Therefore `iris hook` must catch everything and always exit 0 with a valid allow payload.

## Other measured surprises

- Edit tools differ by model, not only by client. Copilot with a GPT model uses `apply_patch`
  whose `toolArgs` is a raw string patch (`*** Add File: x`), while other models use
  structured `edit` or `create` tools with JSON arguments. The adapter has to parse patches.
- Hook cwd differs: Antigravity runs project hooks in `.agents/`, not the repository root.
- Project-level hooks need a trust step on Cursor and Copilot, but not on Antigravity.

## New input from the project owner: instant wake-up

The owner reports that a WebSocket watched by Claude Code's `Monitor` is a proven pattern
for catching new messages instantly. Facts:

- The `Monitor` tool takes `ws: {url, protocols}`; each text frame becomes a notification.
  It has no custom headers, so authentication must travel in the URL or a subprotocol, and
  it expires after at most 30 minutes, after which the model must re-arm it.
- A Claude Code plugin can declare `monitors/monitors.json` with `name`, `command`,
  `description` and optional `when`. The command runs for the whole session, without
  re-arming, and each output line reaches the model. Plugin monitors do not start under
  `claude -p`.
- `asyncRewake` hooks can also wake a session once, when a background hook exits 2.
- Channels exist but are a research preview behind development flags, so they are out of
  scope for now.

The likely design is a server-side `iris watch` stream: a WebSocket endpoint that sends
one short notification per event for one Execution, plus an `iris watch` command that
prints the same stream line by line. A notification only says that something is waiting;
the agent then fetches the content through a normal tool call. Treat this as a requirement
and check that the event model supports it: at-least-once delivery, reconnect from a
cursor, deduplication, and no message content in the notification.
