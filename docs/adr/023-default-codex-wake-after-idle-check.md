# 023: Default Codex wake after a positive idle check

Status: accepted on the owner's direction, 2026-11-12. Changes D7. Supersedes ADR 017's
opt-in default, its allowance for queueing after settlement without a native idle check, and
its automatic second queued wake for the same attention identity. It also changes binary and
version preflight failures from counted attempts to operational retries. ADR 017's hook reminders,
binding, content-free prompt, stream semantics and persistent guards remain in force.

## Problem

The owner wants a loaded Codex session to hear new Iris activity by default, without queueing
a notification merely because a turn is busy. The original runner settles activity and checks
what hooks already announced, but that does not establish whether the native thread is idle.
Its ten-minute second wake can also start another model turn for attention already announced.

The [earlier acceptance record](../research/clients/codex-iris-idle-wake-2026-11-12.md)
measures the ADR 017 implementation. It remains evidence for that version. It is not evidence
that the idle gate or defaults decided here have passed native acceptance on every platform.
Short- and long-thread wake costs remain unmeasured; this decision changes the default on the
owner's direction without presenting those measurements as complete.

## Decision

### Default and explicit off

`iris mcp --client codex` enables the runner by default. `--wake` and `--wake=true` explicitly
enable it; `--wake=false` disables it. No other client's default or attention mechanism changes.
Disabling the runner creates no runner stream, wake-state activity or runner subprocess.

Fresh Codex integration enables the runner. `iris integrate codex --no-wake` writes an explicit
`--wake=false` argument; setup and ordinary reintegration preserve that choice.
`iris integrate codex --wake` replaces explicit off with enabled. An existing child retains
its startup arguments until its MCP connection is restarted.

Older integration wrote the same bare `mcp --client codex` arguments both for the original
default-off state and after `--no-wake`. Those bytes cannot identify which choice a person
made. Bare invocations therefore take the new enabled default, including old entries that
once meant off. The upgrade must say so plainly and offer the explicit off command; it cannot
claim to preserve an old preference that was never recorded. Explicit `--wake=false`, including
manually written entries, survives setup and reintegration.

### Positive native idle evidence

The existing root binding, UUID, generation and hook-evidence checks still select the only
thread Iris may wake. The runner first establishes that authoritative attention still needs
a wake. It then uses the same absolute, version-matched Codex executable and inherited home
as the queue command to run the public `codex app-server proxy` command.

The proxy is a short-lived byte bridge. A bounded client carries an HTTP upgrade and WebSocket
through its stdin/stdout, performs `initialize` and `initialized`, and sends `thread/read`
with the exact root `threadId` and `includeTurns:false`. Only a response naming that same
thread with `thread.status.type` equal to `idle` admits the next step. `active`, `notLoaded`,
`systemError`, an unknown or missing status, a mismatched id, a protocol error or a deadline
does not establish idle. Iris defers the wake without spending an allowance.

The probe reads state only. It sends no subscription, resume or turn-start request, reads no
Codex database, and closes and reaps only its own proxy. It adds no host daemon or per-thread
scheduler. An unavailable or incompatible native API leaves ordinary Iris tools and hook
notifications usable; it does not justify queueing without idle evidence.

After a positive idle response, take fresh told/wake snapshots and read authoritative
attention again. If a hook already told the session or attention has gone, submit nothing.
The existing whole-file comparisons, private publication, final binding/start admission and
merge-by-attempt-id rules still apply. The read before a failed probe is also authoritative:
operational retries stop when attention no longer needs a wake.

### One automatic wake, retained accounting

The runner creates no automatic second queued wake for an unchanged attention identity.
Ten minutes without an inbox fetch does not itself start another model turn. ADR 017's
ten-minute prompt-hook reminder remains unchanged and may appear on a later turn that happens
for another reason. New pending attention can still qualify for its own initial wake.

Binary resolution, version checks and native status checks are preflight operations. A failure
defers with a bounded operational retry and a fixed diagnostic; it creates no new `binary_check`
attempt and spends no wake quota. Retrying does not repeatedly inspect a stale captured
attention result: ask the hub again and stop when no wake is needed. Retry delays retain at
least the existing failure backoff.

A known failure of an actual queue attempt still permits only one bounded retry. Signals,
timeouts, output overflow and uncertain queue outcomes retain an outstanding guard. Preflight
diagnostics contain fixed reasons rather than native payloads or process output.

Existing version-1 journals remain readable. Their retained quota and outstanding attempts,
including legacy second-slot or `binary_check` records, are not silently erased or refunded.
The new policy creates no second-slot attempt. A legacy unknown or unanswered queue continues
to block new submissions until its exact prompt hook consumes it. Recovery remains: resume a
live session; remove the exact guard only after permanent close and MCP shutdown.

## Limits

The native status read and queue command are separate operations. A person can start a turn
or fetch mail after the read; the queue may then run after that turn or find no work. A thread's
load state can also change after the observation. Iris does not cancel an admitted queue item
and does not guarantee zero empty or billable wake turns. A positive idle result is evidence
at the observation, not an atomic claim on the native thread.

A thread reported unloaded cannot pass the gate. This does not implement a wake service for
closed or unloaded sessions, and after the MCP child ends it has no listener. The existing
process-restart guard does not become a whole-host power-loss guarantee. Message bodies remain
untrusted data and never choose commands, identity or admission.

## Verification

Verify Codex's default and explicit-off CLI paths, fresh and bare-config migration, persisted
off through setup/reintegration, and unchanged other-client behavior. Native-probe tests must
assert the exact thread, read-only protocol and bounded cleanup, admit only verified idle, and
defer every active, unavailable, unknown or mismatched response without quota changes.

Verify fresh attention/told reads after idle: mail fetched or announced during the probe must
not cause a reservation. Keep the existing concurrent publication and late-answer tests.
Prove that repeated pending attention cannot create a ten-minute second native wake, while
prompt-hook reminders and legacy guards retain their behavior. Queue failures keep their
one-retry bound and unknown outcomes never gain a fresh allowance.

Run the repository's full local gates before the pull request is ready. Record native checks
for this new implementation separately from the earlier opt-in measurements, including busy
and idle threads and default/custom homes. A fixture or old native pass is not a new platform
acceptance claim, and model token usage is not a cost measurement unless measured as such.
