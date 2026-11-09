# Codex peer after its caller ended

Measured on 2026-11-09, Windows 11 Pro build 26300, Iris `2fab012`, Go 1.27.3
windows/amd64, Codex CLI 0.167.0, Node 26.12.1. The installed Codex MCP bridge
identified itself as `claude-delegator-codex` 3.0.0; its clean source checkout was
`5c88da94d113ab5319fb178f063f77f20ba5ec5c`.

**Finding:** this reproduction showed stale lease state after process exit. It also
confirmed that ending the caller's Iris Execution does not end a live Codex process.
`active` alone cannot distinguish those cases. The lease subsequently expired as D8
specifies, so this records a limit of the existing mechanism, without changing it.

## Isolation and method

A temporary directory held a newly built Iris binary, database, token, Git repository,
Codex home, and measurement logs. The only hub was a child process running:

```text
iris serve --listen 127.0.0.1:49165 --db <scratch>/hub.db --token-file <scratch>/hub.token
```

The scratch repository had an `example.invalid` origin. Children received
`IRIS_HUB_URL` and `IRIS_TOKEN_FILE` for that hub; `IRIS_EXECUTION` and inherited
client identity variables were removed. The scratch Codex configuration contained
only the Iris MCP server, forwarding the two scratch hub variables. Its hooks called
the scratch Iris binary. Authentication was copied into the temporary Codex home
without logging it. No user configuration was changed.

A Python JSON-RPC controller called the installed bridge's `codex` tool over stdio,
with the scratch repository as `cwd`, `gpt-6-luna`, low effort, a read-only sandbox,
and a 180-second timeout. The bridge launched real `codex exec --json` processes.
The prompt asked for Iris `whoami` with the SessionStart handle, followed by a
50-second PowerShell sleep. Both sessions' logs confirm successful `whoami` calls
with `binding_rung: 4` and the scratch repository ID.

For these temporary hooks only, a Node preload added
`--dangerously-bypass-hook-trust` to the bridge's Codex argument vector and logged
the spawned PID. This was an explicit automation override for the inspected scratch
hooks, not an integration change or a measurement of persisted hook review. The
bridge source and its process supervision were unchanged. Codex warned that it
could not create PATH aliases under a temporary home; the hooks and Iris MCP call
nevertheless ran.

The controller registered a synthetic Claude caller Execution through the scratch
hub API, then ended it through `/v1/executions/<id>/end`. This models the hub-side
session-end signal; it is not a real Claude Code shutdown. It then terminated only
processes it had started, as described below.

Process liveness was measured independently of Iris: a retained Windows process
handle was opened for each hook-recorded `client_pid`, its `GetProcessTimes` creation
time had to equal `client_pid_start_ms`, and `WaitForSingleObject(handle, 0)` checked
exit. This avoids interpreting a retained or reused PID as a live process. SQLite
was read with `mode=ro` for the Execution's state, parent link, heartbeat mode and
deadline. Authenticated `/v1/status` requests triggered ordinary hub expiry before
each state sample. No database rows or clock values were altered.

## Observations

Times below are controller monotonic seconds from the start of the successful
probe. `deadline_tick_ms` is the hub's separate monotonic counter; absolute values
are not wall-clock timestamps. Both Codex Executions had `parent_id = NULL` and
`heartbeat_mode = 1`.

The first Codex PID was 29800, with creation time `1794231557877` ms since the Unix
epoch. Its retained process handle matched that identity.

| Probe time (s) | Event | Codex process | Iris state | deadline_tick_ms |
|---:|---|---|---|---:|
| 20.741 | Caller Execution confirmed `ended` | sampled below | caller `ended` | caller only |
| 20.749 | Child sampled after caller end | alive | `active` | 80233 |
| 20.759 | Owned bridge PID 36836 terminated and waited for | sampled below | sampled below | unchanged at next sample |
| 21.766 | After bridge exit | exited | `active` | 80233 |
| 46.777 | After bridge exit | exited | `active` | 80233 |
| 81.793 | After bridge exit | exited | `lost` | 80233 |

The child was already dead at the first sample 1.007 seconds after bridge exit.
Its lease deadline did not advance in the later samples. Stale `active` state was
still observed 26.018 seconds after bridge exit; `lost` was observed at 61.034
seconds. The coarse sampling bounds the transition rather than locating it exactly.

A second bridge invocation registered Codex PID 18416, creation time
`1794231639193` ms. The same caller Execution remained ended. This control terminated
the owned Codex process directly while leaving its bridge available:

| Probe time (s) | Event | Codex process | Iris state | deadline_tick_ms |
|---:|---|---|---|---:|
| 98.112 | Child bound; process identity verified | alive | `active` | 157250 |
| 113.601 | Before direct child termination | alive | `active` | 164189 |
| 113.634 | Termination completed; handle signalled | exited | `active` | 164189 |
| 163.609 | Last sample before expiry | exited | `active` | 164189 |
| 164.621 | First sample after expiry | exited | `lost` | 164189 |

The deadline advanced while the child was alive after the caller had ended. The
dead-process samples then distinguish stale hub state from a running child. With
the deadline unchanged after exit, this control observed `active` 49.975 seconds
after termination and `lost` at 50.987 seconds. Its expiry transition is bounded
to that 1.012-second sampling interval.

## Explanation and limits

The store grants hook registrations a 900,000 ms lease; a bound MCP shim or watch
renews a 60,000 ms lease. Hook activity preserves the shorter lease once heartbeat
mode has been established (`internal/store/sessions.go`, registration and heartbeat).
`expire` in `internal/store/store.go` checks hub deadlines, recovery and epoch, not
OS processes or parent Execution state. `end` in `internal/store/sessions.go`
updates only the named Execution. A Claude-to-Codex bridge call does not provide
the same-client parent-session identity that Iris's subagent registration uses.

`iris mcp` watches its own immediate OS parent, which is the delegated Codex client
or its launch shell. It does not watch the outer Claude session. A genuinely live
Codex client can therefore continue renewing after that caller ends. Conversely,
process exit without a successful SessionEnd leaves `active` until lease expiry:
up to 60 seconds from the last renewal in heartbeat mode, or 15 minutes for a
hook-only session. Parent loss stops renewal before the next heartbeat; it is not
an immediate hub-side end signal.

These runs exercised abrupt bridge and child termination, not graceful Claude
shutdown, a cancelled MCP request, or every bridge cleanup path. They do not explain
the exit mechanism of the first Codex child. They cannot establish the process state
of the originally reported production peer: no historical PID/creation-time sample
was supplied, and no production peer was inspected. The 15-minute hook-only bound
comes from code and the lease tests, not a 15-minute wall-clock run. Process-alive
means the OS process had not exited; it does not prove useful model progress.

For a new incident, record the host, session ID, PID and creation time, check that
exact process on its host, then sample the hub state across its lease deadline.
Check whether a bound MCP process is renewing it; merely having an MCP server
configured does not bind it. Ending a caller is not evidence that its delegate
exited. This is diagnosis, not process termination enforcement.

The private hub and owned children were stopped, and copied authentication was
removed after measurement. Host automatic approval review rejected removal of the
remaining scratch directories with `blocked by policy`; they were left in place.
Production was used only for this investigator's own session identity and the
explicitly requested coordination checkpoints.
