# 030: Wake Kimi by completing a one-shot background watcher

Status: proposed for opt-in implementation on the owner's direction, 2026-11-13. Changes D7 for
Kimi only. For a session open in the Kimi TUI it supersedes ADR 026; the web API remains only for
a positively identified session owned by `kimi web`. The attention identity and told-state rules of
ADR 017 remain in force.

## Problem

A Kimi TUI holds its session lock even while the model is idle. On Kimi 2.6.1, a prompt submitted
through `kimi web` then fails with `40931`, `SESSION_OCCUPIED`. ADR 026 therefore cannot wake the
interactive session it was meant to reach. Removing that lock through `:takeover` or
`KIMI_CODE_SESSION_LOCK=0` is not a handoff: either can let two processes interleave the session
history.

Kimi's managed background tasks provide another native path. A terminal task status starts a model
turn in an idle TUI, but output from a still-running task does not. Iris can use completion as the
wake primitive if its watcher exits after new attention and the model starts the next watcher before
ending that turn.

## Evidence

The measurements ran on Linux with `kimi --version` reporting `2.6.1`, in a new scratch TUI session
`session_ee43a525-3d86-413c-a78d-5e9e0c5d30dd`, not an existing user session. The session ran under
`tmux` as `env KIMI_CODE_TRUST_WORKSPACE=1 kimi --auto`; the environment setting avoids a persistent
trust record. Its event stream is
`~/.kimi-code/sessions/wd_kimi-wake-research-20261113_789e1fd6489a/session_ee43a525-3d86-413c-a78d-5e9e0c5d30dd/agents/main/wire.jsonl`.

First the model used Bash with `command:"sleep 20"`, `run_in_background:true` and
`description:"SCRATCH-COMPLETE"`, then ended without waiting. Exit 0 produced a `turn.prompt` with
`origin.kind:"task"` and `trigger:"notify"`; the model answered `SCRATCH-COMPLETE-OBSERVED`.

The second experiment separated output from termination. The background command was exactly:

```sh
while [ ! -f /home/klattm/projects/iris/.playwright-mcp/kimi-wake-research-20261113/emit-now ]; do sleep 1; done; printf SCRATCH-OUTPUT; sleep 20
```

- The arming turn ended at 18:28:48.464Z.
- The gate was created at 18:29:23Z. The 14-byte `output.log` contains `SCRATCH-OUTPUT` and has
  mtime 18:29:23.975Z.
- The TUI remained idle with `[1 task running]`. No turn or notification appeared while the final
  `sleep 20` ran.
- The task exited at 18:29:43.981Z. At 18:29:43.997Z a new `turn.prompt` had
  `origin.kind:"task"`, `trigger:"notify"` and turn id 3. The model answered
  `SCRATCH-EMIT-COMPLETED` and ended at 18:29:47.016Z.

The automatic message is a `<notification>` containing the task id, terminal status and, when output
exists, an output-file path. It is not a streaming-output notification. These results agree with the
[background-task documentation](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/tools.html).
The [hook documentation](https://www.kimi.com/code/docs/en/kimi-code-cli/customization/hooks.html)
makes `Notification` observation-only and limits `Stop` to a turn that is already ending. Plugins
package those same hooks; they add no documented idle-session push API.

The measurements do not establish behavior on macOS or Windows, task survival across `/clear`, or
every terminal status. Those are acceptance items below, not assumptions in the design.

## Decision

### One-shot command and bounded lifetime

`iris watch --client kimi --once` is a separate mode; the persistent Grok watch is unchanged. The
model starts it only through Kimi's managed background Bash, with `run_in_background:true`,
`disable_timeout:true` and a fixed Iris description. The disabled client timeout is necessary because
Kimi's default background timeout is ten minutes, but the watcher itself has an absolute 24-hour
lifetime. Its deadline produces a non-zero exit without an attention line; the resulting terminal
notification may cost one turn before the watcher is re-armed.

The watcher exits 0 only after it prints one fixed, content-free Iris line for new attention and
durably checkpoints that attention. The line asks the model to use the Iris inbox and claims tools;
it carries no message body, sender, token or native task output. Printing is not the wake: exit 0 is.
The watcher prints before checkpointing, as the persistent watch does. A checkpoint failure exits
non-zero and may replay attention on the next run; a duplicate wake is preferable to skipping it.

A transient hub or socket failure uses the existing bounded reconnect/backoff and consumes the same
24-hour lifetime. It does not print attention and does not start a new Execution. Parent-process
exit, wake being disabled, a changed native session, confirmed Execution loss, an ambiguous binding,
or the absolute deadline ends the watcher non-zero. There is no `:takeover`, lock bypass, detached
helper or listener after the watcher exits.

### A durable cursor, never an initial backlog wake

Each watcher has one positive start cursor and follows only `inbox_changed` and `claim_available`.
The per-session record retains its stream, checkpointed cursor, last emitted identities and
per-kind coalescing deadlines across one-shot processes. The hub consumer name is `iris-watch-kimi`,
distinct from other clients' persistent monitors.

- With no prior record, the initial snapshot establishes the current stream and high-water cursor.
  Existing unread mail and old freed paths do not print or exit. SessionStart, prompt hooks and the
  durable inbox already cover that backlog.
- With a prior record, replay starts after its checkpoint. Attention committed between completion
  and re-arm is therefore new to this watcher, not initial history. Before printing, Iris reads
  authoritative attention and the hook told state again. Attention already fetched or told is
  checkpointed without a wake; otherwise the fixed line is printed and the cursor is checkpointed.
- A same-stream reset uses the durable attention identity, not counts. Only an identity newer than
  the saved cursor can qualify. A snapshot with no newer pending identity merely re-anchors the
  watcher. If the evidence is too old to make that comparison, Iris conservatively re-anchors
  without exiting and relies on the durable inbox and hook reminders for existing attention.
- A changed stream invalidates told state as ADR 017 specifies. When a prior consumer record exists,
  currently pending attention in the new stream may be announced once. A reset during a first-ever
  start still establishes only a baseline and never wakes for history.

The first candidate after an idle period is eligible immediately. The existing 30-second coalescing
window survives re-arming through the per-session record: later candidates wait out the remainder,
and an authoritative recount suppresses one already handled. A candidate is not postponed forever by
new events. Checkpointing covers every event folded into the emitted line. A wake never marks
attention told; only a hook that emits its note does so.

### Per-process state and one Stop request

SessionStart records the native session id beside the `(kimi pid, start time)` of its nearest
single-session Kimi ancestor, as Grok's session file does. Wake state lives outside `sessions/` so
pruning cannot remove a live record, in a `0700` directory with `0600` files and the corresponding
owner-only protection on Windows. Records are keyed by `(kimi pid, start time, native session id)`;
`/clear` cannot inherit the old session's watcher or cursor.

A record holds an `arm_requested` marker or a live `(watcher pid, start time, attempt id)`, plus the
cursor and coalescing state above. Process checks use both pid and start time and reject dead and
zombie processes. Every mutation is conditional under the local state lock; cleanup removes only
the record that still carries the exiting watcher's attempt id.

When wake is enabled and no live or requested watcher exists, an ordinary Kimi `Stop` writes
`arm_requested` with its time and returns the client's blocking continuation. Its stderr asks the
model once to start exactly `iris watch --client kimi --once` through background Bash with the
settings above, then end the turn. Kimi 2.6.1 has no documented Stop task list or `stopHookActive`
field for this purpose, so the local marker prevents repeated continuations. A successful watcher
publishes its live attempt before waiting and clears the request. A later Stop inside a five-second
publication grace period allows without another request; after it, a request with no live attempt
latches `failed` instead of asking forever. SessionStart or an explicit user request can clear that
latch.

A second watcher for the same key never opens a stream or moves a cursor: it exits non-zero while
preserving the live attempt. Stop performs only local file and process checks, calls no hub and
fails open on errors as D3 requires. The Iris skill tells the model that a terminal notification for
this task means to fetch inbox and relevant claims, then leave one replacement watcher armed before
ending. This is model cooperation, not verification of the command it chose to run.

### Lookup-only binding from the Bash child

A managed Bash child has the Kimi process as its nearest client ancestor. The watcher therefore uses
rung 3 of D6, through a lookup-only resolver analogous to the Antigravity wake resolver. It reads
the expected native session id from the hook's per-process record and requires the bind response to
name that same `session_id`, with hook evidence for the Execution and generation.

`IRIS_EXECUTION`, an explicit handle, an MCP-only fallback, adoption, an ambiguous process match and
a missing or changed native session id cannot arm the watcher. It never registers a fallback
Execution. Once bound it pins `(Execution, generation, native session id)` and heartbeats only that
identity. It does not rebind to a later session or generation: after loss, the hook must reattach
and a later turn must arm a fresh watcher.

### Completion-to-re-arm gap

Mail and claim events committed after the last checkpoint remain durable. A replacement watcher
starts from that checkpoint, so the gap does not skip their event identities. The model may fetch or
a hook may announce them before the replacement prints; the authoritative attention/told read then
suppresses a duplicate. A reset whose evidence cannot establish ordering may suppress a native wake,
but it never acknowledges mail or removes it from the inbox. Ordinary prompt reminders remain the
backstop for that case and for a model that never re-arms.

### Opt-in and the ADR 026 web path

Wake remains opt-in. Fresh `iris integrate kimi` is off; `--wake` enables the appropriate native
route, ordinary setup and reintegration preserve the explicit choice, and `--no-wake` or removal
disables both routes and stops only recorded watchers whose process identity still matches. Existing
children retain their startup arguments until restarted.

For a hook-registered single-session TUI, `--wake` installs the Stop request and skill, and the MCP
web worker is not used. ADR 026 remains only for a positively identified `kimi web`-owned session
whose own binding and native idle checks pass. The SessionStart adapter records `client_type` and
the nearest Kimi ancestor. A live `$KIMI_CODE_HOME/server/instances/` entry verified against that
ancestor's pid and start time is positive web-owned evidence. The captured `kimi_code_cli` value,
an eligible single-session ancestor and a complete registry scan proving that it is not such a web
server select the task route. Unreadable or incomplete surface evidence arms neither route; a bind
response or `40931` alone is not a surface discriminator.

The per-session route choice is held under the same local lock as watcher state. Reserving a route
and initiating its task or web prompt are atomic with respect to the other route; waiting for the
task or prompt response happens outside that lock. A session therefore never has an armed one-shot
watcher and an outstanding web prompt at once. No web-to-TUI prompt attempts remain, and a web-hosted
task route is not enabled until it passes the same native acceptance as the TUI.

## Limits

- This is a request to the model, not enforcement. A model can ignore the Stop continuation, start
  the wrong command, fail to fetch mail or fail to re-arm. A failed latch prevents a continuation
  loop but leaves hook reminders as the only notification until a later successful arming.
- Nothing starts a closed Kimi session. Background tasks normally stop when their session closes;
  no external task survives to deliver a completion. `/clear` survival requires measurement, and
  stale watcher identity is never transferred to the new session.
- A busy-turn notification may be steered into that turn or open another one. An empty or duplicate
  billable turn remains possible. Non-zero exits, kills, notification ordering while busy, task-count
  limits and their model cost are acceptance measurements, not guarantees here.
- The 24-hour bound can cause one terminal-notification turn per day without new mail. Transient hub
  outages spend no turn by themselves, but consume that lifetime.
- Output from a persistent `iris watch` cannot wake Kimi. An external process started outside managed
  background Bash has no managed terminal notification and is not a substitute.
- Only Linux with Kimi 2.6.1 was measured. The REST web route keeps ADR 026's experimental API and
  version limits for the web-owned sessions it can positively identify.
- Message bodies remain untrusted data. Neither the watcher line nor the native task notification
  carries them or chooses what the model does with them.

## Rejected

- **A persistent watcher whose output lines wake Kimi.** Output alone produced no turn in the
  controlled measurement.
- **Taking or disabling the TUI lock.** Both permit concurrent writers and interleaved history.
- **One shared native route selected by guessing the client surface.** Unknown evidence arms no
  route; the TUI and web-owned sessions must not race two wake mechanisms.
- **Polling through `CronCreate`.** It can inject a scheduled prompt, but spends turns without new
  attention and is not event-driven delivery.
- **An externally spawned watcher.** It is not a Kimi managed task and cannot provide the measured
  completion notification.

## Verification

Implementation tests at the repository's required coverage must prove:

- exact argument and 24-hour lifetime handling; exit 0 only after fixed-line output and checkpoint;
  no registration, override, fallback, adoption, rebind or lock bypass;
- initial backlog suppression, replay after a saved cursor, authoritative attention/told suppression,
  same-stream reset ordering, new-stream behavior and checkpoint failure replay;
- coalescing across sequential watchers, conditional attempt ownership, duplicate rejection, stale
  pid/start rejection, `/clear` isolation, opt-out cleanup and fail-open Stop behavior;
- positive TUI versus web surface selection and mutual exclusion with ADR 026;
- all ordinary watch invariants: relevant event kinds only, no content in output, no cursor
  regression and no missed replay between completion and re-arm.

The native acceptance runs on a fresh scratch session, never the owner's active session:

1. Enable wake and end a turn. Confirm that Stop arms one managed task, its rung-3 binding names the
   scratch session, no fallback Execution appears and a duplicate request leaves one watcher.
2. Send one message. Measure event-to-exit and exit-to-`turn.prompt` latency, capture the exact
   notification text, confirm `origin.kind:"task"` and `trigger:"notify"`, and require the woken
   model to fetch the message. Before that turn ends, prove that exactly one replacement is armed.
3. Repeat for a freed claim and for a second message sent during the completion-to-re-arm gap. The
   durable inbox plus replay must produce no loss and no duplicate native turn after told/fetch
   suppression.
4. Measure completion with exit 0, non-zero exit, kill and output while running; measure a notification
   arriving during a busy turn. Record whether it is queued, steered or folded and what text reaches
   the model. Measure task-count limits, `/clear`, session close and the 24-hour deadline with a
   shortened test clock.
5. Run the equivalent scratch test on a `kimi web`-owned session before enabling its task route, and
   prove that the REST and task routes cannot both submit for one session.

Run `make test`, `make race`, `make cov`, `make cov-check`, `make check-all` and `make fixtures`
before implementation review. Record real model turns separately from fixtures, including platform,
client version, latency and observed cost. No implementation is authorized by this draft before
review.
