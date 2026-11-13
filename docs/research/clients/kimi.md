# Kimi Code report (2.5.1)

## 2026-11-12 addendum: local web wake in 2.6.0

Kimi 2.6.0 exposes the documented, experimental REST and WebSocket surfaces from `kimi web`.
The session prompt endpoint accepts a client-chosen idempotent `prompt_id` and enqueues input; a
session resource reports `busy`, `main_turn_active`, `archived` and `pending_interaction`. Live
`/openapi.json` and `/asyncapi.json` are the compatibility contract. Running instances register in
`$KIMI_CODE_HOME/server/instances/`; their bearer is in `$KIMI_CODE_HOME/server.token` (mode 0600).
The [Server API reference](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/server-api.html)
also defines `40931` and warns that `:takeover` is only for a lock abandoned by a dead process.

An isolated 2.6.0 server on loopback confirmed session creation, `busy:false`, an empty prompt
queue and an `agent` frame with `status:"idle"`. A later adapter test submitted the fixed Iris
prompt to another isolated session without a provider: the server accepted it, ended with
`last_turn_reason:"failed"`, and reported zero turns, tokens and cost; repeating its `prompt_id`
created no turn. This is evidence for an external web-API wake, not for direct push into a TUI
widget, native TUI refresh or completed model delivery. Iris uses the REST idle read and never
calls `:takeover` on a live session.
See [ADR 026](../adr/026-kimi-web-idle-wake.md). The older report below remains the evidence for
2.5.1 hooks and process mechanics; its statement about the absence of direct idle-TUI push is not
a claim that the newer external REST channel is absent.

> The run ended early on the 3 requests per minute limit of the account; sections A to G were complete. `session_id` does not appear in the stdio MCP environment, so this client uses the nearest-process rung of the binding chain.

Evidence: [MEASURED] = observed on the installed binary on this Linux host (command/fixture given);
[DOCS url] = official docs (base https://moonshotai.github.io/kimi-code/en/); [UNVERIFIED] = inference.
Live experiment: nested `kimi -p` runs in a throwaway `$KIMI_CODE_HOME` inside this workspace
(`hooklab/`), user-level config copied, never edited; raw captures in `fixtures/`.

## A. Facts

### A1 Hooks

- Hooks exist with 20 events: UserPromptSubmit, UserPromptQueued, PreToolUse, Stop, TurnStarted,
  PostToolUse, PostToolUseFailure, PermissionRequest, PermissionResult, SessionStart, SessionEnd,
  SessionHeartbeat, SubagentStart, SubagentStop, TaskStarted, StopFailure, Interrupt, PreCompact,
  PostCompact, Notification. [DOCS customization/hooks.html; names also in binary strings]
- Config: ONLY user-level `$KIMI_CODE_HOME/config.toml` `[[hooks]]` and plugin manifests. No
  project-level hook file — deliberate, so a checked-out repo cannot inject hooks. Plugins are
  per-user; "project-level installation scope is not yet supported". [DOCS hooks.html, plugins.html]
- Rule fields: `event`, `matcher` (regex over event target), `command` (shell string), `timeout`
  (1-600 s, default 30). Unknown fields fail the load. [DOCS] Strictness measured: one bad key in
  one entry made the CLI ignore the ENTIRE hooks array with a warning (`Ignored invalid config
  section 'hooks'`). [MEASURED: run 1]
- Matching rules for one event run in parallel; identical `command` strings dedupe. [DOCS]
- stdin: JSON, snake_case. Measured field sets (fixtures/*.json):
  SessionStart {hook_event_name, session_id, client_type, cwd, source, model, profile};
  UserPromptSubmit {+ prompt, is_steer}; PreToolUse {+ tool_name, tool_call_id, tool_input};
  PostToolUse {+ tool_output}. [MEASURED] Docs also promise `session_title` in the base payload —
  absent in measured payloads: doc/reality drift, no schema-version field. [MEASURED]
  tool_input shapes: Write {path, content}; Bash {command}. [MEASURED fixtures]
- Output contract: exit 0 allow (stdout may be appended to context; for UserPromptSubmit it IS
  appended); exit 2 = block, stderr is the reason and is written into model context; JSON stdout
  `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":...}}` also blocks.
  Other non-zero/timeout/crash = allow. Fail-open everywhere. [DOCS; deny MEASURED, see below]
- Blockable: only PreToolUse, Stop, UserPromptSubmit. Stop-blocking supported (block appends a
  message, model continues). All other events observation-only. [DOCS]
- Timeout: per rule 1-600 s default 30; non-Windows: separate process group, signal then kill.
  [DOCS]
- Async: none; the turn waits for the event's rules. [DOCS]
- Headless: hooks DO fire in `kimi -p` mode — SessionStart, UserPromptSubmit, PreToolUse,
  PostToolUse captured. [MEASURED fixtures/log.jsonl] SessionEnd was NOT observed on `-p`
  completion. [MEASURED]
- Trust: none for user-level hooks; plugins install via TUI `/plugins install` only. [DOCS]

### A2 Spawn mechanics

- Linux measured: hook = `/bin/sh -c <command>` child of the `kimi-code` process (grandchild via
  shell); cwd = session project dir. [MEASURED fixtures: ppid_cmd/gppid_cmd] stdio MCP servers are
  DIRECT children of the same kimi-code process — hook and MCP server share the kimi process as
  ancestor. [MEASURED fixtures/log.jsonl]
- Windows: undocumented (docs mention process groups only for non-Windows). [UNVERIFIED]
- Env: plain hooks inherit the CLI env (no kimi-injected vars measured); plugin hooks additionally
  get KIMI_CODE_HOME and KIMI_PLUGIN_ROOT. [MEASURED env_names; DOCS]
- Sandbox: none; hooks/MCP run with full user privileges. Project-level `.kimi-code/mcp.json`
  stdio servers need workspace trust; headless bypass: `KIMI_CODE_TRUST_WORKSPACE=1`. User-level
  mcp.json needs no trust. [MEASURED run 1 warning + run 2]

### A3 MCP

- Transports stdio/HTTP/SSE. Config: `$KIMI_CODE_HOME/mcp.json` (user) + `.kimi-code/mcp.json`
  (project, wins per name). [DOCS]
- No non-interactive add command (`/mcp-config` is TUI-only). [MEASURED --help; DOCS]
- Schema: `{mcpServers:{name:{command,args,env,cwd}|{url,transport,headers,bearerTokenEnvVar},
  startupTimeoutMs(30000),toolTimeoutMs,enabledTools,disabledTools,enabled,deferred}}`; env values
  are literal overlays, no ${VAR} expansion. [DOCS]
- Tool naming `mcp__<server>__<tool>`; `[[permission.rules]]` with `*`/`**` patterns (e.g.
  `mcp__iris__*`) pre-approves Iris tools; Ask-When-Needed/auto modes auto-approve MCP. [DOCS]
- Failed stdio servers retry with exponential backoff — my non-speaking stub was respawned ~19
  times in one session. [MEASURED fixtures/log.jsonl]
- Server-initiated notifications/resources/elicitation: not documented. [UNVERIFIED]

### A4 Packaging

- Plugin manifest (`kimi.plugin.json` / `.kimi-plugin/plugin.json`) bundles skills, agents,
  sessionStart.skill, systemPrompt(+Path), mcpServers, hooks, slash commands. Per-user install via
  TUI; copied to `$KIMI_CODE_HOME/plugins/managed/<id>`; marketplace overridable via
  KIMI_CODE_PLUGIN_MARKETPLACE_URL. [DOCS plugins.html]

### A5 Wake-up

- Resume `kimi -S <id>` (help) — note the CLI's own hint prints `kimi -r session_...`; both work.
  [MEASURED run output vs --help] `kimi session list` non-interactive. [MEASURED]
- `kimi web` local REST/WS server (127.0.0.1:58627, bearer): binary exposes
  `POST /sessions/{id}/prompts`, `prompts:steer`, `:abort`, `:compact`. [MEASURED --help + strings]
- `kimi acp`: ACP server over stdio. [MEASURED --help]
- No push into an idle interactive TUI; SessionHeartbeat (60 s, only when configured) is
  observation-only. [DOCS]

### A6 Session identity

- Every hook payload carries `session_id` (format `session_<uuid>`). [MEASURED fixtures]
- Sessions under `$KIMI_CODE_HOME/sessions/`; manageable via `kimi session list/export/fork`.
  [MEASURED]
- Stability across compaction [UNVERIFIED]; subagents surface via SubagentStart/Stop; per-subagent
  PreToolUse [UNVERIFIED].

## B. Minimal Iris integration (kimi)

`$KIMI_CODE_HOME/config.toml`:

```toml
[[hooks]]
event = "SessionStart"
command = "iris hook --client kimi --event session-start"
timeout = 5

[[hooks]]
event = "UserPromptSubmit"
command = "iris hook --client kimi --event prompt"
timeout = 5

[[hooks]]
event = "PreToolUse"
matcher = "Write|Edit|Bash"
command = "iris hook --client kimi --event pre-tool"
timeout = 5

[[hooks]]
event = "SessionEnd"
command = "iris hook --client kimi --event session-end"
timeout = 5
```

Decision mapping: allow=exit 0; deny=exit 2 + reason on stderr; context injection=exit 0 + stdout
text at UserPromptSubmit. The adapter must no-op in <5 ms when payload `cwd` is not an Iris-managed
repo, because hooks are global. `iris integrate kimi` must validate the written TOML (one bad key
invalidates the whole section [MEASURED]).

MCP: project `.kimi-code/mcp.json` `{"mcpServers":{"iris":{"command":"iris","args":["mcp"]}}}` +
`[[permission.rules]] decision="allow" pattern="mcp__iris__*"`; note the workspace-trust step for
project stdio servers.

## C. Capability matrix (kimi 2.5.1)

- session-start-context: partial — SessionStart hook observation-only; plugin sessionStart.skill is
  the injection path [DOCS]; hook stdout at SessionStart [UNVERIFIED]
- prompt-context: yes [DOCS + MEASURED payload]
- pre-tool-deny: yes [MEASURED: exit 2 blocked Write, model adapted]
- post-tool: yes [MEASURED PostToolUse with tool_output]
- stop-block: yes [DOCS: Stop blockable with continue-message]
- session-end: partial — exists [DOCS] but did not fire on `-p` exit [MEASURED]
- edit-path-visible: yes [MEASURED tool_input.path/content]
- shell-command-visible: yes [MEASURED tool_input.command]
- subagent-events: yes (SubagentStart/Stop) [DOCS]; per-subagent tool hooks [UNVERIFIED]
- fires-headless: yes [MEASURED]
- async-handler: no [DOCS]
- idle-wake: partial — web REST prompt-by-id + ACP; no idle-TUI push [MEASURED help/strings]

## D. Lens findings — adapter architecture

1. Two-channel decision contract, trivially mappable. Canonical decision -> {exit 0 | exit 2 +
   stderr}; JSON-stdout deny exists as an alternative. Pin ONE mapping in the adapter (exit 2 +
   stderr — the reason lands in model context for free, proven in the deny run) so golden fixtures
   pin one contract. Effort: Quick.
2. Global hooks force repo-scoping INTO the adapter. With no project-level hook file, every kimi
   session on the host fires Iris hooks; the adapter must match payload `cwd` against managed repos
   and no-op fast. This is client-specific and belongs in the kimi adapter, not the daemon.
   Effort: Quick. (For other clients with project-level hooks this logic must stay optional.)
3. Canonical event model must be open, not lowest-common-denominator. Kimi's superset —
   SessionHeartbeat (free 60 s liveness/presence primitive), TurnStarted(origin_kind),
   PermissionRequest/Result, UserPromptQueued — should flow through as typed extensions; unknown
   events pass through with client tag. Effort: Short.
4. No schema version + doc drift = golden fixtures are load-bearing. Measured payloads already
   contradict docs (`session_title` promised, absent). Capture fixtures per client+CLI version
   (as in fixtures/), have the adapter require only {hook_event_name, session_id, cwd} and tolerate
   the rest, and run a synthetic-event canary (`iris doctor`) after each client upgrade.
   Effort: Short.
5. Vendor breakage mode is fail-open — design the blast radius, not the parser. Kimi ignores the
   whole hooks section on one bad key, and fail-open means a broken adapter degrades to
   "uncoordinated agent", never a bricked session. So: the installer validates config it writes;
   the adapter logs a field-name hash per event to detect silent schema drift. Effort: Quick.
6. Not in the adapter: claim arbitration, repo identity, message semantics, auth. In the adapter:
   event-name translation (PascalCase), tool taxonomy (Write/Edit/Bash), matcher regex syntax,
   decision encoding, repo scoping, timeout discipline (set explicit small `timeout`, 5 s).

## E. Verdicts P1-P12

- P1 AGREE — kimi docs themselves warn hooks are not a sole barrier; layering + git guard stands.
- P2 AGREE — kimi has only shell-command hooks (spawned via `/bin/sh -c` [MEASURED]); one code path
  is the only option here.
- P3 AGREE — kimi is natively fail-open; set explicit `timeout = 5` per rule.
- P4 AGREE — hook processes are unsandboxed; loopback HTTP fine.
- P5 CHANGE — replace pid+start-time walking as the primary join key: every kimi hook payload
  carries `session_id` [MEASURED], and per-session stdio MCP shares the same parent process anyway
  [MEASURED]. Use client session id first; keep pid-walking only as a fallback for clients lacking
  it.
- P6 AGREE — hook-triggered lazy start viable (plain child processes).
- P7 AGREE — Bash commands are visible via PreToolUse but their file effects are not; git-status
  reconciliation still required.
- P8 AGREE — confirmed for kimi: no idle push; native channels are web REST prompt-by-id and ACP;
  next-turn injection via UserPromptSubmit.
- P9 AGREE — payload `cwd` is an absolute local path; P9 normalization needed before claims.
- P10 AGREE — UserPromptSubmit injection lands in model context; delimit + cap + provenance.
- P11 AGREE with note — for kimi the ONLY hook target is user-level config.toml; `iris integrate`
  must edit TOML idempotently (dedupe by command string) and re-validate (strict section failure).
- P12 AGREE — orthogonal to kimi.

## F. From scratch

I would ship the kimi integration as a plugin first: one manifest bundles hooks, the iris MCP
server and a sessionStart skill — atomic enable/disable, plugin-root cwd, extra env vars. Because
plugin install is TUI-only, `iris integrate kimi` still writes `[[hooks]]` for headless setups, but
the plugin is the better interactive path. I would drop the pid-walking join key entirely (every
payload has session_id; project mcp.json scopes the server), and I would adopt SessionHeartbeat as
the presence primitive instead of inventing liveness. Finally I would budget for doc drift: trust
measured fixtures over vendor docs, always.

## G. Unknowns

- SessionEnd semantics in headless mode (not observed); behaviour in `kimi web` sessions.
- Windows hook spawn/quoting/process-group behaviour — undocumented; test on Windows.
- session_id stability across compaction; whether PreToolUse fires for subagent tool calls.
- Whether exit-0 stdout at SessionStart is appended to context (docs ambiguous).
- MCP server-initiated notifications/elicitation support.
- Rate limiting: this consult hit a 3-requests/min org cap (HTTP 429 after ~20 quick retries) —
  Iris's doctor canary must distinguish vendor rate limiting from hook/adapter failure.

## Measured live-experiment summary

- Run 1: hooks silently absent (config strictness finding); user-level MCP stub spawned as direct
  child of kimi-code (3 respawns); project-level MCP skipped pending trust.
- Run 2 (trust env set, DENY flag): SessionStart, UserPromptSubmit, PreToolUse(Write deny-me.txt)
  -> exit 2 -> write blocked, reason surfaced, model adapted and completed via Bash pwd;
  PreToolUse(Bash) + PostToolUse captured; both MCP stubs (user + project) spawned as children of
  the kimi process. No 429 in nested runs; model moonshot-ai/kimi-k2.6.
- Raw records: fixtures/log.jsonl; per-event sanitized payloads: fixtures/sessionstart.json,
  userpromptsubmit.json, pre_tool_use_write_denied.json, pre_tool_use_bash.json, posttooluse.json.
