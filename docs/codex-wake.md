# Codex notifications while idle

Iris asks a loaded Codex root thread to read its inbox and claims when new attention arrives
and its native status is verified idle. This is enabled by default for Codex and uses the
thread's own model and queue. Each wake can start a model turn and spend your model budget.
Other clients keep their existing attention mechanisms.

Disable or explicitly enable future sessions with:

```sh
iris integrate codex --no-wake
iris integrate codex --wake
```

`--no-wake` records `--wake=false` in the MCP arguments. Setup and ordinary reintegration
preserve that explicit choice. A bare `iris mcp --client codex` enables wake; a manually
configured server disables it with `iris mcp --client codex --wake=false`. Restart the Codex
session's MCP connection after changing the arguments or upgrading the binary; an already
running child keeps its startup behavior. `iris integrate --remove codex` removes Iris.

Older versions represented both default-off and an explicit `--no-wake` choice by omitting
the flag. Those old entries are indistinguishable and take the new enabled default. Run
`iris integrate codex --no-wake` to record off explicitly for this version.

The thread must stay loaded, and its Iris hooks must be reviewed in Codex's `/hooks` screen.
It must make a root Iris tool call, such as `whoami`, so the MCP child learns its identity.
Subagents and legacy or overridden bindings cannot arm idle wakes. A thread reported unloaded
does not pass the idle check. After Codex unloads the thread and ends its MCP child, or after
daemon stop or reboot, no listener remains; mail is available at the next session hook.
Closing a TUI alone is not proof that its thread has already unloaded.

Iris settles activity, checks whether attention still needs a wake and reads the exact thread's
native status. Busy, unloaded, unavailable or unknown status defers without spending wake
quota. After verified idle it reads attention and told state again, so a hook announcement or
an inbox fetch during the check can remove the need to queue.

There is one automatic wake opportunity for new attention, with one retry only for a known
queue failure. The runner does not start another turn merely because the same mail remains
pending for ten minutes. A later prompt can still carry the usual hook reminder. An outstanding
queued wake blocks later ones until its exact prompt hook consumes it, including guards kept
by an older version.

Checking idle and queueing are separate operations. A person can start a turn or fetch mail
between them, so a queued wake can run later and find no work. Iris does not cancel it and
cannot guarantee zero empty or billable turns. Queue acceptance proves neither a new turn nor
an inbox fetch, and a model may ignore the note.

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
hosted-model cost evidence remain separate from configuration. [ADR 023](adr/023-default-codex-wake-after-idle-check.md)
records the current policy; [ADR 017](adr/017-wake-codex-by-queue.md) and its research record
retain the earlier implementation and measurements. Those measurements do not prove the new
idle gate on every platform.
