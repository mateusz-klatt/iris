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

Iris does not enable experimental CLI settings on your behalf. `/extensions manage` in Copilot
lists the extensions it loaded and their status. `iris integrate --remove copilot` removes the
extension together with the hooks and the MCP entry.

Iris marks the two files it writes in `$COPILOT_HOME/extensions/iris` (`~/.copilot/extensions/iris`
by default) with a header that holds the SHA-256 of the rest of the file. A later install or
removal changes only a file whose digest still matches. If you edited one, or put your own file
there, `iris integrate` stops with a message that names it. Move the file aside and run it again.

## What it does and does not do

- It runs only in a live Copilot CLI session that loaded it. It cannot start a closed or unloaded
  session, and it does not wake a machine that is off.
- A session busy with a turn is not interrupted. The wake is sent after the turn ends.
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
  to `iris watch`. If the extension reports that its attention monitor stopped, run `iris doctor`.
- With Copilot's command sandbox on, extensions may run inside it (`sandbox.sandboxExtensions`).
  Whether `iris watch` can then reach the hub was not measured.
- Measured on Linux with Copilot CLI 1.0.109 only. Other versions, macOS and Windows are not
  covered.

The [measurement record](research/clients/copilot-extension-wake-2026-11-12.md) has the details
and [ADR 024](adr/024-copilot-extension-wake.md) the decision.
