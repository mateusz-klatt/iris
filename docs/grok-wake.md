# Grok notifications while a monitor is running

Iris asks Grok, at the end of a turn, to keep `iris watch --client grok` running in the
`monitor` tool. A line from that watch can open a model turn while Grok's auto-wake is on.
The line names a count. It never includes the message. Each such turn can spend model budget.

This is a request. Stop sees that some monitor is running. It sees the description
`[monitor]` plus the text the model gave the tool. It does not see the command, and it does
not check it. A different monitor satisfies the request. Shutdown and a closed channel do
not ask. A Grok build that ignores hook rewrites of a tool command cannot be given the
command by Iris directly.

Stop asks only an interactive session ([ADR 031](adr/031-wake-only-in-interactive-sessions.md)).
Iris reads the grok process's arguments.
- A run with `-p`/`--single`, `--prompt-file` or `--prompt-json`, or with a subcommand such as
  `agent`, gets no request.
- Any other run must also read a terminal.
- A flag Iris does not know, arguments it cannot read, or a hook whose nearest process is not grok
  also mean no request.

The observed log notes each skipped request as `wake not asked: <reason>`. On Linux the terminal
check reads grok's own input. On macOS and Windows it asks whether the hook has a terminal or
console.

The watch process does not inherit `GROK_SESSION_ID`. The hook records the session id for the
grok process, and `iris watch --client grok` reads it when the variable is empty. A session
that never reaches a repository, or whose start the hub rejected, has no such file. A crash
without SessionEnd leaves the file.

Disable or explicitly enable future sessions with:

```sh
iris integrate grok --no-wake
iris integrate grok --wake
```

`--no-wake` writes `off` in `iris-wake` under the Grok home (`$GROK_HOME` or `~/.grok`).
Setup and ordinary reintegration keep that file. A missing file means on, including an
integration installed before this choice existed. Stop reads the file locally and does not
call the hub to decide. `iris integrate --remove grok` removes Iris, including an `on` or
`off` marker. Other text in that file is left as it is.

`iris integrate grok` also installs a skill that says what an Iris line means. SessionStart
text is not part of Grok's prompt. The skill has to already be loaded. Reinstall keeps a
skill that still contains the notification sentence, so a person's additions stay. Remove
deletes Iris's exact text. A skill that was replaced, and no longer contains that text, is
left in place.

`iris doctor` does not report this marker. It does not say whether a monitor is running.
