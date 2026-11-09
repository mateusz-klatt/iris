# 016: Wake a Codex session with its own queue command

Status: proposed, 2026-11-09, by `claude-holzera-iris`, from measurements by
`codex-holzera-iris` in [the Codex attention record](../research/clients/codex-attention-2026-11-09.md).
Extends D7. Nothing here is implemented yet.

## Problem

D7 gives each client the strongest attention tier it has. Claude Code has an instant one: a
plugin monitor that wakes an idle session. For every other client D7 names sessions that Iris
itself started, through app-server or ACP control, which was never built, and the next-turn note
in hook context. That note reaches a model only on a prompt hook or on an allowed edit (gap
review A4). So a Codex session that a person started, idle or working without edits, hears
nothing about new mail until somebody types.

The fleet met this on 2026-11-09. Its native Codex sessions were woken by a timer per thread
that runs `codex queue` every ten minutes, with mail or without. A message waits up to ten
minutes, and every tick is a model turn on that thread, carrying its whole context. The cost of
a tick was not measured.

## What was measured

Codex CLI 0.167.0 on Linux, against a scripted local provider, with no hosted model. The record
linked above has the commands, the captures and the attempts that were confounded.

| Mechanism | Measured | Not measured |
|---|---|---|
| `additionalContext` from PreToolUse and PostToolUse on a shell tool | Reaches the next model request as developer context, in `exec` and in the TUI | How a real model treats the note |
| Hooks before and after a model request | Their output reaches no model input | |
| A blocking Stop hook | Continues the turn; its reason arrives as a user-role prompt; `stop_hook_active` does not stop a second or third continuation; Escape and Ctrl-C cancel a sleeping Stop | |
| A parked MCP call | A bounded wait inside a turn, ended by `tool_timeout_sec` or by the person; it does not wake a finished turn | |
| `codex queue --thread <id> --resume --message <text>` on an idle thread loaded in the default daemon, started in an ordinary TUI with normally approved hooks | A new turn on the same session id; UserPromptSubmit fires; the text arrives as a user message; the prompt and the reply show in the TUI | Withdrawing a queued message; an archived thread |
| The same command while a turn runs | Queued messages run after the turn, in order, as separate turns | |
| A stdio MCP child of the thread | One process across idle and queued turns, for as long as the thread stays loaded; stopping the daemon ends it | |
| The TUI closed, nothing else subscribed | The daemon unloads the thread 60 s later: the MCP child gets SIGTERM and SessionEnd fires | Other subscribers than a TUI |
| The daemon running, thread not loaded | `queue --resume` loads the saved thread with a new MCP child, fires SessionStart with source `resume`, and runs the turn | |
| The daemon stopped | `queue --resume` exits 0, stores the message, prints a warning and runs nothing. Starting the daemon alone runs nothing. The next resume of the thread runs the stored messages in order | |
| A second client on an app-server Iris manages | Starts a turn on an idle thread and steers a running one; `clientUserMessageId` does not deduplicate | Reaching a TUI that was not started with `--remote` |

## Decision

**1. An idle wake for Codex, by its own queue command, run by the session's `iris mcp`.** Once
bound, `iris mcp --client codex` follows its Execution's stream as a consumer of its own, not as
`iris-watch`. When attention is pending that the session's hooks have not told it (decision 3),
it runs the Codex binary, resolved once to an absolute path and started without a shell:

```
codex queue --thread <session id> --resume --message <fixed text>
```

The session id is the one the session's hooks registered, and it must parse as a UUID. This
process is chosen because it is the only Iris process that lives exactly as long as the loaded
thread, and it already holds the binding and the heartbeat. There is still no process per host
(D1).

The command must reach the daemon the session runs on. Codex gives an MCP server a filtered
environment (D14), so the Iris MCP table passes `CODEX_HOME` through `env_vars`, as it already
passes the hub variables. Whether that is enough for a session with its own Codex home is not
measured: the measured MCP child did not run the queue command itself.

**2. The wake is a trigger, not the notification.** The text is fixed: that there is new
activity in Iris, to use the inbox and claims tools, and that this is a notification from Iris,
not a message from the person. It never carries message content, a sender's name or a number
taken from mail, because it arrives on the user channel. What changed is said by the
UserPromptSubmit hook of the woken turn, in developer context, as the next-turn note is today.

The wake never records anything as told, and Codex is never reported as `monitored`. Only a
hook that put a note in front of the model moves the told state. So a wake that started nothing
leaves the mail waiting for the next real turn instead of hiding it.

**3. One wake outstanding per session, and none when a hook got there first.** After an
attention event the runner waits a short settle time and looks again: a session that is in a
turn and was told by a hook needs no wake. After it queues a wake it queues no other until the
told state has moved, the Execution has been replaced, or ten minutes have passed. It wakes at
most once for one attention cursor.

The runner cannot see turn boundaries. A marker written by the prompt and Stop hooks would let
it wait for the end of a turn, but whether Stop fires after an interrupt is not measured, so the
first version uses the settle time and accepts the cost below.

**4. Attention is identified by an event cursor, not by counts.** With the unread and available
counts, the hub returns the cursor of the newest attention event for the agent that is not yet
settled. Hooks and the wake runner compare cursors; the counts remain for the text of the note.
Today one message handled and another arriving between two samples leave the count unchanged,
and the second is never announced.

**5. The shell post-tool hook carries the note.** The reconcile already calls the hub. Its
response gains the attention block, and the hook adds the note to PostToolUse context for Claude
Code and Codex, the two clients measured to show it. A session in a long turn of shell commands
then hears at its next command, with no extra request. The cost is the tokens of the note.

**6. Opt-in at first.** `iris integrate codex` turns the wake on only when asked, and `iris
doctor` names the tier in use. It becomes the default after two measurements: a hosted model
acts on the wake and the cost of one wake is known for a short and a long thread; and macOS and
Windows run it. The reason for caution is
that a wake spends the person's model budget without a keystroke and starts work nobody typed.
Claude Code's monitor does the same and is on by default, so parity is the aim once this is
measured.

## What this cannot do

- The wake works while the session's TUI is open. A thread stays loaded only while something is
  subscribed to it, and sixty seconds after the TUI closes the daemon unloads it and ends its
  `iris mcp`. From then on nothing wakes it, and the same holds after the daemon stops and after
  a reboot. Its mail stays queued and is announced at the next session start or prompt. Waking a
  session that is not running needs a process per host and a decision to start work on a session
  somebody closed. Neither is decided here. A timer that an operator sets up outside Iris remains
  possible: against a running daemon, `codex queue --resume` reloads the thread.
- Exit 0 from `codex queue` shows the message was stored, not that a turn ran. Iris sees a turn
  only through its own hook.
- Every wake is a model turn, and a wake can find nothing to do: a queued wake cannot be
  withdrawn, so one queued during a turn may run after a hook already told the session.
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
- **An app-server Iris manages**, D7's Iris-owned session. It works, but it needs a launcher and
  a TUI started with `--remote`. The queue command reaches sessions people start themselves.
- **A timer.** Its latency and cost are the problem above.

## Verification

For the pull requests that implement this:

- With a recording stand-in for the Codex binary: one wake per attention cursor; none when a
  hook told the session first; none while one is outstanding; the argv is exactly the fixed one,
  with no shell; a session id that is not a UUID runs nothing; the told state never moves
  because of a wake; nothing runs unless the wake was turned on.
- The reconcile response carries attention, and the post-tool note appears once per cursor for
  Claude Code and Codex, on captured payloads only.
- Cursor identity: one message handled and one received, with the count unchanged, is announced.
- `iris doctor --live` for Codex with the wake on: mail sent to the scratch session is fetched by
  the woken turn. This spends a real model turn, so it stays opt-in as the rest of `--live`.
- A measurement, added to the research record, of `iris mcp` itself running the queue command
  from inside a session, with the default Codex home and with another one.
- Before the wake becomes a default, the research record gains the two measurements of
  decision 6.
