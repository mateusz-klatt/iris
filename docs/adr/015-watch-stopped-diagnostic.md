# 015: Explain a watch that stays stopped

Status: accepted, 2026-11-09. Extends D9's watch recovery behavior.

## Problem

After losing its Execution, `iris watch` announces that notifications stopped and keeps
trying to bind to a replacement. Hooks must register a positively identifiable replacement
before watch can resume (D6, D9). If no replacement can be identified, the initial announcement
leaves the session waiting without explaining the failed retries or suggesting an action.

## Decision

One minute after the stopped announcement, the next completed retry prints at most one
additional diagnostic for that stopped period. The existing backoff remains capped at 30 s,
and binding calls retain their five-second deadline. There are no periodic reminders: each
line may become a model turn. A rejected token while stopped is included in this diagnostic
instead of producing a separate warning.

The resolver retains the underlying retry failure while still answering `lost` to callers.
Watch reports the most specific reason it recognizes: ambiguous matches, no positive binding,
a handle for another client or host, a stale handle, an unavailable Execution, refusal to
reuse an Execution already found lost, a rejected token, or a binding timeout. The line suggests restarting the
session or running `iris doctor`. For an unrecognized failure, the line explicitly says the
reason is unknown. Arbitrary error text is not printed.

No positive binding does not prove that hooks are missing. Watch can suggest checking hooks
with doctor, but cannot diagnose a missing hook from that result alone. The diagnostic grants
no authority: watch never registers after loss and never reuses an Execution already found
lost. A restart or doctor run is advice, not a guarantee of recovery.

A later positive replacement binding still produces the existing resumption line and follows
that Execution from its own cursor. Only resumption permits a later stopped period to receive
another diagnostic. A session that resumes before the minute receives none.

## Verification

Tests advance the injected clock without sleeping to assert the minute boundary, no repeated
diagnostics as failures change, normal resumption, cursor isolation, and a new diagnostic for
a subsequent stopped period. Binding tests assert that failure detail survives while typed
loss, refusal of stale identities, and the prohibition on registration remain intact.
