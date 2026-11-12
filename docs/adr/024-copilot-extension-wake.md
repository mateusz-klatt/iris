# 024: Wake loaded Copilot CLI sessions with an extension

Status: accepted on the owner's direction, 2026-11-12. Changes D7 for Copilot only.

## Problem

Copilot CLI's MCP integration and hooks cannot start a model turn when the CLI is idle.
Iris needs to notify a loaded Copilot session when its inbox or a claim waiter needs attention.
The CLI's extension SDK is experimental and can join the foreground session, observe its
lifecycle and submit messages; it is the documented route that works with an existing session.

## Decision

`iris integrate copilot` installs a user-scoped extension under
`$COPILOT_HOME/extensions/iris` (or `~/.copilot/extensions/iris`). The extension joins the
foreground session with `joinSession()`, starts `iris watch --client copilot`, and gives that
child the joined session's id as `COPILOT_AGENT_SESSION_ID`. Iris's existing Go watcher remains
responsible for binding, leases, reconnects, cursor durability, attention filtering and
coalescing.

The extension passes only recognized, content-free attention lines to Copilot. It submits a
fixed message with the `automation-iris` source, which has no human authority, and never copies
mail bodies or hub payloads into a prompt. When the extension has observed an active turn, it
waits for `session.idle` before sending. SDK message delivery uses the queue, so an initially
active session that has not yet emitted a lifecycle event can finish its current work first.
The message asks the agent to read the current inbox and claims state; it does not acknowledge
messages.

The JavaScript is a thin adapter required by Copilot's SDK. Iris still owns all attention,
identity and delivery policy in Go. The extension uses the Node runtime already provided by
Copilot CLI; users do not install Node for Iris.

Extensions require Copilot CLI's experimental feature to be enabled and the workspace to be
trusted. `iris integrate` reports this prerequisite but does not change the user's CLI settings.

The extension's two files are Iris's alone. Each begins with a header holding the SHA-256 of the
rest of the file. Installation replaces, and `iris integrate --remove` deletes, a file there only
while that digest still matches, whichever `iris` path or version wrote it, so a removal works
even after Iris's manifest was lost or the binary moved. A file that does not match is the
user's: it is never overwritten or removed, and installation stops with a message that names it.
Removal restores the hooks and MCP entries through Iris's integration manifest as before.

## Limits

The extension runs only while Copilot CLI has loaded it in a live foreground session. It does
not start or resume a closed CLI, does not wake an unloaded or powered-off machine, and depends
on the experimental SDK's lifecycle and message APIs. Each admitted wake can cost a model turn;
measured wakes ran two or three assistant turns. A busy session may read the inbox before a
queued wake arrives, so a no-op turn remains possible. Failures are reported with the session
log; pending mail remains durable for a later turn.

`iris watch` holds a further attention line for 30 s after the previous one, so mail that arrives
within that window is announced by a later wake, not at once. A session that starts or resumes
with unread mail is woken soon after it starts, which costs a turn even if the user is about to
read the mail; in one measured resume that took about 15 s.

Message bodies remain untrusted data. A notification contains no body and grants no authority.
The prompt does not ask the model to acknowledge anything and Iris never does, but the model may
acknowledge a delivery on its own, and in one measured run it did. The extension cannot ensure
the model obeys the prompt or that a third-party tool outside Iris will act safely. Copilot did
not show the wake message as a user message in the timeline; the tool calls and the reply were
visible.

The CLI gave the extension its own environment, including token-bearing variables, and the
extension passes it to `iris watch` with the session id added. The SDK documentation says
sensitive variables are removed; that was not observed. Whether Copilot's command sandbox lets
`iris watch` reach the hub is unknown: the measuring host could not run that sandbox. Only
Linux with Copilot CLI 1.0.109 was measured.

## Verification

[The measurement record](../research/clients/copilot-extension-wake-2026-11-12.md) ran the
real extension loader, `iris watch` and a model on Linux with Copilot CLI 1.0.109. It measured
the load and session identity, an idle wake about 0.12 s after a send, a busy session that was
not interrupted and was woken 130 to 145 ms after its turn ended, coalescing, `/clear`, resuming
with unread mail, a graceful exit that left no process, and the end of the extension host and its
watcher after `kill -9` of the CLI. It also records what it did not measure.

The extension's tests cover line filtering, session identity, active and idle transitions, send
failure reporting, monitor failure reporting, shutdown and the entry point, at 100% lines,
branches and functions (`make copilot-extension`). The Go tests cover installation,
idempotence, a custom `COPILOT_HOME`, the ownership header on upgrade and removal, and exact
restoration of the user's hooks and MCP entries.
