# 025: Ask Grok to keep an Iris monitor running

Status: accepted on the owner's direction, 2026-11-12. Changes D7 for Grok only.

## Problem

Grok has no plugin monitor. Its plugin format cannot ship the `monitors.json` Claude uses,
and a Grok MCP child does not run the Codex wake worker. A loaded Grok session therefore
hears new mail only on its next turn.

Grok 1.0.77 does have a `monitor` tool. With `GROK_AUTO_WAKE` left on, one line from a
monitor opens a model turn. `iris watch` prints a content-free line. The monitor shell does
not inherit `GROK_SESSION_ID`, so a watch started that way cannot bind unless something else
supplies the session id. SessionStart's `systemMessage` is stored as a hook annotation and
does not enter the saved prompt, so it cannot tell the model what the line means.

## Decision

### Session file

On a top-level SessionStart, after the hub has accepted the session, the hook writes that
session id into a file named by the grok process's pid and start time. The file is mode 0600
in a 0700 directory beside the sessions directory, not inside it, so pruning does not delete
it. A subagent shares the grok pid and does not replace the file. SessionEnd removes it only
after the hub has ended that execution and only when the stored id is still this session's.
A failed write is ignored. The hook still allows the session.

`iris watch --client grok` reads that file when `GROK_SESSION_ID` is empty and the nearest
client process is grok with a known start time. A value already in the environment wins.
Grok stays multi-session: the pid rung of binding is unchanged, and no wake worker starts
inside `iris mcp --client grok`.

### Stop asks for the monitor

Stop on `end_turn` asks the model to start the monitor tool once, with persistent set and
the command `iris watch --client grok`, until some background task has type `monitor`.
`stopHookActive` means this turn was already asked. `shutdown` and `channel_closed` are left
alone. A missing reason does not ask. A task list that cannot be parsed does not ask.

The ask runs before the repository check and does not call the hub. It stays inside the
hook deadline. `iris integrate grok --no-wake` writes a local marker, `off`, which Stop
reads from disk. Plain `iris integrate grok` does not change the marker, so an explicit off
survives reinstallation. `--wake` writes `on`. A missing marker means on, including an
integration installed before this choice existed.

### Skill

`iris integrate grok` installs `skills/iris/SKILL.md` under the Grok home. The skill carries
the notification sentence and says what the Stop ask can and cannot see. Remove deletes that
exact text. A skill a person replaced is left in place.

## Limits

This is a request to the model, not a check and not enforcement. Stop's `backgroundTasks`
entry has a description, `[monitor]` plus the tool description, and no command. Any running
monitor satisfies the ask. This Grok build ignores `updatedInput`, so the hook cannot rewrite
the command into `iris watch`. A short-lived command can be started again on later turns;
Grok stops honouring Stop after a run of continuations. A crash without SessionEnd leaves the
pid file. `iris doctor` does not report the marker. The line-to-turn primitive and the Stop
continuation were measured on 1.0.77; the installed integration has not been re-measured end
to end.

Owned ACP sessions are unchanged and still not built. Shutting down is not asked to start a
monitor.
