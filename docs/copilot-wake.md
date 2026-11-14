# Copilot CLI attention

`iris integrate copilot` installs Iris's hooks, MCP server and a small Copilot CLI extension.
The extension runs `iris watch` for the joined foreground session and submits a content-free
automation message when Iris reports new mail or a claim that became available. The model then
reads current state through Iris's inbox or claims tools; notifications do not include message
bodies and do not acknowledge them.

## Turn it on

Copilot CLI extensions are experimental. Start Copilot with `copilot --experimental`, or run
`/settings experimental on`, and trust the workspace when it asks. Then install or reinstall, and
restart the Copilot session:

```sh
iris integrate copilot
```

Iris does not enable experimental CLI settings on your behalf, so `iris integrate copilot` prints
that step after it installs. `/extensions manage` in Copilot lists the extensions it loaded and
their status. `iris integrate --remove copilot` removes the extension together with the hooks and
the MCP entry.

Iris marks the two files it writes in `$COPILOT_HOME/extensions/iris` (`~/.copilot/extensions/iris`
by default) with a header that holds the SHA-256 of the rest of the file. A later install or
removal changes only a file whose digest still matches, so removal deletes those files even if
Iris's record of the installation was lost. The hooks and the MCP entry are removed from that
record only, so without it they stay. If you edited a file there, emptied it, or put your own file
there, Iris keeps it. `iris integrate` names it and stops before it writes anything. If the record
shows that Iris wrote that file, `iris integrate --remove` names it too, still removes the hooks,
the MCP entry and the other file, then exits with status 1. Move the file aside and run the same
command again. Without that record, removal keeps the file without a message, and Copilot still
loads it. The `extensions/iris` directory may stay behind, empty.

## What it does and does not do

- It runs only in a live Copilot CLI session that loaded it. It cannot start a closed or unloaded
  session, and it does not wake a machine that is off.
- It starts its watcher only in an interactive session
  ([ADR 031](adr/031-wake-only-in-interactive-sessions.md)): a host whose `canvases` capability is
  `true`, as Copilot CLI's TUI is. A `copilot -p` run reports `false`, gets no watcher and is sent
  nothing. It logs nothing either, because `-p` prints session log lines into its answer. The
  capability stands in for a mode the SDK does not name.
- A session busy with a turn is not interrupted. The wake is sent after the turn ends.
- If Copilot accepts a wake but then sends neither `assistant.turn_start` nor `session.idle`,
  the extension keeps the session marked busy. Later wakes are held, not lost, until the next
  `session.idle`; the mail itself stays in the inbox.
- `iris watch` holds a further attention line for 30 s after the previous one. Mail that arrives
  within that window is announced by a later wake, not at once.
- A session that starts or resumes with unread mail is woken soon after it starts. That costs a
  model turn even if you were about to read the mail.
- Each wake can use model credits: the measured wakes ran two or three assistant turns. A queued
  wake can become a no-op if the agent handled the attention during its turn.
- The wake message is not shown as a user message in Copilot's timeline. The model's calls to the
  inbox and claims tools and its reply are.
- The prompt asks the model to look, not to acknowledge. Iris never acknowledges for it, but the
  model can acknowledge a delivery on its own, and one did when measured. Message bodies are
  untrusted data and never grant authority.
- The extension gets Copilot's environment, including any token variables it holds, and passes it
  to `iris watch`. The monitor's own status lines go to the session timeline as log entries, never
  as a message, so they start no turn: that notifications stopped, are still stopped or resumed, or
  that the hub rejected this host's token. If one says notifications stopped or the token was
  rejected, run `iris doctor`.
- With Copilot's command sandbox on, extensions may run inside it (`sandbox.sandboxExtensions`).
  Whether `iris watch` can then reach the hub was not measured.
- The wake end to end was measured on Linux with Copilot CLI 1.0.109 only. The canvases capability
  that tells the TUI from `-p` was measured on Linux with 1.0.112. Other versions, macOS and Windows
  are not covered.

The [measurement record](research/clients/copilot-extension-wake-2026-11-12.md) has the details
and [ADR 024](adr/024-copilot-extension-wake.md) the decision.
