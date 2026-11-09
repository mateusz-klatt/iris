# Iris gap re-audit — 2026-11-09

Baseline: `4840765fbd672dbb82921bb4793795aa1136b9e7`. Scope: all **36** findings in
`docs/research/gap-review-2026-11-07.md`: A1–A17, B1–B7, C1–C8, D1–D4. Source references below are
repository-relative paths and line numbers **at this commit**, read from an immutable `git archive`
snapshot. Moving main and later PRs are not included.

Method: read-only review of Iris's AGENTS, requirements, architecture, ADRs, implementation and test
sources. This source review ran no tests, client/model invocations or deployments. No external
implementation sources were consulted. “Closed” means the original defect is addressed in the
inspected code and/or the documented contract is explicitly corrected; it does not certify that
tests pass at this SHA or that every client/platform has been measured. “Partly” identifies a
substantive remaining edge or promise. Test evidence is source inspection, not a fresh test result.

## A. Requirements

**A1 — closed. Explicit claims protect ignored paths.**
[internal/store/claims.go:232](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/claims.go#L232)
checks conflicts over both normalized and ignored requested paths; ignore rules only exclude
automatic grants.
[internal/store/ignored_test.go:8](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/ignored_test.go#L8),
`TestExplicitClaimsProtectIgnoredPathsFromAnotherSessionsEdit`, exercises another session's
**touch** against exact, prefix and repository-wide explicit claims, plus the holder's own allowed
edit. The no-explicit-claim case at line 41 remains allowed.

**A2 — closed, with a specified occupied-mailbox boundary.**
[internal/store/sessions.go:371](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/sessions.go#L371)
prefers the resumed session's previous mailbox through `priorAgent` at line 663, keyed by
host/client/session/repository/worktree and excluding a mailbox currently held by an active
execution.
[internal/store/mailbox_test.go:11,52](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/mailbox_test.go#L11)
covers ordinary resumption and the occupied-mailbox fallback;
[internal/store/churn_test.go:19](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/churn_test.go#L19)
verifies three distinct mailboxes and distinct unread messages through all six restart orders. This
preserves continuity when the prior mailbox is free; it does not take a busy mailbox away from
another live session.

**A3 — closed. Foreign metadata is escaped.**
[internal/hook/context.go:26,59,71](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/context.go#L26)
passes handles, peer fields and claim fields through `display.Inline`;
[internal/store/claims.go:75](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/claims.go#L75)
also refuses control characters on explicit-claim paths.
[internal/hook/context_escape_test.go:12](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/context_escape_test.go#L12),
`TestSessionContextKeepsEveryForeignValueOnOneLine`, inserts forged newline guidance across display
fields and asserts that no standalone forged line is emitted. Escaping preserves the distinction
between data and Iris text; it is not model-level isolation.

**A4 — partly. Prompt/allowed-edit fallback exists; the all-client promise is broader.**
[internal/hook/runner.go:171](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/runner.go#L171)
now registers/checks attention on eligible prompt events and line 233 adds an attention note to
allowed pre-edit context for Claude/Codex.
[internal/hook/attention.go:18](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/attention.go#L18)
enables prompt context for Claude, Codex, Copilot, Kimi and Antigravity; line 31 limits tool context
to Claude/Codex.
[internal/hook/attention_test.go:39,88,104,122,172](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/attention_test.go#L39)
covers new counts, suppression, supported clients, outage fail-open and allowed edits. These fix the
original complete absence. Generic post-tool/read-only/Stop events are not attention checks, and
Grok/Cursor CLI have no prompt path here, despite
[docs/architecture.md:248](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L248)
saying next-turn context for all with limits. Counts, rather than event identity, decide whether a
note is new (`attention.go:45–71`). Native idle Codex attention is not demonstrated by these tests.
The separate [Codex attention measurement](clients/codex-attention.md) establishes client
capabilities; it does not change the Iris implementation assessed here.

**A5 — closed for the emitted-line checkpoint boundary.**
[internal/watch/watch.go:491](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/watch/watch.go#L491)
checkpoints only when the coalescer holds no unprinted notification;
[internal/store/api.go:204](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/api.go#L204)
reads an initial cursor/pending snapshot without creating a delivered cursor.
[internal/watch/cursor_test.go:22,139,175](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/watch/cursor_test.go#L22)
covers held frames and failed saves,
[internal/watch/loss_test.go:25](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/watch/loss_test.go#L25)
replaces the Runner after lost/held output, and
[internal/hub/stream_failure_test.go:205](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hub/stream_failure_test.go#L205)
retries after an initial reset write fails. Printed stdout is the boundary: these tests do not
establish that a client/model consumed that line.

**A6 — closed. Reintegration preserves unrelated handlers.**
[internal/integrate/jsonfile.go:53](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/integrate/jsonfile.go#L53)
removes only matching Iris handlers, preserves sibling handlers and their group properties, and
places Iris in its own group so a user matcher cannot suppress Iris.
[internal/integrate/remove_test.go:489](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/integrate/remove_test.go#L489),
`TestReintegrateKeepsSiblingHandlersAndGroupKeys`, verifies siblings, matcher preservation, direct
handlers and duplicate Iris groups.

**A7 — partly; P1 remains. TOML conflict scanning is not complete validation.**
[internal/integrate/toml.go:19,56](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/integrate/toml.go#L19)
now refuses several existing unmarked Iris definitions.
[internal/integrate/toml_conflict_test.go:13](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/integrate/toml_conflict_test.go#L13)
exercises ordinary/quoted table headers and several dotted/inline forms without changing the
original TOML. However, the implementation still scans lines and never parses complete input/output
TOML. In particular, inline-value matching removes double quotes only (`toml.go:83`): an inline root
definition using a single-quoted key, `mcp_servers = { 'iris' = { command = "mine" } }`, is not
recognized by that branch and can be followed by a conflicting appended `[mcp_servers.iris]`. This
is a code-inspection counterexample, not an executed client failure. Installation also remains
sequential: Codex writes hooks at
[internal/integrate/codex.go:56](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/integrate/codex.go#L56)
before checking TOML at line 79; a later refusal can leave a partial installation. `Layout.update`
at
[internal/integrate/integrate.go:361](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/integrate/integrate.go#L361)
is per-file, not an installation transaction. Full TOML validation and multi-file failure behavior
remain unresolved.

**A8 — closed for both original identity errors.**
[internal/store/sessions.go:147](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/sessions.go#L147)
preserves nondefault remote ports; `rememberRepository` at line 279 persists newly learned root
identity and aliases.
[internal/store/identity_test.go:7](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/identity_test.go#L7)
verifies distinct endpoints do not join; line 32 tests unknown root → root A learned → same-root
host joins → contradictory root B stays separate. Later identity-rewrite/repair work does not negate
these tests. This does not claim identity certainty when root evidence is unavailable; the current
architecture documents that limit.

**A9 — partly; stable send retries still have a boundary defect.**
[internal/mcpserver/server.go:239,338](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/server.go#L239)
derives an omitted send key before the side effect from execution/generation/arguments and a fixed
ten-minute clock bucket.
[internal/mcpserver/retrykey_test.go:22](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/retrykey_test.go#L22)
covers a lost hub response followed by a same-bucket retry; explicit keys win. But line 336
explicitly says crossing a bucket boundary creates a new key: even an immediate retry across that
boundary can send twice, and rebinding to a new execution/generation also changes the derivation.
The tool description at line 77 overstates this as retrying the same call being unable to send
twice. Console draft keys and synchronous duplicate-submit protection are fixed
([web/src/App.tsx:92,580,593](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L92);
[web/src/App.test.tsx:108,165,188](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.test.tsx#L108)).
Console keys are retained in the in-memory draft, not established as durable across browser
recreation. Reliably retryable ambiguous sends still require an operation identity stable across the
actual retry lifecycle.

**A10 — partly; transport loss and recipient loss are fixed, client consumption is not observable.**
[internal/store/messages.go:162](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/messages.go#L162)
retains attention when receipt mode is requested; the MCP shim requests it at
[internal/mcpserver/server.go:168](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/server.go#L168)
and posts receipts only after decoding at line 286.
[internal/store/store.go:441](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/store.go#L441)
wakes missed receipts once, and `requeue` at line 472 restores attention on unacknowledged mail from
a lost execution. Evidence:
[internal/store/receipt_test.go:11,76,113,134](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/receipt_test.go#L11),
[internal/mcpserver/receipt_test.go:60](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/receipt_test.go#L60),
and
[internal/store/requeue_test.go:98,132](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/requeue_test.go#L98).
Thus ADR 010's header saying implementation is planned is stale. Its substantive limitation remains:
the shim sends the receipt **before** returning the tool result; cancellation/discard/context loss
on the client side can still quiet unseen mail while the execution remains alive
(`docs/adr/010-fetch-receipt.md`, decisions 5 and 7). Legacy clients without receipt mode
deliberately retain old fetch semantics. Do not claim end-to-end model receipt or handling.

**A11 — closed for the original wire/UTF-8 mismatch.** The store byte-bounds encoded inbox pages at
512 KiB
([internal/store/messages.go:12,137](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/messages.go#L12)),
reports `more`, and advances after acknowledgements.
[internal/hubclient/client.go:193](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hubclient/client.go#L193)
reads one byte beyond its 1 MiB cap and returns a distinct oversized-response error. Console
sendability uses `TextEncoder` byte lengths
([web/src/App.tsx:53,67](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L53)).
Tests:
[internal/store/inbox_budget_test.go:14](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/inbox_budget_test.go#L14),
[internal/hubclient/limit_test.go:14](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hubclient/limit_test.go#L14),
[web/src/DraftBytes.test.tsx:22](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/DraftBytes.test.tsx#L22).
Model-output budgeting is separately evaluated under D4.

**A12 — closed under explicit reported-handling semantics.**
[internal/store/messages.go:214,230](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/messages.go#L214)
records acceptance and optional handled time separately; sender-only `receipts` at line 244 exposes
per-recipient state. MCP exposes `ack`/`receipts`
([internal/mcpserver/server.go:82–83](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/server.go#L82)),
and ADR 005 defines the distinction.
[internal/store/receipts_test.go:63,218,240,270,281](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/receipts_test.go#L63)
covers states, handled-implies-accepted, later recipient/sender generations and authorization.
“Handled” is the recipient's declaration, not independent verification that requested work occurred.

**A13 — closed. Human unread can be cleared.**
[internal/store/operator.go:446](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/operator.go#L446)
acknowledges human deliveries when a thread is read, scoped to the selected repository;
[web/src/App.tsx:484](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L484)
calls the read endpoint only when that view contains unread mail.
[internal/store/read_test.go:8](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/read_test.go#L8)
verifies unread transitions; console tests at
[web/src/App.test.tsx:413,565](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.test.tsx#L413)
cover failure preserving unread and avoiding unnecessary repeated reads. Account-console read state
now has additional scoped behavior; this item only concerns the original permanently unread human
mailbox.

**A14 — closed. Refresh preserves edited replies and chooses the counterpart.**
[web/src/App.tsx:108](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L108)
selects the latest non-human sender or the human's recipients; line 497 preserves edited drafts and
keys, indexed by repository/thread scope.
[web/src/ReplyDraft.test.tsx:76,106,152,163,174](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/ReplyDraft.test.tsx#L76)
covers unrelated refresh, untouched suggestions, follow-ups after human messages, human-only threads
and StrictMode behavior.

**A15 — closed for the original pagination omission/tie gap.**
[internal/store/operator.go:324](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/operator.go#L324)
continues in `(last visible timestamp, thread id)` order using a thread-id cursor and returns `next`
at line 360.
[web/src/App.tsx:437](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L437)
loads requested pages and deduplicates after refresh.
[internal/store/thread_pages_test.go:11](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/thread_pages_test.go#L11)
exercises identical timestamps;
[web/src/OlderThreads.test.tsx:50,62,83,103](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/OlderThreads.test.tsx#L50)
exercises continuation, refresh, duplicates and filter reset. This is browse continuation, not a
promise of a frozen history snapshot during concurrent writes.

**A16 — closed. Claimless execution loss refreshes the console.**
[internal/store/store.go:390–399](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/store.go#L390)
emits a generic change when an active execution becomes lost, independent of claims.
[internal/store/expiry_event_test.go:11](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/expiry_event_test.go#L11)
advances time with no claims or unrelated activity, asserts the UI event cursor advances and the
overview shows lost, then asserts no repeated event. The recovery-window expiry case is separately
tested at line 35.

**A17 — closed for proof and fail-open diagnostics.**
[internal/hook/reconcile.go:148](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/reconcile.go#L148)
uses generation-aware combined registration/reconciliation or the shared registration fallback; its
deadline/panic/error paths settle diagnostics at line 79 while returning valid fail-open output.
[internal/hook/reconcile_recovery_test.go:29,67,166](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/reconcile_recovery_test.go#L29)
covers remembered proof, repository/hub/git/reconcile failures, panics, timeout races and a budget
exhausted before the worker starts. Shell status comparison retains its documented inability to
infer every filesystem edit or make after-the-fact conflicts preventive.

## B. Behavioral proof

**B1 — closed.**
[internal/store/race_test.go:16](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/race_test.go#L16),
`TestRacingMultiPathClaimsAreAllOrNothing`, races twenty contenders over exact/case-varied
multi-path touches, both rename endpoints, and an explicit overlapping prefix. It inspects stored
holdings for every contender and requires all-or-none plus exactly one shared-tree holder (lines
64–84), repeated over five rounds. This is the previously missing combined scenario; it was not
executed during this review.

**B2 — closed.**
[internal/watch/loss_test.go:25](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/watch/loss_test.go#L25)
constructs a fresh resolver/Runner after failed printing or death during coalescing, with no new
unrelated traffic.
[internal/hub/stream_failure_test.go:205](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hub/stream_failure_test.go#L205)
injects the initial pending reset write failure and requires a reset on reconnect. These go beyond
reconnecting one in-memory Runner. They are programmatic recovery simulations, not a real
client/model attention measurement.

**B3 — closed.**
[internal/store/churn_test.go:19](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/churn_test.go#L19)
gives three concurrent sessions distinct mail, ends them, resumes all six permutations, and asserts
each session retains its mailbox and only its own message. It also checks a newcomer does not take
an actively held mailbox. Occupied-prior-mailbox behavior has its own test at
[internal/store/mailbox_test.go:52](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/mailbox_test.go#L52).

**B4 — closed.**
[internal/store/ignored_test.go:8](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/ignored_test.go#L8)
now tests the actual `touch` operation against explicit ignored-path claims, including exact, prefix
and repository-wide cases. The prior wrong-operation-only proof is no longer the only evidence.

**B5 — closed.**
[internal/store/identity_test.go:7,32](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/identity_test.go#L7)
covers both nondefault ports and learned-root contradiction, including real store registrations and
checking the persisted root column. These supplement the original equivalent-spelling tests.

**B6 — partly.**
[internal/mcpserver/retrykey_test.go:22](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/retrykey_test.go#L22)
loses the first HTTP response and checks equal derived keys on retry, and
[web/src/App.test.tsx:108](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.test.tsx#L108)
checks a lost console response keeps its draft key. However, the MCP test reuses one connected
session and a fake hub; it neither recreates the MCP client nor asserts one actual committed logical
message in the store. Console tests likewise reuse the mounted draft. No inspected test covers a
lost response across the automatic key's clock-bucket/execution boundary. The stronger failure
sequence requested by B6 remains unproved and A9 shows why it matters.

**B7 — closed for the old false-positive test; native measurement remains separate.**
[internal/doctor/live_test.go:94](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/live_test.go#L94)
now has an execution-handle echo mode without the binding operation;
`TestLiveRunSaysWhichPartFailed` at line 156 requires that case to fail MCP proof. The successful
fake path at line 98 performs the binding operation used by `whoami` and reports the scratch
repository identity. This is a negative-test improvement and a simulation of the MCP path, not an
actual native model invocation or a complete MCP framing end-to-end test. C2 evaluates the
production diagnostic criterion.

## C. Documentation versus implementation

**C1 — closed. Authenticated redirects are refused.**
[internal/hubclient/client.go:188,252](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hubclient/client.go#L188)
wraps default/custom/hedged clients with unconditional redirect refusal without mutating the
caller's client. Doctor uses the same policy
([internal/doctor/doctor.go:615](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/doctor.go#L615)).
Tests inspect the redirect target remaining untouched:
[internal/hubclient/redirect_test.go:15,40](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hubclient/redirect_test.go#L15),
[internal/doctor/redirect_test.go:13](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/redirect_test.go#L13),
and the WebSocket handshake at
[internal/watch/redirect_test.go:17](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/watch/redirect_test.go#L17).
This is source evidence for the credential-downgrade fix, not a live network/TLS measurement.

**C2 — closed for the hook-handle false positive.**
[internal/doctor/live.go:210](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/live.go#L210)
now requires the scratch repository ID in client output; SessionText does not reveal it
([internal/hook/context.go:19](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/context.go#L19)).
[docs/architecture.md:692](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L692)
explains why the old execution handle proves nothing.
[internal/doctor/live_test.go:156](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/live_test.go#L156)
requires both no-MCP and handle-only echo cases to fail, while the success fake performs the binding
operation at line 98. The signal establishes that otherwise unavailable scratch identity came back;
it is neither cryptographic proof nor a fresh native MCP/model measurement.

**C3 — closed for integration completeness and a no-op canary.**
[internal/doctor/doctor.go:338](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/doctor.go#L338)
compares installed configuration to dry rendering; the canary must return its random receipt
(`doctor.go:324,354`).
[internal/doctor/drift_test.go:45,77,95,122](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/drift_test.go#L45)
covers removed hook/MCP entries, broken sibling configuration and a fallback-only response;
[internal/doctor/receipt_test.go:26](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/receipt_test.go#L26)
tests real hook invocation forms. Documentation distinguishes this offline proof from client loading
([docs/architecture.md:649](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L649),
[docs/join-hub.md:167](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/join-hub.md#L167)).
Complete TOML validity remains A7, and Codex trusted-hash content is explicitly not verified by this
offline check
([docs/architecture.md:619](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L619)).

**C4 — closed, including a corrected console promise.**
[internal/hook/runner.go:120,127,316](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/runner.go#L120)
records cross-loading and bounded unknown-tool observations; doctor summarizes them
([internal/doctor/doctor.go:406](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/doctor.go#L406)),
obtains last-hook evidence for the authenticated host (line 753), and warns about a sensitive
repository on a folding checkout (line 773). Tests:
[internal/hook/observed_test.go:12,29,50](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/observed_test.go#L12),
[internal/doctor/lasthook_test.go:15](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/lasthook_test.go#L15),
[internal/store/hookseen_test.go:16](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/hookseen_test.go#L16),
[internal/doctor/case_test.go:30](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/case_test.go#L30).
The console now explicitly reloads stored reports and asks the user to run doctor on the client
computer
([docs/architecture.md:799](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L799),
[web/src/App.tsx:891](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L891)).
It does not execute doctor remotely. D2 separately tracks incomplete freshness presentation.

**C5 — closed. Release wakes interested waiters.**
[internal/store/waiters.go:19,42](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/waiters.go#L19)
records denied paths and emits `claim_available` only to active waiters whose paths are free.
Ordinary broadcast `claim_released` frames do not wake the watch coalescer
([internal/watch/watch.go:62](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/watch/watch.go#L62)).
[internal/store/waiters_test.go:40,56,78,97,191](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/waiters_test.go#L40)
checks no-waiter silence, one-time directed wakes, remaining holders, prefix overlaps and
ended/family cases. Interest expires at the blocking claim's expiry or one hour; the test at line
171 deliberately keeps an expired waiter quiet. This is not a guarantee that every expired claim
wakes everyone who once wanted it.

**C6 — open. Promised repository policy controls are not exposed.**
[docs/architecture.md:121,441,445](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L121)
still describes repository fail-closed opt-in, configurable `claim.ignore`, and conflict actions.
Ignore policy is hardcoded
([internal/store/claims.go:103](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/claims.go#L103));
`SetRepositoryPolicy` at line 136 has only test callers, with no supported CLI/API/configuration
path. Hook failures still allow
([internal/hook/runner.go:83,98](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/runner.go#L83)).
Tests that inject policy directly
([internal/store/store_test.go:482](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/store_test.go#L482),
[internal/store/validation_test.go:13](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/validation_test.go#L13))
do not supply operator controls. Implement the documented controls or explicitly defer/narrow these
promises. ADR 003's internal-only case-policy exception does not resolve the other controls.

**C7 — closed, within explicit attribution limits.** Post-write reconciliation now `autoClaim`s
changed paths
([internal/store/reconcile.go:95,114,167](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/reconcile.go#L95)),
preserving others' claims, honoring ignores and a 500-claim limit with an omission message.
[internal/store/reconcile_test.go:48](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/reconcile_test.go#L48)
asserts claims, subsequent denial to another session and renewal; line 100 covers the cap, and lines
10/33 cover shared-worktree silence and a first-post baseline. Shared worktrees cannot attribute
shell changes to one session (`reconcile.go:49`); unchanged porcelain state and later background
writes remain invisible
([docs/architecture.md:509,871](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L509)).
This is post-write coordination, not prevention, and no live Code Mode run was performed here.

**C8 — closed by narrowing the contract, not by adding universal fallback.**
[docs/architecture.md:200](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/architecture.md#L200)
now limits MCP-only registration to a positively identified session;
[internal/binding/binding.go:149](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/binding/binding.go#L149)
implements that condition.
[internal/binding/resolve_test.go:198,233](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/binding/resolve_test.go#L198)
covers single-session fallback versus multiplexed/unidentified clients;
[internal/proc/session_test.go:67](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/proc/session_test.go#L67)
requires positive Codex exec evidence. Codex single-session detection still depends on Linux `/proc`
([internal/proc/proc.go:113](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/proc/proc.go#L113)).
A hooks-absent macOS/Windows session without an independently provided handle still cannot bind
automatically. This mode is no longer promised; it has not become supported merely through
documentation.

## D. Proposed capabilities and their accepted contracts

**D1 — partly. Remote-target semantics are specified, but detection is not implemented.** Accepted
ADR 009 distinguishes client host from execution target, requires mapping evidence, and specifies
unsupported-mode/fail-open/no-claim diagnostics
([docs/adr/009-remote-execution-targets.md:17,22,27](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/adr/009-remote-execution-targets.md#L17)).
[docs/join-hub.md:159](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/join-hub.md#L159)
and
[docs/status.md:120](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/status.md#L120)
disclose remote targets as unsupported. The ADR explicitly builds nothing yet (line 3), and status
says detection needs a real payload capture (line 122); registration still submits one local
host/worktree
([internal/hook/runner.go:388](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/hook/runner.go#L388)).
No inspected detector, mapping, diagnostic or test proves that remote-mode hooks avoid claiming the
local carrier checkout. Real captures and tested unsupported-mode detection remain necessary before
claiming correct classification.

**D2 — partly. Host isolation works; capability granularity and session-level expiry remain
incomplete.** Doctor supplies host/time
([internal/doctor/doctor.go:162](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/doctor.go#L162)),
and storage keeps separate host reports plus `unknown` for old doctors
([internal/store/operator.go:149,181](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/operator.go#L149)).
The report panel marks reports older than 24 hours or invalid dates
([web/src/App.tsx:193](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L193));
tests cover isolation, migration and stale host panels
([internal/store/hostreport_test.go:10,54](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/hostreport_test.go#L10),
[web/src/hostReports.test.tsx:14,58](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/hostReports.test.tsx#L14)).
But ADR 007 requires per-row timestamps and separate measured capabilities
([docs/adr/007-health-per-host.md:21](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/adr/007-health-per-host.md#L21)),
whereas `ClientReport` has one status/detail/last-hook set
([internal/doctor/doctor.go:49](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/doctor/doctor.go#L49)).
`healthOf` drops the report's age
([web/src/App.tsx:189](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/web/src/App.tsx#L189)),
and the session cell renders its old status directly (line 965), so stale “Hook received” can still
appear beside a session. The host-level implementation is real despite ADR 007's stale “planned”
header; the remaining promise is not fully implemented.

**D3 — closed for the accepted retention/restore contract.** ADR 006 sets retention horizons,
deliberate mail preservation, disk-full behavior and restore criteria
([docs/adr/006-retention-and-restore.md:17](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/adr/006-retention-and-restore.md#L17)).
Transactional sweeps exist
([internal/store/retention.go:24](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/retention.go#L24))
and run hourly from the server
([cmd/iris/main.go:156](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/cmd/iris/main.go#L156));
failures are reported (`retention.go:72`). WAL-consistent backup and scratch restore-check are
documented
([docs/deploy.md:67](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/deploy.md#L67))
and implemented
([internal/restorecheck/restorecheck.go:36](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/restorecheck/restorecheck.go#L36)).
Tests cover live-state preservation, retained retry replay, sweep failure, restored recovering
authority/reset behavior, unchanged backup bytes and CLI chaining
([internal/store/retention_test.go:30,79,116,166](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/retention_test.go#L30),
[internal/restorecheck/restorecheck_test.go:42,100](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/restorecheck/restorecheck_test.go#L42),
[cmd/iris/admin_test.go:123](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/cmd/iris/admin_test.go#L123)).
Mail/history remains deliberately unbounded; backups cannot recover later messages. No production
restore rehearsal was performed. The ADR's “planned” header is stale.

**D4 — closed for the accepted encoded-body budget and explicit partial-read contract.** ADR 008
defines 32 KiB shared encoded-body budget, marked cuts, `read` continuation and acknowledgement-only
settlement
([docs/adr/008-model-facing-output.md:18](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/docs/adr/008-model-facing-output.md#L18)).
[internal/budget/budget.go:9,22](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/budget/budget.go#L9)
counts encoded bytes and cuts at UTF-8 boundaries;
[internal/mcpserver/server.go:250](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/server.go#L250)
budgets bodies and puts truncation warnings outside sender data.
[internal/store/messages.go:284,301](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/messages.go#L284)
restricts reads to entitled recipients and returns offset continuation without settlement. Tests:
[internal/mcpserver/budget_test.go:14,46,63](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/mcpserver/budget_test.go#L14),
[internal/store/readbody_test.go:15,67](https://github.com/mateusz-klatt/iris/blob/4840765fbd672dbb82921bb4793795aa1136b9e7/internal/store/readbody_test.go#L15).
This is not a bound for total metadata plus both MCP representations, tokens or every client's
output policy. It does not prove model consumption; A10's post-receipt discard limitation still
applies. The ADR's “planned” header is stale.

## Disposition and first-release implications

**28 closed, 7 partly, 1 open.** Partly: **A4, A7, A9, A10, B6, D1, D2**. Open: **C6**. C8 closes a
misleading promise by narrowing scope; D3/D4 close accepted bounded contracts, not every broader
possible interpretation of the original proposals.

The first issues to resolve before claiming the original P1 guarantees complete are:

1. **A7: protect client configuration.** Replace incomplete TOML conflict scanning with valid input/output handling and define multi-file installation failure behavior. An integration must not make a valid underlying client configuration invalid.
2. **A9/B6: stabilize ambiguous-send retries across the real failure boundary.** Fixed clock buckets do not give ten minutes of retry safety from the first attempt. Add proof using an actual stored send, response loss, client recreation and a bucket-boundary retry; narrow the MCP tool description until that guarantee holds.
3. **A4: resolve the next-turn attention coverage claim.** Prompt and allowed-edit attention are implemented, but unsupported clients/events and native idle Codex behavior need an honest support boundary. The separate [Codex attention measurement](clients/codex-attention.md) supplies client transport evidence; product integration remains separate.
4. **D2/C6: finish or explicitly narrow operator promises.** Stale session-adjacent health can falsely reassure an operator; repository policy switches remain unavailable despite documentation. These are P2 completion items rather than evidence that all prior P1 defects remain.

A10's receipt boundary and D1's unsupported remote-target mode require explicit limitations. If a
release promises reliable client/model consumption or remote-target protection, those become
blockers; the accepted ADRs currently do not establish those guarantees. D1 still lacks its promised
unsupported-mode detection, so a documentation caveat must not be described as runtime enforcement.

This inventory is **not release approval**. It verifies source evidence at one commit and supplies
no fresh full-check run, native cross-platform UAT, concurrent Claude/Codex shared-checkout
measurement, real client attention timing, or production restore rehearsal. Existing tests and
comments describing earlier measurements must not be converted into new measurements at this SHA. No
deployment or fleet assignment was initiated by this review.
