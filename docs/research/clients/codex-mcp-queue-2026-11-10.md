# An MCP child queueing its own Codex session (0.167.0)

2026-11-10, WSL. [ADR 017](../../adr/017-wake-codex-by-queue.md) decision 1 leaves open whether
`iris mcp`, an MCP child of a Codex thread, can itself run `codex queue --thread <its session>
--resume` and reach the daemon its session runs on. Decision 3 leaves open whether a queued
turn's prompt hook gets the queued text unchanged. This run answers both for three Codex homes,
with a stand-in for `iris mcp`. It changes nothing in Iris. It supplements
[the Codex attention measurement](codex-attention.md).

## Setup

- WSL 2, kernel 6.6.87.2-microsoft-standard-WSL2. Codex CLI 0.167.0, the installed release
  binary run in place. The homes were on ext4 under the user's home directory, not under `/tmp`:
  with a home under a temporary directory Codex refuses to create its `PATH` helper binaries,
  which would change the `PATH` this run looks at.
- `HOME` was a scratch directory in every case. "The default home" below therefore means
  `.codex` under that scratch `HOME`, never the user's own. The two custom-home cases also had a
  scratch `CODEX_HOME`. The environment was built from scratch: only `USER` came from the user's,
  `LOGNAME` was set from it, and `LANG` and `TERM` were fixed values. No credentials were present
  in it or in the configuration. For the processes the harness started, outside
  HTTP and HTTPS went to a closed port on loopback, with localhost exempt. Codex did not pass
  those proxy variables to the MCP child, so the child's `codex queue` ran without them. This is
  not network isolation enforced by the system.
- No TUI ran. The thread was loaded by the daemon for a queued message, as described below.
- The model was a scripted Responses provider on loopback (`wire_api = "responses"`, no auth).
  Its answers were fixed: a message for the seed, a call to the stand-in's tool for the warm-up,
  and a message for the wake. A request whose turn metadata did not say `request_kind: turn`
  got a plain message. MCP tools are deferred in Code Mode: they are not in the `exec` tool's
  description but are on its `tools` object and in `ALL_TOOLS`. The warm-up's answer therefore
  asked `exec` to find the tool in `ALL_TOOLS` and call it.
- The stand-in for `iris mcp` was a stdio MCP server with one tool. It recorded the names of the
  variables in its own environment and the values of `CODEX_HOME`, `HOME` and `PATH`, the `_meta`
  of its tool call, and its parent's command line. It then waited for a gate file and ran
  `codex queue --thread <sessionId> --resume --message <text>`, with `codex` found on its own
  `PATH` and with its own environment. It recorded the exit code, the output and the time. The
  text was ADR 017's fixed text with a marker in front and a random token at the end.
- Command hooks for SessionStart, UserPromptSubmit, Stop and SessionEnd recorded their payloads.
  Before each daemon started, a stdio `codex app-server` of the same home listed the hooks
  (`hooks/list`). It wrote each one's `currentHash` as `hooks.state.<key>.trusted_hash`
  (`config/batchWrite`), the form Iris's own check of a `/hooks` review looks for. Whether a
  review in the TUI writes exactly this was not compared. `hooks/list` then reported all four
  trusted. No measured run used `--dangerously-bypass-hook-trust`; the discovery runs that did
  are not counted.

## Sequence

For each case:

1. `codex app-server daemon start`. The daemon installed its own copy of the binary in the home.
2. `codex exec <seed>`, which created and saved the thread. Exec ends its session as soon as the
   turn is over: SessionEnd fired 28 to 46 ms after Stop. So exec cannot stand in for a session
   that stays open. What became of the exec's own MCP child was not followed.
3. The harness's own `codex queue --thread <thread> --resume --message <warm-up>`. The daemon
   loaded the thread and started a new MCP child for it. The warm-up turn called the stand-in's
   tool, whose `_meta` carried `sessionId`, `threadId` and the turn metadata.
4. Once the session's rollout file recorded the warm-up turn's `task_complete`, the harness
   published the gate file by writing a temporary file and renaming it.
5. The stand-in ran `codex queue`. The harness waited up to 90 s for the stand-in's result, then
   up to 30 s for a third turn in the rollout.
6. `codex app-server daemon stop`, then cleanup.

Delivery counted only with three kinds of evidence together: the rollout's `task_started`,
with its `turn_trigger`, and its `task_complete`; the provider's request, with its
`request_kind` and `turn_trigger`; and the hooks, UserPromptSubmit's `prompt` and a Stop after
it. The harness recorded each of them; they were judged together by reading the records, not
by an automated check. An exit status of 0 alone did not count.

The harness made itself a child subreaper, so the daemon, its updater and their children stayed
below it. It read the process table to find its own descendants and acted only on those, and it
read no process's environment.

## Results

| Case | `CODEX_HOME` in the child | The child's `codex queue` | Third turn | UserPromptSubmit `prompt` | Provider |
|---|---|---|---|---|---|
| Default home, no `CODEX_HOME` | absent | exit 0 in 32 ms: `Queued message <id> for thread <id>.` | trigger `queue`, completed | the queued text, unchanged | `request_kind` turn, trigger `queue` |
| Custom home, passed with `env_vars = ["CODEX_HOME"]` | the custom home | exit 0 in 31 ms, the same line | trigger `queue`, completed | the queued text, unchanged | `request_kind` turn, trigger `queue` |
| Custom home, not passed | absent | exit 1 in 277 ms: `thread/queue/add failed: failed to read thread: invalid thread-store request: no rollout found for thread id <id>` | none | none | none |

In the two delivered cases a Stop hook fired after the wake's prompt hook.

- **The child's parent.** In every case the armed child's parent was the daemon:
  `<home>/packages/app-server-daemon/releases/0.167.0-…/bin/codex app-server --listen unix://
  …`, the copy the daemon installs in its home. `daemon start` installed it from the CLI that
  started it, so here the daemon and the `codex` the child found were the same version, 0.167.0.
  They are two binaries, and nothing here shows that they are kept at the same version.
- **The child's environment.** It held `HOME`, `LANG`, `LOGNAME`, `PATH`, `TERM` and `USER`, and
  `CODEX_HOME` only where `env_vars` named it. Codex put four directories of its own before the
  inherited `PATH`: a directory of helper links and one holding `rg`, once for the daemon and
  once for the CLI that started it. The child found `codex` further along, where the harness's
  `PATH` put it.
- **The control.** Without `CODEX_HOME` the child's `codex queue` used `HOME/.codex`. Its queue
  database there held no queued item, and no turn ran within the 30 s the harness waited. As a
  side effect, the failed command created and initialised a home
  under `HOME/.codex`, with its databases and bundled skills, and no queued message in it. The
  failure is visible as a non-zero exit, which ADR 017 decision 3 counts as failing before
  submission.
- **What stays behind.** In every case the daemon's updater, `codex app-server daemon
  pid-update-loop`, was still running 15 s after `daemon stop`, and the harness sent it SIGKILL.
  The harness did not check afterwards that it was gone. A listing of processes after the final
  run found none from the cases; its output was not kept, so the evidence below cannot confirm
  it. The daemon's and the updater's sockets are in a shared directory, `/tmp/codex-daemon-<uid>/`.
  Each is named by the SHA-256 of the path of the link to it in the home, so homes do not collide.
  During development, runs that did not remove the sockets left both of them there after the
  stop; listings of the directory at the time showed this. The final harness removed its own
  through the case's links, ignoring errors, and a listing after the final run found the directory
  empty. The output of those listings was not kept, so the evidence below cannot confirm either
  observation. A live check that starts its own Codex daemon must do both itself: stop the
  updater, and remove the two sockets.

The three cases ran three times with the same outcomes, by the summary printed after each. The
earlier summaries were not kept, so this cannot be checked from the evidence below. Only the last
run's files are kept. The last run used the kept revision of the harness, which differs from the
earlier one only in recording every MCP message in full and in having the extra case below. The
figures here come from the last run.

## What Codex sends before the first tool call

A separate run of two cases, kept in its own directory, recorded every message each stand-in
received, whole. One case was the default home again. In the other the stand-in also offered
resources and prompts, to see whether Codex lists them during startup. Each case had two
MCP children, the exec's and the daemon's; all four received the same messages before a tool was
called:

| Message | What it carried |
|---|---|
| `initialize` | `protocolVersion`, `capabilities` and `clientInfo`: name `codex-mcp-client`, title `Codex`, version `0.167.0` |
| `notifications/initialized` | nothing |
| `tools/list` | `_meta` holding only `progressToken: 0` |

In these startups Codex asked for no resources, resource templates or prompts, even where they
were offered, and sent no ping. None of these messages carried the session's or the thread's id.
The first message that did was the daemon child's `tools/call`, in its `_meta`, as in the three
cases. In these runs, then, an MCP server of Codex 0.167.0 learned which session it serves only
from a tool call; other MCP configurations, transports and later requests were not tried. It did
learn the version of the Codex that started it, from `initialize`.

## The call's id beside the hooks' id

A binding from the tool call needs the call's `_meta.sessionId` to be the session id Codex gives
the hooks. A third run, with a revision of the harness that adds an ids mode and is kept beside
the first revision, looked at four other ways a session begins. Each case had its own scratch
home, the setup and hook trust described above, and a scripted provider like the one above, with
a script of its own in the sub-agent case; no daemon ran unless stated:

1. **Exec resume.** `codex exec` seeded a thread; `codex exec resume <thread>` ran a turn that
   called the stand-in's tool.
2. **Exec fork.** The same with `codex exec fork <thread>`.
3. **A restarted daemon.** After the seed, `daemon start`, `daemon stop`, `daemon start` again,
   then `codex queue --thread <thread> --resume` with a turn that called the tool, then
   `daemon stop`. The first daemon never loaded the thread.
4. **A sub-agent.** In one `codex exec`, the root agent called `spawn_agent` (namespace
   `collaboration`), then `wait_agent`; the sub-agent called the stand-in's tool and finished. Only
   this case also recorded SubagentStart and SubagentStop.

For each tool call, a script read the stand-in's and the hooks' records and set the call's
`_meta.sessionId` beside the `session_id` of the hooks whose `turn_id` was the call's
`x-codex-turn-metadata.turn_id`. It counted them equal only when those hooks showed exactly one
session id and it was the call's. The same script ran over the five daemon-loaded runs above: the
three cases and the two in `pre/`, whose warm-up turns were tool calls.

| Run | Hooks of the call's turn | Equal | Also |
|---|---|---|---|
| The five daemon-loaded runs above | UserPromptSubmit, Stop | yes, in all five | SessionStart `startup` (the seed), then `resume` |
| Exec resume | UserPromptSubmit, Stop | yes: the seed's id | SessionStart `resume` with the seed's id |
| Exec fork | UserPromptSubmit, Stop | yes: the fork's new id | SessionStart `fork` with the new id |
| A restarted daemon | UserPromptSubmit, Stop | yes | SessionStart `resume` |
| A sub-agent | SubagentStart, SubagentStop | yes: the **root's** id | the call's `threadId` was the sub-agent's own |

- **Thread and session.** Outside the sub-agent, every call's `threadId` equalled its `sessionId`.
  The sub-agent's call carried the root's `sessionId` with its own `threadId`, and its turn
  metadata said `thread_source: subagent`, `subagent_kind: thread_spawn`, and the root's thread as
  `parent_thread_id`; elsewhere `thread_source` was `user`. In every call the turn metadata's
  `session_id` was the call's `sessionId`, and `_meta.windowId` was the `threadId` followed by
  `:0`.
- **The sub-agent's hooks.** SubagentStart and SubagentStop carried the root's `session_id`, the
  sub-agent's thread as `agent_id` and `agent_type: default`. No UserPromptSubmit or Stop fired for
  the sub-agent's turn, and no SessionStart for the sub-agent.
- **The sub-agent's MCP children.** Two instances of the stand-in MCP server started under the
  one exec process, 174 ms apart, and the only tool call came after the second had started. Which
  of them served it was not recorded.

This run looked at none of: a thread resumed or forked in the TUI, a thread that stays loaded
while its daemon restarts, a sub-agent on the daemon, other app-server clients, and Iris's own
PreToolUse and PostToolUse hooks in a sub-agent's turn.

## What this does not show

- A thread started by a person in the TUI, with the thread's own MCP child running the queue. The
  record linked above measured the queue on a TUI's thread, and this run measured the child on a
  thread the daemon loaded without a TUI. Neither measured both together, and the environment the
  daemon gives a TUI thread's MCP child was not captured.
- A wake after the thread unloads, about 60 s after its last subscriber leaves, or after the
  daemon restarts.
- Other environments: for example `CODEX_HOME` passed while `HOME/.codex` runs a daemon of its
  own, or a `PATH` on which `codex` is not found.
- Other text and other wakes: in each case the stand-in tried to queue one wake of 156 bytes of
  ASCII. Longer or multi-line text, other characters, two wakes at once, a repeated wake and a
  wake queued while a turn runs were not tried.
- Iris's own hooks and accounting. The hooks here only recorded their input and gave no context.
- How long a wake takes or how reliable it is: 32, 31 and 277 ms are timings from single runs of
  the queue command. The control's absence of a turn holds for the 30 s the harness waited.
- A queue command that hangs: the stand-in ran it without a deadline, and none hung.
- macOS, Windows, a hosted model, how a model treats the text, and what a wake costs.
- Whether a queued message survives a crash.

## Evidence

The harness is one Go file: the provider, the stand-in MCP server and the hook probe. It and
`results.json` are kept on the WSL host, outside the repository, with these SHA-256 values:

- `main.go`: `6a230b3573dc4467789db1fcb7ab367042b0a369f131ef8cbae83f48370b2f1e`
- `results.json` of the three cases: `ff08fec6761e22b2a51681a3b29b6d37ed65c20937f9c924df3b8eeb9bf01122`
- `results.json` of the run that recorded every message: `2ff7e707a9eba0a33a0f8499851b9c25f49b295c401b6a3ae4e2fbf91c29a145`

Each case directory also kept the provider's requests, the hook payloads, the stand-in's log, the
rollout files and the queue result. The run that recorded every message is in a directory of its
own, `pre/`, with the same files.

The run that set the ids side by side used the revision of the harness with the ids mode, kept in
a directory of its own with its four case directories and their summary. The comparison is a
short Python script, kept with its output:

- the harness with the ids mode, `main.go`: `d7ca8b9e16b4460f55611fd8aedabf7ef9cbd08587e843ab70d62bd21dfe1acc`
- its summary, `ids.json`: `27da25e6f328153edd1479aa25fe49dc9883458bd1a80ab6ac2e611a1935f948`
- the comparison, `compare-ids.py`: `4a27b46f5aaa878a31de9fea2bff2e90c3868241f1e272f4912a049c0796476d`
- its output over the nine runs, `compare-ids.out`: `dd52b886db7ef42765081c66ca5f41ec394f0eeeeceb8f8a1ba3bc487e18e69d`

The captures this record names that Iris's tests replay are in the repository: the root's tool
call of the default-home case and the sub-agent's, in `internal/mcpserver/testdata/codex/`, and
that root session's SessionStart and the sub-agent's SubagentStart and SubagentStop, in
`internal/hook/testdata/codex/`. Their values are as recorded; machine paths are anonymised as in
the other captures.
