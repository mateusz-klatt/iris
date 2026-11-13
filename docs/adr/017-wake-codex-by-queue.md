# 017: Wake a Codex session with its own queue command

Status: accepted, 2026-11-09, by `claude-holzera-iris`. It rests on measurements by
`codex-holzera-iris` in [the Codex attention record](../research/clients/codex-attention.md);
that session challenged the drafts and signed off this design. Extends D7; the binding rung under
"Amendments from the implementation" at the end changes D6. That rung, the hub's attention
identity and the hooks' side are implemented. W3a implements the manual opt-in runner and W3b
adds integration controls and doctor diagnostics. [ADR 023](023-default-codex-wake-after-idle-check.md)
supersedes the default-off policy, adds a positive native idle check, changes preflight-failure
accounting and removes the automatic second native wake. The hook reminders and other binding,
attention and guard requirements here remain. Historical implementation and acceptance below
do not prove the later idle gate on every platform.

The [implementation acceptance record](../research/clients/codex-iris-idle-wake-2026-11-12.md)
tracks the later real-Iris measurements, their platform coverage and remaining limits.

## Problem

D7 gives each client the strongest attention tier it has. Claude Code has an instant one: a
plugin monitor that wakes an idle session. For every other client D7 names sessions that Iris
itself started, through app-server or ACP control, which was never built, and the next-turn note
in hook context. That note reaches a model only on a prompt hook or on an allowed edit (gap
review A4). So a Codex session that a person started, idle or working without edits, hears
nothing about new mail until somebody types.

The fleet met this on 2026-11-09. Its native Codex sessions were woken by a timer per thread
that runs `codex queue` every ten minutes, with mail or without. A message waits up to ten
minutes, and each tick that runs is a model turn on that thread. What such a turn costs on a
hosted model was not measured.

## What was measured

Codex CLI 0.167.0 on Linux, against a scripted local provider, with no hosted model. The record
linked above has the commands, the captures and the attempts that were confounded.

| Mechanism | Measured | Not measured |
|---|---|---|
| `additionalContext` from PreToolUse and PostToolUse on a shell tool | In `exec`, both reach the next model request as developer context. In the TUI, PostToolUse was confirmed separately | PreToolUse in the TUI; how a real model treats the note |
| Hooks before and after a model request | Their output reaches no model input | |
| A blocking Stop hook | Continues the turn; its reason arrives as a user-role prompt; `stop_hook_active` does not stop a second or third continuation; Escape and Ctrl-C cancel a sleeping Stop | |
| A parked MCP call | A bounded wait inside a turn, ended by `tool_timeout_sec` or by the person; it does not wake a finished turn | |
| `codex queue --thread <id> --resume --message <text>` on an idle thread loaded in the default daemon, started in an ordinary TUI with normally approved hooks | A new turn on the same session id; UserPromptSubmit fires; the text arrives as a user message; the prompt and the reply show in the TUI | Withdrawing a queued message: the experimental schema declares a delete call, which was not exercised |
| The same command while a turn runs, on a thread without a TUI | Queued messages run after the turn, in order, as separate turns | Ordering across a restart |
| A stdio MCP child of the thread | One process across idle and queued turns while the thread stays loaded. Its start carries no session identity; each tool call carries the thread and session id. Stopping the daemon ends it | |
| The TUI closed, no turn afterwards | The daemon unloaded the thread 60 s later: the MCP child got SIGTERM and SessionEnd fired | Whether a queued turn after the close starts a new 60 s |
| The daemon running, thread not loaded | `queue --resume` loads the saved thread with a new MCP child, fires SessionStart with source `resume`, and runs the turn | |
| The daemon stopped | `queue --resume` exits 0, stores the message, prints a warning and runs nothing. Starting the daemon alone runs nothing. The next resume of the thread runs the stored messages in order | A crash while a message is queued |
| A second client on an app-server started for the experiment | Starts a turn on an idle thread and steers a running one; `clientUserMessageId` does not deduplicate | Reaching a TUI that was not started with `--remote` |

## Decision

This is the original decision. Its opt-in default and native second-wake directions are
superseded by [ADR 023](023-default-codex-wake-after-idle-check.md); the dated W3 amendments
below describe the implementation before that change.

**1. An idle wake for Codex, by its own queue command, run by the session's `iris mcp`.** Once
bound, `iris mcp --client codex` follows its Execution's stream as a consumer of its own, not as
`iris-watch`. When attention is pending that the session has not been told (decisions 3 to 5),
it runs the Codex binary, resolved once to an absolute path and started without a shell:

```
codex queue --thread <session id> --resume --message <fixed text>
```

A thread's MCP child was measured to live as long as the thread stays loaded, and `iris mcp` is
such a child that already holds the binding and the heartbeat. The thread to wake is the session
of its established binding (D6), never a guess from the process, because an MCP child learns no
identity when it starts. The session id must parse as a UUID. There is still no process per host
(D1).

The command must reach the daemon the session runs on. Codex gives an MCP server a filtered
environment (D14), so the Iris MCP table passes `CODEX_HOME` through `env_vars`, as it already
passes the hub variables. Whether that is enough is not measured: the measured MCP child did not
run the queue command itself. It is a condition for accepting the implementation (decision 7).

**2. The wake is a trigger, not the notification.** The text is fixed: that there is new
activity in Iris, to use the inbox and claims tools, and that this is a notification from Iris,
not a message from the person. It never carries message content, a sender's name or a number
taken from mail, because it arrives on the user channel. What changed is said by the
UserPromptSubmit hook of the woken turn, in developer context, as the next-turn note is today.
That a queued turn fires that hook was measured. That the note Iris emits there reaches the
model on such a turn was not.

The wake never records anything as told, and Codex is never reported as `monitored`. Only a
hook that emitted a note moves the told state. This removes one failure: taking the queue's
acceptance of a message for a delivered notification. It does not prove delivery. A hook records
its note as told when it emits it, and Iris cannot see whether the client then put it in front
of the model, so a cancelled turn or lost hook output can still leave mail unannounced. Decision
5 gives that case one more attempt.

**3. One wake for one attention event, and one unconsumed wake for a session.** The runner
keeps what it last woke for, and what the queue command answered, in the session's memory file
beside its generation, so a restarted `iris mcp` knows it too. It keeps four answers apart:

- the command exits 0 and names a queued message: submitted;
- it exits 0 with the warning that the thread could not be resumed: stored, and nothing runs
  until the thread is next resumed;
- it fails before it submits: nothing was stored;
- anything else: unknown, and treated as submitted.

A wake is submitted at most twice for one attention identity: when it becomes pending, and
once more under decision 5. A failure before submission allows one retry of either. That
allowance is part of the kept state.

While a submitted wake is unconsumed the runner submits no other, whatever arrives, the second
wake of decision 5 included. A wake is consumed only by its own turn. The fixed text ends with a
random token, new for each attempt and never taken from mail, and the prompt hook whose whole
prompt is that fixed text with the token of the kept attempt marks the wake consumed. A prompt
that a person typed does not, and neither does one that only contains the token. The
runner writes the attempt and its token to the kept state before it starts the command, because
the woken turn's prompt hook can run before the command returns; the command's answer, arriving
later, never turns a consumed wake back. The captured prompt hook of a queued turn has a prompt
field; that it holds the queued text unchanged is a condition for accepting the implementation.
The token routes and does not authenticate: whatever can type into the session can write it,
and a match shows only that this prompt hook ran.

No timer ends an unconsumed wake, because a stored message cannot be shown to have gone, and
this first version does not withdraw one. A session start does not end it either: a resumed
thread fires SessionStart before it runs its stored messages, so a start does not show the
stored wake gone, and the kept attempt and its token survive the start. Only the wake's own
prompt hook releases the reservation. This trades liveness for not piling up wakes: a submission
whose answer was unknown and that was never stored blocks later wakes for that session, and
until it is cleared the session hears only through its hooks. `iris doctor` reports a wake that
has stayed unconsumed and says how to clear it by hand.

Before it submits, the runner waits a short settle time and looks again: a session that is in a
turn and was told by a hook needs no wake. The runner cannot see turn boundaries. A marker
written by the prompt and Stop hooks would let it wait for the end of a turn, but whether Stop
fires after an interrupt is not measured, so the first version uses the settle time and accepts
the cost named below.

**4. Attention is identified by stream and cursor, not by counts.** With the unread and
available counts, the hub returns the identity of the newest attention event that is still
pending for the agent: the hub's stream id and that event's cursor, or nothing. Only two kinds
of event count: mail that still needs attention, and a path the session waited for becoming
free. Heartbeats, registrations and the general `changed` event do not, so none of them can
cause a wake. A different stream id, as after a restore, discards the told state, and what is
pending is announced once more. Hooks and the runner compare identities; the counts are only
the text of the note. Today one message handled and another arriving between two samples leave
the count unchanged, and the second is never announced.

**5. An announcement that brought no fetch is made once more.** If the same attention identity
is still pending ten minutes after a hook emitted its note, it counts as untold once: the next
prompt hook says it again, and the runner may submit the second wake that decision 3 allows.
After that Iris stays quiet about it until something new arrives. This is best effort. It bounds
how often Iris announces one event, and what that costs, not when the model receives the note or
whether it does: the second note can be lost as the first was. A model that chooses not to read
its mail is asked twice, not in a loop.

**6. The shell post-tool hook carries the note.** The reconcile already calls the hub, but its
response names only the Execution today. It gains the attention block, and the hook adds the
note to PostToolUse context for Claude Code and Codex, the two clients measured to show it. A
session in a long turn of shell commands then hears at its next command, with no extra request.
The cost is the tokens of the note.

**7. Opt-in at first.** `iris integrate codex` turns the wake on only when asked, and `iris
doctor` names the tier in use. It becomes the default after three measurements: a hosted model
acts on the wake, and the cost of one wake is known for a short and a long thread; macOS and
Windows run it; and `iris mcp` itself runs the queue command from inside a session, with the
default Codex home and with another. The reason for caution is that a wake spends the person's
model budget without a keystroke and starts work nobody typed. Claude Code's monitor does the
same and is on by default, so parity is the aim once this is measured.

## What this cannot do

- The wake needs the thread to be loaded. Sixty seconds after the TUI closed, with no turn in
  between, the daemon unloaded the thread and ended its MCP child; whether a queued turn extends
  that is not measured. After the unload, after the daemon stops and after a reboot, there is no
  `iris mcp` to hear an event or to run the command. The mail stays queued and is announced at
  the next session start or prompt. Waking a session that is not running needs a mechanism
  outside the session, and a decision to start work on a session somebody closed. Neither is
  decided here. A timer that an operator sets up outside Iris remains possible: against a running
  daemon, `codex queue --resume` reloads the thread.
- Exit 0 from `codex queue` shows the message was stored, not that a turn ran. Iris sees a turn
  only through its own hook, and it sees that the hook emitted a note, not that the model
  received it.
- Every wake that runs is a model turn, and it can find nothing to do: a wake queued during a
  turn may run after a hook already told the session. The cost of a turn is not measured.
- The fixed text bounds what Iris says on the user channel. It does not bound what the model
  reads next through the inbox tool. Message bodies stay untrusted data, as before.
- The evidence is one client version on Linux with a scripted provider.
- Other clients are unchanged. Each needs its own measured command.

## Rejected

- **Stop continuation as the turn-boundary tier.** It works, but its reason is a user-role
  prompt, the client does not bound repeats, and the queue already runs after the current turn
  in order. It stays a measured option for a client that has no queue command.
- **A parked MCP wait.** It holds a turn open, the model has to choose to wait, and a timeout
  costs a request.
- **An app-server that Iris would manage**, D7's Iris-owned session. The experiment's own
  app-server shows the control works, but it needs a launcher and a TUI started with `--remote`.
  The queue command reaches sessions people start themselves.
- **A timer.** Its latency is the problem above, and it runs a turn when there is nothing to
  say.

## Verification

For the pull requests that implement this:

- With a recording stand-in for the Codex binary: one submission when an attention identity
  becomes pending; none when a hook told the session first; none while one is unconsumed, also
  across a restart of `iris mcp`; each of the four answers of decision 3 handled as written; the
  argv exactly the fixed one, with no shell; a session id that is not a UUID runs nothing; the
  told state never moves because of a wake; nothing runs unless the wake was turned on.
- Consumption: a prompt that a person typed before the queued wake ran does not release the
  reservation; the prompt that carries the wake's token does; the token is never derived from
  mail, and a prompt that only contains it inside other text does not release the reservation.
  The attempt is on disk before the command starts: a prompt hook that runs before the command
  returns consumes it, and the late answer does not undo that. A resumed session whose stored
  wake runs after SessionStart keeps the reservation through the start, and the wake's own
  prompt hook releases it.
- Attention identity: one message handled and one received, with the count unchanged, is
  announced; a changed stream id announces what is pending once; a heartbeat, a registration and
  a `changed` event cause no wake.
- The second announcement of decision 5 happens once, after ten minutes, and not again; its wake
  is submitted only when none is unconsumed, and the allowance survives a restart.
- The reconcile response carries attention, and the post-tool note appears once per identity for
  Claude Code and Codex, on captured payloads only.
- `iris doctor --live` for Codex with the wake on: mail sent to the scratch session is fetched by
  the woken turn. This spends a real model turn, so it stays opt-in as the rest of `--live`.
- Before the wake becomes a default, the research record gains the three measurements of
  decision 7.

## Amendments from the implementation

Each change below was made by the pull request that implemented it.

**A binding rung from the tool call** (changes D6; gives decision 1 the binding it relies on).
`claude-holzera-iris` accepted it on 2026-11-10.
- Decision 1 wakes a session once its `iris mcp` is bound. On the daemon a thread's MCP child has
  the daemon for a parent, D6 skips the nearest-process rung for app-server, and Codex gives an MCP
  child no session id in its environment
  ([the MCP child record](../research/clients/codex-mcp-queue-2026-11-10.md)). So the shim of a
  session the daemon hosted was bound only by `IRIS_EXECUTION` or by a handle the model passed,
  and a handle copied from elsewhere rebound it.
- In the runs measured, on Codex 0.167.0, every tool call named its session in `_meta.sessionId`
  and its thread in `_meta.threadId`. The tool call's session id matched the session id of the
  hooks of the same turn in nine runs: five threads the daemon loaded, `codex exec resume`,
  `codex exec fork`, a thread loaded on a restarted daemon, and a sub-agent, whose call named the
  root's session and its own thread. The attention record describes the ids as matching for a new
  TUI thread. Not looked at: a thread resumed or forked in the TUI, a thread that stays loaded
  while its daemon restarts, a sub-agent on the daemon, other app-server clients, other Codex
  versions, macOS and Windows. No message the MCP child received before the first tool call
  carried the id.
- A live `IRIS_EXECUTION` binds every call, as before, whatever session the call names. Otherwise
  the first call to reach binding decides the process's mode for its life. If that call names no
  session, the process keeps D6's chain, which uses no call's id. If it names one, the process is
  in id mode: it binds only by the session each call names, as rung 2, never through the client
  process or a handle, and it registers, adopts and hands over nothing. In id mode the `execution`
  argument is ignored.
- If that first call's thread id is its session id, the process is a root thread's MCP child. It
  keeps and renews its session's binding. Its first bind waits up to the 15 s grace period for the
  session's hook to register the session; the binding is then refreshed by its Execution and
  renewed by the heartbeat, as before. After a loss the session is looked up again at once, and an
  Execution found lost is never bound again. A different or missing thread id makes Iris treat
  the process as a sub-agent's MCP child, which looks each call up and renews nothing. A shim's
  heartbeat puts the Execution on a 60-second lease, and a sub-agent's MCP child, whose lifetime
  was not measured, could then let the root's Execution lapse while the root is idle. A call that
  names a session other than its process's is also attributed by a lookup, and that binding is
  never renewed. No lookup waits behind a first bind's grace period.
- A Codex sub-agent is its root session to Iris: it shares the root's mailbox, claims and
  generation. Iris installs no SubagentStart or SubagentStop hook for Codex, and the decoder reads
  `subagentId` and `subagent_id`, not the `agent_id` Codex sends, so the captured payloads decode
  to the root session. Claude Code's sub-agents send the same `agent_id` and decode the same way. A
  SubagentStart context there reaches only the sub-agent's model, so a hand-installed SubagentStart
  hook never counts it as told for the root session.
- A call whose session has no single usable Execution gets a typed `unbound` error: "No single
  usable Iris Execution matches the session Codex names on this call, on this host. Its hooks may
  not have run, or the hub may have restarted and the session not reattached yet. Run `iris
  doctor`, which says whether the hooks need review in /hooks when the hub can tell, then send any
  prompt and retry. A call that names its session is never bound by an execution handle." It names
  no cause, because the hub answers alike for a session never registered and for one recovering,
  expired or ended.
- The call never falls through to a handle. A handle reaches the shim through the model, which can
  copy it from another session's context or from a message body. In every path compared, the
  call's session id matched the hooks'. A session whose hooks never ran has no handle of its own.
  The cost: if a Codex version, or a path not looked at, gave a session's calls a session id its
  hooks did not register, as Claude Code does on `--continue`, that session would have no Iris
  tools, where today it can pass the handle from its own SessionStart context. And a `codex exec`
  session whose hooks never ran no longer gets an MCP-only Execution. `IRIS_EXECUTION` stays the
  explicit override.
- For a session the daemon hosts, the root's shim now binds at the first Iris tool call, and its
  heartbeat moves the Execution to the 60-second lease a bound Claude session has. SessionEnd still
  ends it. If that never comes, it lapses about a minute after its last heartbeat or hook, instead
  of up to 15 minutes after its last hook. Lookups leave the lease mode as they find it.
- `iris doctor` counts two observations, each written once per process: calls of two sessions,
  and calls of a session without a usable Execution, when a root's grace period ran out or a
  lookup found none. For the second, its hint follows the hub's word on when a Codex hook from this
  host last reached it. If none did in the last 24 hours, doctor advises the review in `/hooks`.
  If one did, doctor says the hooks run here. Either the hub restarted and that session had not
  reattached, or this Codex gives its tool calls a different session id from the one it gives its
  hooks; in that case this version of Iris cannot bind the session the calls name. Doctor also says
  that a session whose own hooks did not run while other sessions' hooks did looks the same.
  Without the hub's word, doctor says it cannot tell. That evidence is per host and client, not per
  session.
- Left for the wake: it may arm only in a root's process, or a session would be woken once per
  sub-agent, and a call's thread id never authorizes waking a sub-agent. It still checks UUID syntax
  before it builds a command (decision 1), and it takes the hub's hook evidence, not this binding,
  as proof that a hook registered the Execution.
- What this cannot do: bind before the model's first Iris tool call, so until then there is no
  wake; tell why a session has no usable Execution; verify the id, which is the client's word as
  a session variable is for other clients (D12); tell whether the native thread is alive, since the
  hub knows only the Iris lease and when a hook last ran; give a sub-agent an identity of its own;
  or check an override against the session a call names. A process in legacy mode keeps D6's
  limits.
- The tests replay the captured calls and hooks through the decoder and the MCP server, and one
  runs against the real hub and store, across a restart.

**The attention identity lives on the delivery** (the hub's side of decision 4; amends it).
`claude-holzera-iris` accepted it on 2026-11-10 with the plan for the wake.
- Decision 4 names the identity by the hub's stream and the cursor of the newest pending attention
  event. Retention deletes events after seven days, or after an hour once every live cursor has
  passed them, and never deletes mail, so mail nobody fetched would lose its identity.
- So a delivery keeps `attention_seq`, the cursor of the newest `inbox_changed` event for it. A
  trigger that migration 023 creates sets it as the event is inserted, for every producer: a send,
  the console's send, a missed receipt's recovery, a requeue and a handover. The trigger is part
  of the database, so a hub from before migration 023 that runs on it after a rollback sets the
  cursor too. The rollback that `docs/deploy.md` documents restores the backup taken before the
  deploy instead, and migration 023 then runs again on the way forward. The identity's cursor is
  the largest of the `attention_seq` of the agent's mail that still needs attention and is not
  acknowledged, and of the cursors of the surviving `claim_available` events addressed to the
  Execution.
- Migration 023 fills `attention_seq` for mail that still needs attention. It uses the cursor of
  each delivery's newest surviving event or, if retention removed that event, the event
  sequence's high-water mark, so such mail reads as new once. It gathers the newest events in one
  pass over them and does not read them again for each delivery; a query-plan test holds it to
  that.
- Every attention block carries `identity`, `null` when nothing is pending; a hub from before sends
  no field, and a client tells the two apart by its presence. Session, touch and both shell
  reconcile answers carry the block, and so does an events read that asks with `attention=1`, as
  the wake's runner will. The watch socket's own reads never ask, so they pay nothing for it. An
  events read may name its `generation`; without one it is unqualified, as before. A bind answer
  says `hook_seen`, whether any hook has reached the hub for the Execution.
- Cost: the identity comes from the two queries that already gave the counts, each now with a
  maximum beside its count. The freed paths' query names the Execution's agent, which every
  `claim_available` event carries, so it uses the index on `(agent_id, seq)` instead of reading
  every event. A query-plan test holds both to their indexes. The trigger adds one update by
  primary key to each `inbox_changed` event, and that update runs the console's revision trigger
  on `Delivery` a second time in the same transaction.

**Claim availability stays historical** (decision 4). A `claim_available` event stays in the
identity until retention removes it, even after the session or somebody else has claimed the
path. The `available` count already includes such events until retention removes them. It is a
bounded reminder. Settling availability would be a further change to decision 4, and none is
made.

**A restore keeps the stream** (decision 4; amends it). Decision 4 says a different stream id, as
after a restore, discards the told state. A backup copies the whole database, `Meta` included, so
a restored hub keeps its stream id and takes the event sequence back instead (ADR 006).
- So every attention block also carries `high_water`, the event sequence's high-water mark. The
  hooks and the runner, once built, compare each cursor they remember with it: the hooks' told
  cursor, and the cursor of what the runner last woke for. A remembered cursor above it means
  that the hub went back, and they treat it as they would a different stream. The hooks discard
  the told state, so what is pending is announced once more. The runner starts the wakes that
  decision 3 allows for an identity afresh, and a wake still unconsumed goes on blocking others,
  as decision 3 has it. This is the rule that ADR 006's decision 7 gives a monitor whose cursor is
  ahead of the hub.
- What this cannot do: a client that first checks after `high_water` has reached its remembered
  cursor cannot tell that the hub went back. The hooks and the runner will treat only a larger
  cursor in the same stream as new, so attention committed after the restore whose cursor is at
  or below a remembered one will go unannounced and wake no one until newer attention arrives,
  though the counts include it and the inbox returns it. A monitor has the same limit. A backup
  could give its copy a new stream id, which would close the gap for restores from
  `iris admin backup`; that changes ADR 006's restore, and this amendment does not make it.
- ADR 006's amendment of 2026-11-13 makes that change. A database that carries the restore marker
  `iris admin backup` writes gets a new stream at the first start of a binary with migration 025 on
  it, so the gap remains only for a database without the marker.

**The hooks follow the identity** (decisions 3 to 6; amends them).
- The told state. A session's attention file, beside its generation, keeps the counts as before
  and, from a hub that names attention, the identity last told, when, whether it was said a second
  time, and a revision that every change of that identity raises. The stream is written as hex, so
  nothing a hub sends can split the line. With a file from before ADR 017, or with no identity
  told yet, the counts decide once, as before, and the identity then counts as told.
- A note is said when the hub's identity has another stream or a larger cursor than the told one.
  Within a stream the told cursor goes down only after a restore the hub shows, so a slower
  hook's older answer cannot reopen what was said. The restore rule above voids the told state
  only when no other hook changed the told identity between this hook reading the told state,
  before it asks the hub, and its decision: an older answer that arrives late is below a newer
  telling too, and must not be taken for a restore. A touch answer is never taken as one either,
  because the hub may replay an earlier call's answer to a retried call, attention and all.
- The second note of decision 5 is said only by a prompt hook, when the same identity is still the
  newest pending ten minutes after it was told. A note says everything pending, as counts, because
  the identity, not the counts, said that something is new.
- The SessionStart context counts as told, and a context that restates the identity already told,
  as a resumed session's does, counts as its second telling. Where a monitor beats, it says the
  note, and the told state stays as it was. A hub from before, which sends no `identity`, and a
  reconcile answer without `attention` behave as before. The told state goes when the session
  ends; a stale generation that the hook drops no longer takes it along.
- The shell note of decision 6. A PostToolUse shell hook whose reconcile answer carries attention
  adds the note to its context, for Claude Code and Codex, whose post-tool context reaches the
  model. Before a command it says nothing.
- The lock. Every read and every change of a told state or a wake file, and every removal by
  pruning, holds an exclusive lock on `sessions/.lock`, `flock` on Unix and `LockFileEx` on
  Windows, tried for at most 50 ms. Reads hold it too, because on Windows the rename used here
  fails when another process holds the destination file open. An operation that cannot
  take it leaves its state unchanged, and a told state that is not updated says no note; a
  revision not read makes that answer no evidence of a restore. The hook records the failure at
  the stage `session files` and goes on (D3); doctor counts such entries beside the row's status,
  not as a failed hook, since the hook's allow or deny is unchanged. A write protects the
  sessions directory before the lock is taken, never under it: on Windows that costs about 80 ms
  for a thousand sessions' files, because Windows applies the directory's inheritable protection
  to each, while another hook waits at most 50 ms. Pruning lists and judges the directory before
  it takes the lock, and judges each stale entry again under it, so the lock is held only for
  rechecks and removals. One pruning removes at most 64 entries: removing two thousand held the
  lock for 305 ms on Windows and 9 ms on Linux; the rest go at later session starts. If the caller
  has stopped waiting, as a shell reconcile past its budget has, when the attention check runs
  under the lock, no note is decided and the told state is not written. The lock binds only
  Iris's own processes; one that ignores it is not stopped. An Iris from before this change on the
  same host, as after a rollback, takes no lock and drops the told identity when it writes the
  counts. Its pruning removes a file untouched for 30 days, so every lock refreshes `.lock`'s
  time, and that pruning does not remove it while hooks run.
- Consumption of decision 3. A Codex prompt hook first compares its prompt with the newest attempt
  in the session's wake file, before repository discovery and before any network call. Only a
  prompt that is that attempt's whole message, byte for byte, marks it consumed, so the planned
  runner must store the message it submits, the fixed text with the attempt's token. A session
  without a wake file takes no lock. Any other prompt takes the lock for one read of the wake
  file; only a match protects the directory, outside the lock, and takes it again to read and
  write. Only Codex's prompt is read: Kimi sends its prompt as an array. A capture of
  a queued turn's prompt hook, from the run in
  [the queue record](../research/clients/codex-mcp-queue-2026-11-10.md), shows that the field
  holds the queued text unchanged.
- The wake file is JSON with `version` 1 and `attempts`, at `sessions/wake/<name>`; the planned
  runner writes it. A hook sets only an attempt's `consumed_ms` and keeps every other field as it
  finds it. When it writes the file, the token, message and answer of attempts consumed more than
  30 days ago go from it, and nothing else does. The wake files sit apart from the files that
  pruning reads: no hook lists their directory, and nothing removes one, because a file's spent
  slots must outlast mail nobody fetches. The planned runner must make that directory with
  protection of its own: on Windows a told-state write beside 20,000 wake files took 2.2 s when
  the directory inherited the sessions directory's protection, and 1.9 ms when it had its own.
- What this cannot do: show that a note reached the model (decision 2); say the second note from
  an edit or shell hook; or keep the wake files few. Taking a wake file to be about 1 KB with its
  attempts, a host where 50 sessions a day arm the runner keeps about 18,000 files and 18 MB a
  year, and nothing removes them. The other session files still go 30 days after their last
  change.

### W3a: manual opt-in runner, 2026-11-12

`iris mcp --client codex --wake` runs the listener inside that MCP child. Omitting `--wake`
starts no listener, wake-state operation or runner subprocess. Other clients cannot enable it.
The integration passes `CODEX_HOME` through to keep the queue command on the same Codex home.
W3a does not yet add the integration flags or an idle-wake live doctor probe; the existing live
doctor proves its documented hook and tool actions, not this event-to-idle-turn chain.

The target must be a positive root id-mode binding with a UUID session id and no live execution
override. A client/host-qualified bind read must confirm hook evidence for that exact execution
and generation. A one-second binding check cancels the old worker on replacement or loss.
Admission's final target comparison and `cmd.Start` share the resolver mutex; waiting for the
command holds neither that mutex nor the sessions file lock.

The listener uses a generation-qualified anonymous stream, without `consumer_id`, so it saves
no retention cursor and never makes Codex `monitored`. Connection, reconnection, reset,
`inbox_changed` and `claim_available` trigger an authoritative events read. Other frames only
advance the reconnect cursor. A buffered hint lets the socket reader continue while a command
waits. A candidate settles for ten seconds; later hints do not postpone an earlier candidate.
An older identity's second-note deadline never delays settlement for new attention. Operational
retry floors and the persisted known-failure allowance still apply.
Failed connections back off from one to thirty seconds. Hub admission failures retry after
thirty seconds, lock-budget or snapshot changes after ten, and other state I/O failures after
sixty. Outstanding or unusable state is polled from files every fifteen seconds, with no
attention reads while it blocks. A consumed reservation first settles before another read.

The runner shares `sessions/.lock` with hooks, with its own one-second lock budget. It compares
the told and wake files' presence and whole bytes before and after the hub read and again before
publishing. The wake document has mandatory version, revision and attempts. Each attempt has a
unique 128-bit hexadecimal id and immutable kind: `binary_check` or `queue`. Tokenless binary
failures count toward the same one-retry allowance but cannot be outstanding or consumed. An
unconsumed queue attempt retains its token and exact prompt. Unknown answers remain blocking.
Two known failures exhaust a slot; a single one retries no earlier than sixty seconds after
its answer. Unconsumed aborted attempts cost no slot. Replacement publication removes superseded
unconsumed aborts, retaining the mark and quota-bearing attempts, so the current identity has
at most five records. Unusable or older prototype documents are not silently reset.

Preparation protects and syncs a private temporary file outside the shared lock. Installation
compares snapshots, renames, reads back and closes under the lock. Unix directory sync follows
unlock and precedes command start. Directories must be exactly `0700` on Unix; Windows checks
the owner and actual owner-only protected inheritable DACL, correcting or refusing permissive
entries. An already correct directory avoids recursive ACL rewriting. Command outcomes merge
by immutable attempt id, preserving consumption and newer attempts, with at most three writes
one hundred milliseconds apart. A certainly unstarted reservation is aborted by that exact id;
if its abort cannot be saved, an unanswered queue stays blocking and a binary failure stays
counted. An actual failed `cmd.Start` counts as a known failure rather than a free abort.

The binary is resolved once to an absolute path. Before every submission, `--version` must
match the MCP initialize version exactly, within five seconds, one-second WaitDelay and four
KiB combined output. Queue execution has thirty seconds, one-second WaitDelay and separate
sixty-four KiB output caps. Only complete measured success, stored-warning and same-thread
no-rollout rejection forms receive those outcomes. Other output, signals, timeout or overflow
is unknown. Raw output is never saved as diagnostic text or written to MCP stdout.

Fixed observations in `hook-observed.log`, event `mcp`, are `wake_root_eligible`,
`wake_stream_following`, `wake_invalid_state`, `wake_hooks_unseen`,
`wake_deferred: session active`, `wake_aborted: session active`, the `wake_failed` reasons
below, `wake_unknown` and `wake_worker_panic`. `wake_hooks_unseen` means the hub has no hook
report from the bound execution and generation, so the session is not yet eligible; nothing ran
and nothing failed.

`wake_deferred: session active` means the idle check reported the session active: for Codex an
active thread, for Kimi a session that is busy, in a turn, archived or waiting on an interaction
(ADR 026). `wake_aborted: session active` means Kimi answered a reserved prompt with `40901`,
SESSION_BUSY. Neither spends quota or is a failure; both check again after fifteen seconds. A
session can stay active for many such checks, and occupied (`wake_failed: session occupied`
below) for as long as another Kimi process keeps it open, so the engine notes each of these three
only when a step's outcome changes to it: a session active for an hour leaves one line, not 240.
Any other outcome clears that memory, so the next such period is noted again, and so does a new
binding, whose engine starts without it.

Each `wake_failed` observation names one condition, never error, command or client text, because
doctor counts observations by their exact text:

- `wake_failed: hook check`: the hub could not be asked whether the session's hooks were seen.
- `wake_failed: attention read`: the authoritative attention read failed.
- `wake_failed: version differs`: the check found a version other than the client's. For Codex
  the executable names another version than the running session, as after a CLI update under
  it; restarting that session clears it. For Kimi the running `kimi web` server names another
  version than the installed `kimi` (ADR 026); restarting `kimi web` clears it.
- `wake_failed: binary check`: any other binary check failure, such as a missing executable,
  an unreadable version or, for Kimi, an unavailable web server.
- `wake_failed: idle check`: the session's idle state could not be verified.
- `wake_failed: session occupied`: Kimi answered `40931`, SESSION_OCCUPIED, on the status read or
  the prompt: another Kimi process, such as the TUI, holds the session's lock, and kimi web cannot
  prompt the session until that process exits (ADR 026). It spends no quota and records its reason
  as the last failure.
- `wake_failed: queue failed`: the queue command gave a known failure.
- `wake_failed: state file`: the wake state file could not be read, locked or published.

The hook and attention checks retry after thirty seconds; the binary, version and idle checks, an
occupied session and a state file failure other than a busy lock or a changed snapshot retry after
sixty seconds.

Root eligibility and an accepted socket are separate facts; neither proves
a queued turn, prompt consumption or a model's inbox fetch. A recovered worker panic leaves
MCP tools and heartbeat working. Native acceptance must still join the real incoming delivery,
already idle same-UUID turn, exact prompt, consumed mark and inbox result, with both default
and explicit Codex homes. The [implementation plan](../research/clients/codex-wake-runner-plan-2026-11-12.md)
records the platform and cost evidence still required.

The ledger protects against cooperating process or shim restarts. It does not prove durability
across whole-host power loss: first-parent publication, Windows directory sync and answer or
consumption sync leave gaps. Losing a reservation in that case can allow another wake.
An outstanding guard never expires automatically. Resume a live session to consume its queued
prompt. Remove its exact wake file only after permanently closing the session and stopping its
MCP child; removing a live guard can admit duplicates.

### W3b: integration controls and local diagnostics, 2026-11-12

`iris integrate codex --wake` enables the runner and `--no-wake` disables it for future MCP
children. Without either, installation, setup and reintegration preserve the current choice;
a fresh integration remains off. The two flags are mutually exclusive and cannot be combined
with removal, all clients or a different client. Existing children retain their startup
arguments. A complete TOML parse determines the existing preference before hooks are changed;
unrelated user configuration is preserved and malformed configuration is refused.

Doctor reports the configured attention tier and local retained wake metadata. A configured
runner is explicitly not proof of current arming or delivery. Guard inspection lists outside
the shared lock and reads each file in its own hold, without changing guard bytes or directory
protection. The stable lock file may be created or normalized by the shared lock mechanism.
Unknown or unreadable state remains blocked. The report names attempt bounds, consumed or
outstanding state and age, checked binary/version metadata and fixed known failure reasons;
it never prints prompts, tokens, arbitrary stored details or raw process output. It gives the
same recovery rule as W3a: resume a live session, and clear a guard only after permanent close
and MCP shutdown. These local diagnostic lines do not change the hub report schema or make
the existing exec-based live probe an idle-wake test.

See [Codex idle wake](../codex-wake.md) for the controls and their limits. Native same-thread
delivery, hosted-model behavior and short/long-thread cost evidence remain separate measurements;
configuration and command acceptance do not substitute for them.
