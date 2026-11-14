# Kimi wake

Kimi starts a model turn in an idle session when a background task it manages ends. Iris uses that:
it asks the session's model to keep one task running, `iris watch --client kimi --once`, which ends
when Iris has new attention for the session. The turn that its end opens reads the inbox and claims
and starts the next task. Each wake is a model turn and can spend model budget.
[ADR 030](adr/030-kimi-one-shot-wake.md) has the decision and the measurements behind it.

**Wake is off by default** until its native acceptance passes. Only Linux with Kimi 2.6.1 has been
measured, and macOS and Windows need their own acceptance before wake turns on by default there.

## Turning it on and off

```sh
iris integrate kimi --wake
iris integrate kimi --no-wake
```

The choice is recorded in `kimi-wake/preference` in the OS account's default Iris configuration
directory. The file holds `default` when no choice was made, which is off for now, or `on` or `off`.
Setup and reintegration keep whatever it holds, so an explicit choice survives them.
`iris integrate --remove kimi` removes it with the rest of the integration.

The hooks and the watcher read only that directory, so `--wake` refuses to run when the environment
points Iris at another one, as `IRIS_TOKEN_FILE` can. Turning wake off and integrating without it
work anywhere.

An integration made before ADR 030 kept the choice in Iris's MCP arguments. Reintegration moves it:
`--wake` or `--wake=true` there becomes `on`, anything else becomes `default`, and the arguments
become `mcp --client kimi` with no wake choice in them. `iris mcp --client kimi --wake` still starts,
but the flag does nothing: Iris starts no MCP web worker for any Kimi session.

`--wake` also installs a skill at `skills/iris/SKILL.md` in the Kimi home (`KIMI_CODE_HOME`, or
`~/.kimi-code`). It tells the model what each end of the task means. Turning wake off or removing
the integration takes back exactly the text Iris wrote; a copy someone changed stays as it is. A
file that already holds anything else is left alone, and Iris installs no skill there, because Kimi
reads a skill's frontmatter only at the top of the file. The request in the prompt and the
watcher's own lines still say what to do.

## What Iris does

1. At SessionStart, the hook records the native session id beside the Kimi process that runs it.
2. At a user prompt that starts a turn, while wake is on, the hook adds a request to the prompt: start
   one background Bash task with the exact command, the description `Iris: wait for new attention`,
   `run_in_background=true` and `disable_timeout=true`. It asks only when the hub has accepted the
   session and no watcher for it is running, requested or stopped (latched, below). A prompt typed
   into a running turn starts no turn, so it neither asks nor counts as an answer.
   It asks only an interactive session ([ADR 031](adr/031-wake-only-in-interactive-sessions.md)):
   - a run whose arguments Iris can read is not asked when they hold `-p` or a subcommand such as
     `acp`;
   - otherwise Kimi itself must read a terminal: fd 0 on Linux, its controlling terminal on macOS;
   - Kimi blanks its own arguments once it runs, so a `kimi -p` typed at a terminal is asked once;
   - a request skipped because of the process or its mode is noted as `wake not asked: <reason>`
     in the observed log. A live watcher, a pending request or a latch skips it without a note.
3. The watcher looks up the Execution that the hooks registered for that Kimi process and session,
   and only that one. It never registers an Execution of its own, and it keeps the Execution's lease
   alive without counting as a monitor, so prompt reminders go on as before.
4. When new mail or a freed claim arrives, it prints one fixed line, records how far it has read and
   exits 0. Kimi opens a turn, and the skill asks the model to use the Iris inbox and claims tools and
   to start exactly one replacement before that turn ends.

The first watcher of a session does not wake it for mail that was already waiting; the hooks and the
inbox cover that. A replacement starts reading where the previous one stopped, so attention that
arrived in between is not lost. The first new attention after a quiet spell wakes the session at
once. Attention of the same kind that follows within 30 seconds waits out the rest of that window
and can then cause one more wake, not one per event. Mail and claims are counted separately, and
mail the model has already fetched wakes nothing.

A watcher that ends without attention says why, and the skill asks the model not to start another:

| Exit | Meaning |
|---|---|
| 0 | New attention. Use the inbox and claims tools, then start one replacement. |
| 10 | Wake is off, the integration was removed, the Kimi process ended or its session changed. |
| 11 | Another watcher already waits for this session. |
| 12 | The watcher could not start, for instance because it found no Execution to bind. |
| 13 | It reached its 24-hour limit. |
| 14 | The hub no longer has the session. |
| 15 | Attention may be waiting but could not be recorded: use the inbox and claims tools once. |

After 10, 12 or 13, and when a request was not answered by the time of the next user prompt, the
session is latched: Iris stops asking. SessionStart, including a resume, clears the latch, and so does
a watcher the model starts anyway, for instance because the user asked for one.

`--no-wake` and removal stop every watcher whose recorded process id and start time still match a
running process, wait up to two seconds for it to end, and then latch its session disabled. A stop
that fails, or a watcher still running after the wait, is reported, and its record is left as it
is. A watcher that is not stopped notices within 20 seconds that wake is off and ends. Turning wake back on does not reach
sessions latched this way until they start again or resume.

## Limits

- **This asks the model; it enforces nothing.** The model can ignore the request, run another
  command, not fetch the mail or not start a replacement. Iris cannot verify the command it runs.
- If the model does not start a replacement after a wake, nothing wakes the session until the next
  user prompt asks again. Kimi 2.6.1 runs no hook on the turn a finished task opens and no Stop hook
  on any turn.
- `/clear` ends the task and starts a new session id. The new session gets a watcher at its next user
  prompt. Nothing starts a closed session.
- A task that ends during a busy turn opens its turn after that one ends. Empty and duplicate turns
  are possible: a freed claim stays historical and can cost one extra turn.
- `iris doctor` does not show the preference or the watcher records yet.
- Message bodies are untrusted data. Neither the watcher's line nor Kimi's task notification
  carries them.

The local `kimi web` API that ADR 026 used is not used: it could not reach a session open in the
Kimi TUI.
