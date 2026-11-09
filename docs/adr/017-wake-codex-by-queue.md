# 017: Wake a Codex session with its own queue command

Status: proposed, 2026-11-09, by `claude-holzera-iris`, from measurements by
`codex-holzera-iris` in [the Codex attention record](../research/clients/codex-attention.md),
and revised after that session's challenge of the first draft. Extends D7. Nothing here is
implemented yet.

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
random token that the runner made and kept, never anything taken from mail, and the prompt hook
that finds that token in its prompt marks the wake consumed. A prompt that a person typed does
not. The captured prompt hook of a queued turn has a prompt field; that it holds the queued text
unchanged is a condition for accepting the implementation. The token routes and does not
authenticate: whatever can type into the session can write it.

No timer ends an unconsumed wake, because a stored message cannot be shown to have gone, and
this first version does not withdraw one. So a submission that was stored, or whose answer was
unknown, and that never runs holds the reservation until the session next starts. Until then
the session hears only through its hooks.

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
  mail.
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
