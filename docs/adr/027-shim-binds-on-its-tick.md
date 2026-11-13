# 027: An unbound shim binds its session on its heartbeat tick

Status: accepted, 2026-11-13. The plan was reviewed three times, and the coordinator approved it
with five conditions. Changes D6, D8 and D9: `iris mcp` no longer waits for a tool call before it
renews its session.

## Problem

A shim renewed only a binding that a tool call had made. One that had no call yet, or whose
heartbeat had found its generation lost and had no call since, renewed nothing. Its session then
ran on the hook-only lease of D8, 15 minutes renewed only by hooks. A session idle past that lease
between hooks lost it, and the next registering hook took a new generation with
`reason=lease_expired proof=stale_retry source=hook`. While lost:
- peers showed it as lost;
- its claims stopped blocking;
- its fetched mail was requeued;
- senders saw it as unreachable.

Measured on the public hub on 2026-11-13, one Grok session on WSL took nine generations in four
hours, one at each turn of a background loop that ran every 30 minutes and made no Iris call:
- Its shim had bound on a call at 23:09 UTC and kept the session alive.
- Between 01:48 and 02:18 its renewals stopped reaching the hub for more than the 60 s lease. A
  second session on the same host lost its lease in the same window, which points to the host,
  but the cause was not established.
- From then on nothing renewed the session, until a call at 06:26 bound the shim again and the
  churn stopped.
- The same session lost two generations the evening before, while its newly started shim had not
  had a call.

A scratch hub reproduced the log line for a hook-only session. The same session with `iris
watch`, whose loop binds afresh, kept its generation. A real Claude Code session on a 30-minute
ScheduleWakeup loop that made no Iris call kept its generation too, because the plugin starts a
watch. The gap is a session whose only heartbeater is the shim:
- Cursor, Kimi and Antigravity have no monitor.
- Grok's monitor runs only after its Stop hook has asked the model to start one
  ([ADR 025](025-grok-stop-monitor.md)). The session above predates that.
- Any client's monitor can be missing.

## Decision

`binding.Maintain` looks for the session on each tick while the process holds no binding of its
own. In one hold of the binding's lock, after the parent check and `adopt`, the tick does the
following:
1. It goes on only when **all** of these hold:
   - Maintain runs for the shim, not for `iris watch`;
   - the client is not Codex;
   - the call mode is undecided or legacy;
   - the process holds no binding, owns no fallback Execution and has no handover pending.
2. It looks the session up as a call without a handle would, through rungs 1 to 3 of D6:
   - `IRIS_EXECUTION` while it is live, then the client's session id, then the client process;
   - zero to two bind requests, within its own five seconds;
   - no request at all when there is no evidence to look by.
3. If the lookup fails, nothing changes: no binding, and a confirmed loss stays confirmed, so a
   call still answers `lost` with its guidance.
4. A result this process found lost is refused, and nothing is renewed for it.
5. Otherwise the binding is taken, the loss is cleared, and the same tick renews it. The renewal
   puts the Execution into heartbeat mode with the 60 s lease.

The tick never registers: hooks, and the first call's existing fallback, stay the only
registrars. It never binds by a handle, which only a call carries. Its lookup cannot discover a
recovering or expired Execution, because the bind query and `live` refuse those. A heartbeat
after a successful lookup carries that binding's generation as proof, as any retained binding's
does, so a hub restart between the two is recovered as D9 describes.

The tick never decides how calls bind. So the wake rules of ADR 017 and ADR 026 are unchanged:
- A Codex process is never looked up for. Its calls can name their session, and then its first
  call puts it in id mode for life.
- A Kimi wake still needs a decided legacy process bound at rung 3. After a loss, such a process
  regains its wake target when the tick binds the new generation, and only for that generation.

A rebind after a confirmed loss writes one fixed observation, "rebound on heartbeat after a loss".
It is written outside the lock, only after the same tick's heartbeat succeeded, and doctor counts
it as a kind of its own. The observation shows only this: a heartbeat answered `lost`, and a
later tick bound the session again. `lost` covers an Execution that expired, ended or is unknown
to the hub. So rebinds that keep coming say that the session keeps losing its binding and getting
it back. A host that sleeps, or loses its network, past the lease is one possible cause; the
observation does not establish it, so it is a reason to look, not a diagnosis. Some rebinds go
uncounted. If the heartbeat right after a rebind fails with an error other than `lost`, the
binding stays and the loss is already cleared, so that rebind is never observed, not even by a
later successful heartbeat. The count is a diagnostic, not a tally of every rebind.

## Limits

- **A session with no live shim process.** A client that starts its MCP server for one turn, or
  whose MCP server died, has nothing to renew it between turns. It still loses its lease, which
  is correct, because nothing of it is alive.
- **A host that sleeps, or loses its network, past the 60 s lease.** The session still loses its
  generation, its claims and its fetched mail, and the next registering hook takes a new
  generation. This decision only stops that one loss from repeating at every later idle gap.
- **A process started for another reason.** Positive evidence does not say why a client started
  a process. One started to list tools, or for a single turn, that lives past a tick with session
  evidence binds its session and renews it. After it ends, the session is on the 60 s lease, and
  later hooks renew 60 s (D8). A single Iris call has always had the same effect. A listing that
  ends before its first tick (20 s) never looks.
- **Unchanged:** Codex (id mode, a root's loss and its wake), a decided sub-agent, and handles.
  A Codex root that loses its binding looks once and then waits for a call. That is the same
  class of gap, left for a change under ADR 017.
- **Cost.** One lookup per 20 s tick per unbound shim, with no backoff. A shim that can never
  bind, for example one whose hooks are disabled, keeps asking.

## Verification

- Unit tests in `internal/binding/tick_test.go` run the tick against an in-memory hub in synctest
  bubbles, on the real 20 s interval. They cover:
  - binding and renewing on the tick;
  - no request without evidence;
  - the fall-through from session id to client process;
  - a live or lost override;
  - a lost Execution refused;
  - no registration past any grace period;
  - each exclusion (Codex in every mode, id mode, handles, the watch, a fallback, a pending
    handover);
  - failed and stalled lookups;
  - a loss that stays confirmed until a bind;
  - a heartbeat failing after a lookup;
  - the Kimi wake target;
  - the parent check;
  - the observation, which is written outside the lock.

  Two real-time tests force a call and a tick to contend for the lock, in both orders.
- Integration tests run the real hub and store in the same bubbles:
  - **the regression case:** hooks every 30 minutes, no tool call, a live shim, two periods. It
    fails without this change: the first period's hook takes generation 2 on the hook-only lease.
    With the change, the session keeps generation 1 on the 60 s lease;
  - one generation lost to a sleep, then the hook's next generation bound and renewed;
  - recovering Executions not found by any evidence, with the claim and the fetched mail kept and
    a retained binding reattaching;
  - a short-lived process leaving the session on the 60 s lease.
