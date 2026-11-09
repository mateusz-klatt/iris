# Codex attention measurement (0.167.0)

2026-11-09, Linux. This supplements the [earlier Codex report](codex.md) with measurements of
shell-hook context, Stop continuation, bounded MCP waiting, explicit app-server control and
the default daemon's queue. It does not change Iris behavior or establish support on other
platforms.

## A. Evidence and isolation

**[MEASURED]** The installed client reported `codex-cli 0.167.0`. These experiments ran the real
CLI, PTY TUI and app-server against a scripted Responses provider on loopback. Its model label
was `gpt-6-luna`, reasoning effort low; its assistant output and usage counters were artificial.
No hosted model calls were made. The evidence establishes client transport and lifecycle,
not model reasoning, compliance, actual token cost or consumption of Iris mail.

Each experiment used its own `CODEX_HOME`, Git workspace, provider and threads. No owner thread,
production hub or timer was used or changed. No credentials were supplied or copied, and no
authenticated model calls were made. Daemon commands were scoped
to the experiment's home. PID ownership was checked before stopping processes; daemon updater
children were included in cleanup. No project builds, test suites or deployment ran.

The child environments omitted inherited agent identity and API credential variables. Final
runs also routed external HTTP(S) through an unavailable loopback proxy, exempting localhost.
An early client launch attempted unauthenticated remote plugin metadata and received HTTP 401;
later logs show such startup requests failing through the proxy. This was not OS-enforced
network isolation. Early runs retained the ordinary process home while replacing `CODEX_HOME`;
later hook runs also used an isolated process home and launch directory. No filesystem trace
proves absence of every incidental read. A queue isolation trace did verify that daemon
PID/settings/package/control-socket paths resolved within that experiment's `CODEX_HOME`.

In this note, `<SCRATCH>`, `<WORKSPACE>`, `<PORT>`, `<THREAD>`, `<TURN>` and `<UUID>` replace
machine-specific paths, ports and identifiers. Marker strings are synthetic. Commands preserve
the measured flags and arguments with those substitutions. They describe captured experiments;
the local scripted provider and PTY driver must also be supplied to reproduce them.

**[DOCS]** Installed help and generated experimental schemas were inspected before consulting
public [hook documentation](https://learn.chatgpt.com/docs/hooks),
[app-server documentation](https://learn.chatgpt.com/docs/app-server) and
[MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).
Schema availability alone is not a measured runtime guarantee.

## B. Configuration and commands

The scripted provider configuration was:

```toml
model = "gpt-6-luna"
model_provider = "probe"
model_reasoning_effort = "low"
approval_policy = "never"
sandbox_mode = "read-only"

[model_providers.probe]
name = "Scripted local provider"
base_url = "http://127.0.0.1:<PORT>/v1"
wire_api = "responses"
requires_openai_auth = false

[projects."<WORKSPACE>"]
trust_level = "trusted"
```

Hook experiments enabled `[features] hooks=true`. Their JSON contained command handlers with
explicit timeouts, for example:

```json
{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"python3 <SCRATCH>/hook_probe.py <RUN> PostToolUse","timeout":3}]}]}}
```

The exec and standalone TUI hook probes used these commands:

```text
codex --no-daemon exec --skip-git-repo-check --dangerously-bypass-hook-trust --json -C <WORKSPACE> 'Run the synthetic shell probe once, then finish.'
codex --no-daemon --no-alt-screen --dangerously-bypass-hook-trust -C <WORKSPACE> 'Run the synthetic shell probe once, then finish.'
```

**[MEASURED]** The invocation-only trust bypass emitted a warning. It was used only for reviewed
synthetic hooks and does not establish normal installation trust. The successful default-daemon
TUI queue experiment instead used normal hook review, described in section F.

The provider requested a harmless shell operation through the client's actual Code Mode shape:

```json
{"type":"custom_tool_call","name":"exec","namespace":"functions","call_id":"call_1","input":"text(await tools.exec_command({cmd:\"printf SYNTHETIC_SHELL_OK\"}));"}
```

The actual request exposed the namespace in an `additional_tools` input item. Provider captures
separate main requests from automatic title-generation requests. A title request must not be
counted as another user turn or converted into a real token-price estimate.

## C. Three attention tiers

| Tier | Measured client path | What the person sees or can do | Remaining boundary |
| --- | --- | --- | --- |
| Mid-turn | Shell PostToolUse context reaches the next provider request in exec and TUI; exec also confirms PreToolUse | Existing turn continues | Iris does not gain this behavior merely from the client supporting it; real attention-note rendering and normal Iris hook trust were not tested |
| Turn boundary | Stop block supplies another request | TUI shows “Blocked by hook”; Escape/Ctrl-C can cancel a sleeping Stop and a new prompt works | Integration must supply its own bound and event deduplication |
| Idle | CLI queue wakes a native TUI-originated thread on the default daemon; explicit app-server control is an alternative | Queued input and reply appear in TUI | Real Iris event relay, queue cancellation and cross-platform behavior remain unverified |

**[UNVERIFIED]** These are candidate integration surfaces. They do not demonstrate a production
Iris notification causing an inbox fetch or an agent handling a delivery. Hook execution,
client acceptance, provider receipt, fetch, acknowledgement and handling are distinct stages.

## D. Hook context, Stop and bounded MCP waiting

### Shell and model-request hooks

**[MEASURED]** Valid `hookSpecificOutput.additionalContext` from shell PreToolUse and PostToolUse
appeared as separate developer-role messages in the immediately following main request in exec.
Both markers remained in the subsequent request. Payloads identified the shell tool as `Bash`.
An independent TUI capture confirmed PostToolUse context in its next main request; an intervening
request was title generation.

**[MEASURED]** PreModelRequest and PostModelRequest each fired three times in the successful exec
run. Pre payloads included session/turn identity, cwd, transcript and model; Post added status,
error and usage. Their deliberately returned context markers appeared in none of the provider
inputs. Only completed/error-null outcomes were exercised, not provider retries or failures.

**[DOCS]** OpenAI describes model-request hook output as diagnostic and unable to inject context
or block a request. The measured cases agree. PostToolUse is a context surface.

### Stop continuation and interruption

The first Stop handler returned:

```json
{"decision":"block","reason":"ATTENTION_PROBE_STOP_CONTINUE_ONCE: check the synthetic notification"}
```

**[MEASURED]** This produced one more main request. The reason was a user-role
`<hook_prompt hook_run_id="…">` message. It did not cause another UserPromptSubmit hook.
The first Stop had `stop_hook_active:false`; the continued turn's Stop had `true`. Returning an
empty object then ended the turn. TUI showed “Blocked by hook” before continuing.

**[MEASURED]** A separate bounded experiment returned block three times. Its Stop flags were
`false,true,true,true`, and all three blocks caused additional requests. The client's flag is
not an automatic one-continuation guard; the experiment's own counter stopped it.

**[MEASURED]** A five-second Stop sleep with a one-second configured timeout was canceled;
SessionEnd followed hook start by approximately 1.198 seconds, no continuation occurred, and
exec exited 0. SIGINT during a ten-second sleep triggered Interrupt about 333 ms later, canceled
the hook, produced no continuation and exited 1.

**[MEASURED]** In TUI, Escape and Ctrl-C during a ten-second Stop sleep triggered Interrupt after
approximately 181/188 ms. A subsequent synthetic human prompt fired UserPromptSubmit and made
one new main request. The interrupted Stop reason was absent and the new turn's Stop flag was
false. Both experiments then exited normally. These are individual timing observations, not
latency percentiles or service-level guarantees.

### Parked MCP call

An own stdio server exposed a read-only `parked/wait` tool. The provider selected it through the
client's actual tool catalog. Configuration explicitly set `tool_timeout_sec` to two or fifteen
seconds. **[DOCS]** The documented default is 60 seconds.

**[MEASURED]** A requested 300 ms wait returned after 301 ms; its marker entered the next
provider request as tool output. PreToolUse and PostToolUse both fired for `mcp__parked__wait`.
A five-second wait with a two-second timeout received `notifications/cancelled` after 2.002
seconds. The next request contained a deadline tool error; no PostToolUse fired on that failure.
Exec continued and exited 0.

**[MEASURED]** SIGINT during a ten-second MCP wait caused cancellation and Interrupt, with no
second model request and exec exit 1. TUI Escape caused cancellation approximately 7 ms after
the keypress; a new prompt then reached the provider and the TUI exited normally.

**[UNVERIFIED]** This establishes a bounded active waiting state. It does not wake a completed
turn, prove indefinite parking or ensure a real model will re-enter the wait. Repeated timeouts
can create model requests, so parking is not proven cost-free.

## E. Explicit app-server control

**[MEASURED]** An own server was started with:

```text
codex app-server --listen unix://<SCRATCH>/control.sock
```

The Unix endpoint required HTTP Upgrade and WebSocket framing. The controller sent `initialize`
with `experimentalApi:true`, then `initialized`, created a thread and completed a warmup turn
to persist its rollout. The TUI attached using:

```text
codex --remote unix://<SCRATCH>/control.sock --no-alt-screen resume <THREAD>
```

**[MEASURED]** A second client issued `turn/start` to the idle thread. The TUI displayed its
external input, working state and scripted reply. A matching `turn/steer` during a delayed
response was accepted; its marker reached a later provider input and appeared in the TUI.
Both controller inputs had user-message framing.

**[MEASURED]** A wrong `expectedTurnId` was rejected with error `-32600`; a stale steer after
completion was rejected with `no active turn to steer`. Neither rejected marker appeared in
provider inputs. Repeating `turn/start` after completion with the same `clientUserMessageId`
created two distinct completed turns and two user-message entries carrying that same client ID.
That field is not an idempotency guarantee for this sequential retry case.

**[MEASURED]** Socket mode was 0600 and its scratch parent was 0700. This is same-user Unix access,
not an authorization boundary between applications belonging to that user. The explicit server
and remote TUI result alone does not prove attachment to an arbitrary default-daemon session.

## F. Queue through the default daemon

The successful native TUI sequence used the ordinary local-daemon path:

```text
codex app-server daemon version
codex app-server daemon start
codex --no-alt-screen IRIS_QUEUE_WARMUP
codex queue --thread <THREAD> --resume --message IRIS_QUEUE_IDLE_WAKE
codex app-server daemon stop
```

All commands used the same private `CODEX_HOME`; the TUI's process cwd was `<WORKSPACE>`, set by
the harness rather than a `-C` argument. The initial version command failed because the
own control socket did not yet exist. Start returned backend `pid`, a scratch socket path and
managed client version 0.167.0. There was no `--remote`, `--no-daemon` or hook-trust bypass on the
successful TUI command. A PTY driver answered terminal cursor queries and trusted only the
scratch workspace and its four reviewed synthetic hooks through the normal client dialog.

**[MEASURED]** The TUI-created thread appeared in `thread/loaded/list`. Queue returned exit 0 and:

```text
Queued message <QUEUED_SUBMISSION> for thread <THREAD>.
```

The evidence of wake is stronger than that exit code: a new turn actually ran; UserPromptSubmit
fired with the same session ID and a new turn ID; the provider received a user-role marker;
the turn completed; and its queued input and reply were visible in TUI. Stop fired for that
turn, while SessionStart was not repeated. The hooks reached this state through normal trust
approval of the synthetic hooks. This does not verify the owner's actual Iris installation.

**[MEASURED]** A separate controller-owned, headless default-daemon thread accepted two queue
submissions while its original response was delayed. `thread/queue/list` showed ONE before TWO.
They later became separate completed turns in that order, and the queue emptied. The loaded
thread also ran a queued turn without a TUI while the observing controller remained connected.
This is queuing after the active turn, not steering its current response or merging both
submissions into one prompt. It does not prove an ordering guarantee for every race/restart.

**[DOCS, local help]** `--resume` attempts resumption; failure can warn while queue submission
still exits successfully. Queued messages use the thread's model. CLI help did not expose list
or withdrawal commands. Generated experimental schemas declared `thread/queue/list` and
`thread/queue/delete`; only listing was exercised in these initial runs. Schema declarations
for update/reorder/start are not measured support claims.

## G. MCP process lifetime

An own stdio MCP server logged process start, requests, signals and exit, and returned its PID
and an incrementing call counter. It was actually initialized and called. The client used its
default daemon, normal review of four synthetic hooks, an isolated home/workspace and the local
scripted provider. External queue commands explicitly selected that same private `CODEX_HOME`.

**[MEASURED]** In the first finite run, the same child PID and process-start identity survived
completed-turn TUI idle, graceful Ctrl-D and two queued turns without a TUI. Its returned call
counters increased 1→2→3. No observer connection remained open during the idle windows.
Short observer connections used only initialize, read and list, not thread resume/subscription.

| State | Observation |
| --- | --- |
| TUI open after completion | Same MCP child throughout 20.56 seconds idle |
| TUI closed | Same child throughout 20.04 seconds with no observer; thread still loaded |
| First headless queued turn | Same child served the next call, then survived 20.04 seconds idle |
| Second headless queued turn | Same child served another call, then survived 10.02 seconds idle |
| Explicit own daemon shutdown | Child received SIGTERM and exited |

**[DOCS]** The [app-server documentation](https://learn.chatgpt.com/docs/app-server#unsubscribe-from-a-loaded-thread)
describes a default 60-second idle grace after the last subscriber leaves, configurable through
`thread_unload_delay_secs`. Its read operation does not resume or subscribe to a thread.
No grace-period override was used. The short windows above do not prove indefinite retention.

**[MEASURED]** A second run tested that boundary directly. After a completed MCP call and graceful
TUI close, there were no observer connections or turns for 70.2459 seconds. The original child
received SIGTERM and exited 60.0076 seconds after Ctrl-D. At the end of the window,
`thread/loaded/list` was empty and `thread/read` reported `notLoaded`.

One `codex queue --thread <THREAD> --resume --message <MARKER>` on the same still-running daemon
then loaded the same persisted thread and launched a new MCP PID/process-start identity. That
child initialized and served its first identity call with its counter reset to 1; the queued
turn completed. Hooks recorded SessionEnd on unload, SessionStart with `source=resume` on reload,
then UserPromptSubmit/Stop with the same session identity. Thus the child was retained across
turns while loaded, and replaced after actual thread unload/reload in this experiment.

**[MEASURED]** MCP initialization carried client/protocol metadata but no thread/session ID.
Actual tool calls carried matching `_meta.threadId`, `_meta.sessionId` and nested
`x-codex-turn-metadata` identity. Calls caused by queueing reported `turn_trigger:"queue"`.
This is positive request metadata, not a basis for inferring thread identity from a daemon PID.

**[UNVERIFIED]** The generic child did not itself invoke queue or subscribe to Iris. Its inherited
`CODEX_HOME` routing environment was not captured. A production MCP-owned wake command therefore
still needs qualified client-home/daemon routing and positive thread binding. Session-owned
delivery is possible only while that child exists; after unload or daemon shutdown, recovery
requires an external mechanism. All own children, daemons and updaters were verified absent
after cleanup.

## H. Persisted threads and daemon recovery

**[MEASURED]** An ordinary TUI created and persisted a thread. After the TUI/controller
disconnected, the verified own daemon and updater were stopped. The socket disappeared and the
own-process scan became empty. A clean daemon start then returned an empty `thread/loaded/list`
before this command:

```text
codex queue --thread <THREAD> --resume --message IRIS_RECOVERY_RESTARTED
```

It returned 0 without a failed-resume warning, loaded the persisted thread, completed a new turn
and emptied its queue. Hooks recorded SessionStart with `source=resume`, then UserPromptSubmit
and Stop, retaining the original session ID. Resume SessionStart happened after the queue CLI
had returned. This submission was made after restart; it does not test a queued item caught
in a daemon crash.

**[MEASURED]** The daemon/updater were then stopped again. Immediately before offline queueing,
the own-process scan was empty, socket absent and daemon-version command failed to connect:

```text
codex queue --thread <THREAD> --resume --message IRIS_RECOVERY_STOPPED_ONE
codex queue --thread <THREAD> --message IRIS_RECOVERY_STOPPED_TWO
```

Both commands returned 0 and distinct queued-submission IDs. The first additionally warned:

```text
Warning: message was queued, but could not resume thread <THREAD>: start a local daemon or specify a server with --remote
```

After the first submission, the daemon remained absent and no matching provider request occurred
during a six-second observation. The second submission also did not start the daemon. Thus
`queue --resume` did not start the daemon in this configuration. Explicit
`codex app-server daemon start` left the thread unloaded; `thread/queue/list` showed both exact
queued IDs/texts in their original order. Persistence is therefore demonstrated beyond the CLI
processes and through later daemon startup, rather than inferred from their exit codes.

Finally, ordinary `codex --no-alt-screen resume <THREAD>` ran both queued messages as distinct
completed turns in FIFO order, displayed both replies and emptied the queue. One resume
SessionStart and both UserPromptSubmit/Stop pairs retained the original session ID. Provider
payloads showed user-role messages.

| State before dispatch | Submission persisted | Turn execution observed |
| --- | --- | --- |
| Running restarted daemon, persisted thread unloaded | Yes | Queue resume loaded the thread and completed a turn |
| Daemon stopped, queue with resume | Yes, with failed-resume warning | No; daemon stayed absent |
| Daemon stopped, queue without resume | Yes | No daemon start |
| Explicit daemon start after offline queueing | Both prior IDs/texts still listed | Thread remained unloaded until resumed |
| Native TUI resume of that thread | Queue later empty | Both messages completed separately and visibly, FIFO |

**[UNVERIFIED]** Forced-crash/power-loss durability, exactly-once retry after an ambiguous queue
response, failures during consumption, archived/deleted threads, withdrawal and cross-host
behavior were not measured. The queue acceptance, daemon availability and turn-completion
boundaries must remain distinct in an integration design.

## I. Cost, data roles and integration limits

**[MEASURED]** Hook additional context used developer-role input. Stop reasons and queue/control
submissions used user-role input. Ordinary MCP results remained tool output. A notification
bridge must therefore send fixed Iris-owned text through Stop/queue; sender-controlled subjects
or bodies must be fetched as untrusted data rather than promoted into that instruction channel.
These role observations do not prove prompt-injection resistance or model compliance.

**[UNVERIFIED]** No real token-price measurement was made for either short or long threads. A wake
that finds no pending work can still incur a model request and output; some captured flows also
generated thread titles. Artificial provider counters cannot price that work. Adding context to
an already scheduled request avoids forcing a new turn but can still add input tokens.

At Iris source baseline `4840765fbd672dbb82921bb4793795aa1136b9e7`, told-counts suppress repeated
unread/available counts, not event identities. Reading an old message and receiving a new one
between samples can leave the count unchanged. A continuation/wake design needs an explicit
delivery bound, retry state and cancellation policy. A monitor heartbeat does not prove native
delivery. The [gap re-audit](../gap-reaudit-2026-11-09.md) records these product-source limits.

No experiment here changed production attention behavior. Actual Iris WebSocket-to-queue relay,
multiple competing wake owners, real inbox handling, unattended credential/trust failures,
cross-host queue control, Windows/macOS behavior and real-model cost remain unverified.

## J. Confounded attempts retained

These failures were retained in the research evidence rather than treated as client defects:

- Initial scripted shell output missed Code Mode namespace handling. It did not prove shell
  hook support or failure; later correct payloads did.
- Initial PreToolUse output used an invalid permission-decision value. Missing context in that
  attempt was a harness error; corrected context-only output succeeded.
- Sending text and Enter in one PTY burst left pasted text unsubmitted. Separating typing from
  Enter by 0.5 seconds made post-interruption user input work.
- Raw transport assumptions failed against the Unix app-server WebSocket endpoint. Correct
  HTTP Upgrade and framing succeeded.
- TUI resume before the first persisted rollout failed. Completing an own warmup turn before
  attachment corrected the probe.
- A native TUI could not attach to a controller-owned default-daemon thread: it reported the
  conversation open in another app. This is not evidence of a successful native TUI queue wake.
- A TUI started with the hook-trust bypass ran its warmup but was absent from a separately
  started daemon's loaded list. Bypass was a confounding difference; no causal claim is made.
- Removing bypass initially stopped at hook review. Approving only the own synthetic hooks
  through the normal dialog produced the successful native TUI result in section F.

## K. Evidence index

The private research archive retains exact argv/config, raw hook stdin/output, provider requests,
PTY transcripts, RPC traffic, process ownership/cleanup and selected summaries. Identifiers and
machine paths are anonymized above. Logical capture names below identify the retained evidence;
they are not repository files or production-client fixtures.

| Capture | Principal evidence |
| --- | --- |
| `hooks/runs/exec-04-home-isolated` | Correct shell hook context, model-request diagnostics, Stop continuation |
| `hooks/runs/tui-01` | Independent TUI PostToolUse context; invalid PreToolUse output is excluded |
| `hooks/runs/exec-stop-repeat`, `hooks/runs/exec-stop-timeout`, `hooks/runs/exec-stop-sigint` | Repeat bound, configured timeout, interruption |
| `hooks/runs/tui-escape-enter`, `hooks/runs/tui-ctrlc-enter` | TUI Stop interruption and subsequent user input |
| `hooks/runs/mcp-short`, `hooks/runs/mcp-timeout`, `hooks/runs/mcp-sigint`, `hooks/runs/mcp-tui-escape` | Bounded stdio MCP result, deadline and cancellation |
| `appserver/run-1794245001`, `run-1794245129` | Visible remote-TUI wake/steer, rejected stale input, duplicate start |
| `queue/run-1794245526` | Headless default-daemon FIFO, list, loaded-without-TUI queue |
| `queue/run-1794246284` | Normally trusted native TUI idle queue and same-session hooks |
| `queue/recovery-1794246640` | Persisted unloaded thread, offline queue, failed-resume warning and later FIFO consumption |
| `lifecycle/run-1794246665` | Same MCP child across loaded-thread idle and multiple queued turns |
| `lifecycle/run-1794247001` | Default inactivity unload, child exit, same-thread queue reload and new MCP child |

Captured client behavior against a scripted provider must not be promoted into an authenticated
model canary or a claim that sender-controlled mail was read or handled.
