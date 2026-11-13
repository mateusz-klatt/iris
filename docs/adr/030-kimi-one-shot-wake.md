# 030: Wake Kimi by completing a one-shot background watcher

Status: proposed for opt-in implementation on the owner's direction, 2026-11-13. Changes D7 for
Kimi only and supersedes ADR 026 entirely. No MCP web worker is started for any Kimi session. The
attention identity and told-state rules of ADR 017 remain in force. After native acceptance, the
owner's later direction makes wake enabled by default where the implementation is verified live.

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
- The task exited at 18:29:43.981Z. Its `task.terminated` record was at 18:29:43.996Z and a new
  `turn.prompt` followed at 18:29:43.997Z: 16 ms from process exit and 1 ms from the terminal event.
  The turn had `origin.kind:"task"`, `trigger:"notify"` and turn id 3. The model answered
  `SCRATCH-EMIT-COMPLETED` and ended at 18:29:47.016Z.

Follow-up measurements used new scratch sessions under
`~/.kimi-code/sessions/wd_kimi-wake-measure-20261113_2cb0ef9fcff6/`:

- **Non-zero exit.** `sleep 15; exit 7` produced `status:"failed"`, `exitCode:7` and a warning
  `<notification type="task.failed">` with `Title: Background process failed`. The process ended at
  20:08:45.422Z, `task.terminated` was at 20:08:45.428Z and the new `task/notify` turn at
  20:08:45.429Z. The model saw the notification and answered `SCRATCH-NONZERO-OBSERVED`.
- **SIGTERM.** A scratch `sleep 120` received `kill -TERM 58581` at 20:10:00Z. It ended as
  `status:"failed"`, `exitCode:-1`, not `killed`; `task.terminated` was at 20:10:00.197Z and the new
  turn at 20:10:00.198Z. The model saw the same failed-notification form and answered
  `SCRATCH-KILL-OBSERVED`.
- **Busy turn.** The agent started `sleep 10; printf SCRATCH-BUSY-DONE` in the background and then
  ran foreground `sleep 20`. The background process ended at 20:11:05.384Z. `task.terminated` and
  `prompt.steered` were recorded at 20:11:05.391Z and .392Z, while the foreground tool was still
  active. The active turn ended at 20:11:20.998Z and a separate `task/notify` turn began at
  20:11:20.999Z. The notification was not lost or folded into an earlier model request; it waited
  about 15.6 seconds for the active turn to finish.
- **`/clear`.** A new session started `sleep 60` as `SCRATCH-CLEAR2`. `/clear` changed the session id
  from `session_5a3fcb93-161e-418e-a07d-adafc467a7f5` to
  `session_638c5a91-9064-450e-8a7a-6f48c85d9186`. The old session recorded `status:"killed"` at
  20:14:03.798Z. The process was gone, and neither session had a notification or a new turn for it.
  An earlier attempt raced a task notification and was discarded as evidence.
- **Task count.** The model started 24 managed `sleep 300`/`sleep 600` tasks without a rejection.
  The scratch TUI then disappeared without an error in its session log; all 24 `sleep` processes
  remained alive and were cleaned up by their exact recorded pids and commands. This establishes a
  lower bound of 24, not a maximum or a causal connection to the TUI's exit.

The automatic message is a `<notification>` containing the task id, terminal status and, when output
exists, an output-file path. It is not a streaming-output notification. These results agree with the
[background-task documentation](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/tools.html).
The [hook documentation](https://www.kimi.com/code/docs/en/kimi-code-cli/customization/hooks.html)
makes `Notification` observation-only and limits `Stop` to a turn that is already ending. Plugins
package those same hooks; they add no documented idle-session push API.

These measurements cover exit 0, non-zero exit, SIGTERM, a busy turn and `/clear` on Linux with Kimi
2.6.1. TaskStop-specific behavior, the maximum task count, macOS, Windows and the cause of the
unexpected TUI exit remain unmeasured. The acceptance section separates implementation blockers from
follow-up measurements.

## Decision

### One-shot command and bounded lifetime

`iris watch --client kimi --once` is a separate mode; the persistent Grok watch is unchanged. The
model starts it only through Kimi's managed background Bash, with `run_in_background:true`,
`disable_timeout:true` and description exactly `Iris: wait for new attention`. The disabled client
timeout is necessary because Kimi's default background timeout is ten minutes, but the watcher itself
has an absolute 24-hour lifetime. Its deadline produces a non-zero exit without an attention line; the
resulting terminal notification may cost one turn before the watcher is re-armed.

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
new events. Checkpointing covers every event folded into the emitted line. Coalescing metadata is
independent of the cursor: an unreadable or malformed coalescing record means no coalescing delay,
never a suppressed wake or an inference that attention was told. A wake never marks attention told;
only a hook that emits its note does so.

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
`arm_requested` with its time and returns the client's blocking continuation. Its stderr is exactly:

```text
Start one background Bash task with command exactly `iris watch --client kimi --once`, description exactly `Iris: wait for new attention`, run_in_background=true and disable_timeout=true. Then stop. When its terminal notification arrives, fetch the Iris inbox and relevant claims and arm exactly one replacement before ending. Iris asks for this but cannot verify the command you chose.
```

Kimi 2.6.1 has no documented Stop task list or `stopHookActive` field for this purpose, so the local
marker prevents repeated continuations. A successful watcher publishes its live attempt before
waiting and clears the request. A later Stop inside a five-second publication grace period allows
without another request; after it, a request with no live attempt latches `failed` instead of asking
forever. SessionStart or an explicit user request can clear that latch.

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

### Opt-in now, default after acceptance, and retirement of ADR 026

Until the full native re-arm acceptance passes, wake stays opt-in and off by default because the
existing TUI route does not work. Fresh `iris integrate kimi` remains off; `--wake` installs only the
Stop request, the skill and the one-shot task route. No MCP web worker is started for any Kimi
session. `--no-wake` or removal disables the task route and stops only recorded watchers whose
process identity still matches. Existing children retain their startup arguments until restarted.

After native acceptance on a platform, fresh `iris integrate kimi` and `iris setup` turn wake on by
default there. An explicit `--no-wake` is always preserved, and ordinary setup and reintegration
preserve either explicit choice. Platforms without native acceptance remain off. This implements the
owner's direction of 2026-11-13 at about 20:20 UTC: wake should be on by default for every CLI whose
implementation is complete and verified live, while incomplete or failing implementations stay off.

ADR 026 is retired for every Kimi session, not only the TUI. There is no surface-discrimination,
route-reservation or mutual-exclusion machinery, and no web fallback when a task cannot be armed. If
a `kimi web`-owned surface later needs this mechanism, it requires its own ADR, positive binding and
native measurements rather than reviving the web worker here.

## Limits

- This is a request to the model, not enforcement. A model can ignore the Stop continuation, start
  the wrong command, fail to fetch mail or fail to re-arm. A failed latch prevents a continuation
  loop but leaves hook reminders as the only notification until a later successful arming.
- Nothing starts a closed Kimi session. On Kimi 2.6.1 `/clear` killed the managed task and delivered
  no notification to the new session, so a fresh Stop must arm a fresh watcher; no cursor or watcher
  identity transfers. The unexpected TUI exit left all 24 test processes alive, so native cleanup on
  an abnormal exit is not guaranteed; Iris's own parent-identity check must end an orphaned watcher.
- A busy-turn notification was retained and opened a separate turn only after the active turn ended.
  Empty or duplicate billable turns remain possible. The maximum task count and model cost are not
  established; 24 simultaneous tasks passed without a rejection before the unexplained TUI exit.
- The 24-hour bound can cause one terminal-notification turn per day without new mail. Transient hub
  outages spend no turn by themselves, but consume that lifetime.
- Output from a persistent `iris watch` cannot wake Kimi. An external process started outside managed
  background Bash has no managed terminal notification and is not a substitute.
- Only Linux with Kimi 2.6.1 was measured. The retired web route is not a fallback; a future
  web-owned use case needs new evidence and its own decision.
- Message bodies remain untrusted data. Neither the watcher line nor the native task notification
  carries them or chooses what the model does with them.

## Rejected

- **A persistent watcher whose output lines wake Kimi.** Output alone produced no turn in the
  controlled measurement.
- **Taking or disabling the TUI lock.** Both permit concurrent writers and interleaved history.
- **Keeping the ADR 026 web route beside the task route.** It cannot reach a TUI and adds unmeasured
  surface-discrimination and mutual-exclusion complexity. A future web-only use gets its own ADR.
- **Polling through `CronCreate`.** It can inject a scheduled prompt, but spends turns without new
  attention and is not event-driven delivery.
- **An externally spawned watcher.** It is not a Kimi managed task and cannot provide the measured
  completion notification.

## Verification

Implementation tests at the repository's required coverage must prove:

- exact arguments, fixed task description and exact Stop continuation text; 24-hour lifetime
  handling; exit 0 only after fixed-line output and checkpoint; no registration, override, fallback,
  adoption, rebind or lock bypass;
- initial backlog suppression, replay after a saved cursor, authoritative attention/told suppression,
  same-stream reset ordering, new-stream behavior and checkpoint failure replay;
- coalescing across sequential watchers, immediate eligibility when coalescing metadata is
  unreadable, conditional attempt ownership, duplicate rejection, stale pid/start rejection, `/clear`
  isolation, opt-out cleanup and fail-open Stop behavior;
- all ordinary watch invariants: relevant event kinds only, no content in output, no cursor
  regression and no missed replay between completion and re-arm.

The native acceptance runs on a fresh scratch session, never the owner's active session. **Items 1
through 4 block readiness of the implementation PR:**

1. Enable wake and end a turn. Confirm that Stop arms one managed task with the exact description,
   its rung-3 binding names the scratch session, no fallback Execution appears and a duplicate
   request leaves one watcher.
2. Send one message. Measure event-to-exit and exit-to-`turn.prompt` latency, capture the exact
   notification text, confirm `origin.kind:"task"` and `trigger:"notify"`, and require the woken
   model to fetch the message. Before that turn ends, prove that exactly one replacement is armed.
3. Repeat for a freed claim and for a second message sent during the completion-to-re-arm gap. The
   durable inbox plus replay must produce no loss and no duplicate native turn after told/fetch
   suppression.
4. With the implemented watcher, prove successful exit 0 and a non-zero failure notification, a
   notification arriving during a busy turn, and `/clear` killing the task without transferring a
   notification or identity to the new session. The measurements in this ADR establish the primitive
   for design review; they do not substitute for repeating these cases against the implementation.

Follow-up measurements cover TaskStop, the maximum task count, abnormal session close and the
24-hour deadline with a shortened test clock, plus model cost. The count established here is only a
lower bound of 24. macOS and Windows require their own live acceptance before wake becomes default
on those platforms. None of this follow-up weakens the blockers above or the full re-arm acceptance
required before default-on.

Run `make test`, `make race`, `make cov`, `make cov-check`, `make check-all` and `make fixtures`
before implementation review. Record real model turns separately from fixtures, including platform,
client version, latency and observed cost. No implementation is authorized by this draft before
review.
