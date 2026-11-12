# Codex notifications while idle

Iris can ask a loaded Codex thread to read its inbox and claims when new attention arrives.
This is opt-in: each wake can start a model turn and spend your model budget. It uses the
thread's own model and queue. Default integration keeps the usual hook notifications.

Enable or disable future sessions with:

```sh
iris integrate codex --wake
iris integrate codex --no-wake
```

Reintegrating without either flag preserves the current choice. Restart the Codex session's
MCP connection after changing it; an already running child keeps its startup arguments.
`iris integrate --remove codex` removes the Iris integration. For a manually configured MCP
server, the enabled command is `iris mcp --client codex --wake`.

The thread must stay loaded, and its Iris hooks must be reviewed in Codex's `/hooks` screen.
It must make a root Iris tool call, such as `whoami`, so the MCP child learns its identity.
Subagents and legacy or overridden bindings cannot arm idle wakes. A closed or unloaded thread,
stopped daemon or reboot has no listener; its mail remains available at the next session hook.

Iris settles activity for ten seconds before checking whether the hooks already announced it.
It allows one wake for new pending attention and one further announcement after ten minutes.
An outstanding queued wake blocks later ones until its exact prompt hook consumes it. Queue
acceptance alone proves neither a new turn nor an inbox fetch, and a model may ignore the note.
This bounds repeated wake attempts; it does not promise prompt delivery.

Run `iris doctor` to see the configured tier and local wake state. Configuration, an eligible
root and an accepted event stream are separate facts. The existing `iris doctor --live` hook
probe does not prove idle wake delivery. Native delivery evidence must connect an incoming
delivery to an already idle same-thread turn, its exact prompt, consumption and inbox result.

If a wake remains outstanding, resume that session so its queued prompt can run. There is no
timeout that clears its guard. Remove the exact wake file named by doctor only after permanently
closing that session and stopping its MCP child. Removing a live guard can queue duplicates.
Do not clear all session state to repair one guard.

The state guards against cooperating process restarts. Whole-host power-loss durability is not
proved, and reservation loss in that case can permit another wake. Platform acceptance and
hosted-model cost evidence remain recorded separately in [ADR 017](adr/017-wake-codex-by-queue.md).
