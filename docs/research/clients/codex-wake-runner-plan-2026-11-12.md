# Codex wake runner: W3 revision 3

Status: implementation plan, 2026-11-12. Base: `c4983205cb320a1b4313d114c1b2ab644023e59c`.
Owner: WSL Codex; Holzera reviews; macOS and Windows own native acceptance.
This supersedes revision 2 of `/home/klattm/iris-plans/w3-plan.md` for W3a. W1 and W2 are already on main. No native wake, hosted-model response, cost measurement or final local gate is claimed by this plan.

## Scope and division

W3a adds manual opt-in `iris mcp --client codex --wake`, the event-driven runner and its persistence, shared binding/hook/initialize seams, `CODEX_HOME` pass-through, observations and documentation. Without `--wake` it creates no runner, stream, wake file or runner subprocess. Other clients are unchanged.

W3b adds `iris integrate codex --wake` / `--no-wake`, preservation on reintegration, doctor tier/state diagnostics and user documentation. ADR 017 is not complete before W3b and native acceptance. Default remains off until decision 7's hosted-model, cost and platform evidence exists. No host daemon, Codex database dependency, per-thread scheduler or wake of another session is added.

Planned file boundaries:

- `internal/wake/`: scheduler, hub event adapter, admission, typed wake document, private filesystem writes and platform directory checks, bounded command execution, tests.
- `internal/hook/sessions.go` and tests: a narrow shared sessions-directory API, lock budget, complete told snapshot, wake path/version and fixed observation writing. Existing hooks keep their file format and 50 ms budget; only the hook writes told state or consumes a wake.
- `internal/binding/wake.go` and tests: a positive root target snapshot and serialized final admission/start. No fallback binding is added.
- `internal/mcpserver/server.go` and tests: optional initialization callback captures the client's version; existing constructors/callers remain valid.
- `internal/hubclient/` and tests, only where needed: generation-qualified anonymous event connection using existing URL/token handling, not `watch.Runner`.
- `cmd/iris/mcp.go` and tests: opt-in wiring, cancellation and panic isolation. `internal/integrate/codex.go` and tests add only `CODEX_HOME` pass-through in W3a.
- ADR 017 implementation amendment, API generation wording and the queue research record's exact rejection wording. W3b owns CLI integration flags and doctor presentation.

## Positive target and lifetime

The target is available only after a root id-mode call bound this MCP process: first-call thread ID equals session ID; the session has UUID syntax; binding names a live Execution/generation; no live `IRIS_EXECUTION` override, legacy mode, subagent mode or undecided process is eligible. A lookup-only binding never arms the runner. The target includes original session ID, Execution ID, generation and resolver host. Queue arguments use a canonical UUID, while hub/file lookups retain the actual registered session identity.

The worker checks its target every second. Loss, replacement or generation change cancels its in-flight step/connection and follows the new eligible target. The worker stops with the MCP context; a recovered worker panic records a fixed observation and leaves tools/heartbeat working. Its queued command does not hold the binding mutex while waiting.

Before admission the worker makes a client/host-qualified bind lookup for its exact Execution and requires `hook_seen=true`. It remembers that proof only for that Execution. The events response must contain a known attention identity field; an older hub without it never causes a submission.

## Event consumer and scheduling

Use an authenticated generation-qualified `/v1/watch` connection with no consumer ID. It must neither take `iris-watch`'s monitor slot nor save a retention cursor or claim `monitored`. Each connection/reconnection and reset triggers an authoritative generation-qualified events read with `attention=1`; no notification content is trusted or forwarded. Reconnect uses 1, 2, 4 ... 30 second backoff. `inbox_changed` and `claim_available` are candidates; generic changes, registration, heartbeats and ordinary claim releases are not.

Candidate due time is `max(trigger time + 10 seconds, required due time)`. A single scheduler runs due candidates together; later events do not postpone the earliest candidate. Additional candidates are a known pre-submission retry, the ten-minute second-note deadline and a formerly outstanding attempt becoming consumed. Outstanding state is checked under the file lock every 15 seconds, without hub reads while it still blocks.

The authoritative read has a five-second deadline. `identity=null` means no pending attention and submits nothing. Other-stream/newer-cursor/high-water regression, older-cursor refusal and the two announcement slots follow ADR 017. An outstanding attempt blocks even across stream, binding, generation and process changes. Waking does not fetch/ack mail, mark it handled or advance told state.

## The admission transaction

1. Under `sessions/.lock`, snapshot the told file's presence and **whole bytes**, plus the wake file's presence and whole bytes. Read failures and unusable wake state block admission. An outstanding attempt ends the step before any hub request.
2. Ask the hub outside every file lock. Recheck the positive target. Under the file lock compare both complete snapshots with step 1 before using the answer. Presence changes, prune/recreate ABA, `twice` changes and byte changes invalidate it even if a revision repeats. Retry after ten seconds without changing quota.
3. Select the allowed slot from current pending attention and told state. Slot 0 is for new untold identity. Slot 1 requires the same pending/told identity, ten minutes elapsed and no second telling. A consumed/submitted/stored/unknown/unanswered attempt spends its slot. Two known pre-submission failures also spend it; one permits one retry after 60 seconds. Unconsumed aborted attempts spend none.
4. Check the absolute Codex binary outside the file lock. Create a unique random 128-bit reservation ID for **every** attempt, including a binary failure. A queue attempt also has its exact fixed prompt and correlation token; these are not mail data or authentication credentials.
5. Outside the lock, protect the directories and prepare, protect, write, sync and close a temporary document. Under the lock recheck target-independent snapshots again, install by rename, read back the exact new attempt by ID and close its handle. Release the file lock before directory sync and before resolver admission.
6. Atomically compare the final positive target and call `cmd.Start` under the resolver mutex. Never acquire this mutex while holding `sessions/.lock`. A replaced/dropped target prevents start. Release the resolver mutex before `Wait`, output handling or file writes.
7. Merge the command outcome under the file lock by **reservation ID**, never token or array position. Preserve a hook's `consumed_ms` and any newer attempt. Retry a failed answer write at most twice more, 100 ms apart; an unanswered queue attempt remains outstanding/unknown.

If a published reservation's command certainly never started, abort that exact ID. Failed abort persistence has two explicit outcomes: an unanswered queue reservation remains unknown/outstanding and blocks; an already answered binary-failure reservation remains a counted pre-submission failure with its existing bounded retry. An observation never grants an allowance. A consumed record stays quota-bearing even if a late answer says aborted.

## Wake document and bounded state

Keep the version-1 JSON envelope that W2 preserves. Add a mandatory unique `id` to each new reservation, including binary failures. The hook preserves it and still consumes only the newest attempt whose whole message equals its prompt. All attempts in a usable document share one nonempty stream and positive cursor, IDs are unique, slots are 0/1, required times are positive, and at most one outstanding attempt exists, as the newest. Consumed records compacted by W2 remain spent; compaction preserves the ID. A malformed, unreadable, unsupported-version or semantically invalid file is never replaced with fresh allowance.

While preparing a new attempt, discard superseded **unconsumed** aborted records. Keep all quota-bearing records and the current mark. The newest aborted record remains until its replacement is published, so an all-aborted document does not lose its mark. Each slot has at most two quota-bearing tries; one current aborted record makes the published document bounded by five records. Unknown enum answers are conservative unknown; a missing token/message is valid only for a known pre-submission failure or already consumed compacted record, not an outstanding queue attempt.

The command's answer is not delivery proof. `submitted` means the measured acceptance line, `stored` includes the exact measured resume warning, and known pre-submission failure is restricted to failure to start or the exact same-thread measured no-rollout rejection. Signals, timeout, output overflow, unexpected text/status and uncertain writes are `unknown`; none retries as though no queue write occurred. Parser tests use complete recorded output, including the full rejection line, and never broaden a match to a prefix.

## Private filesystem and process boundary

Every writer validates/protects `sessions/wake` outside the shared file lock. Unix mode must equal `0700`, including existing `0500`, `0000` or restrictive-umask creation. Validate ownership and normalize through existing owner protection. Windows checks a non-null protected DACL's actual ACEs: current-user allow, required access and directory inheritance, no other grants. A protected Everyone/wrong-user/null/wrong-inheritance ACL is not sufficient. Apply `OwnerOnly` outside the lock when correction is needed, then verify it. Do not reapply an already correct inheritable directory DACL needlessly.

Resolve Codex once on PATH to an absolute executable; reject relative-dot resolution. Before each submission run `--version` with a five-second deadline, one-second WaitDelay and 4 KiB cap, and require exact initialize-version agreement. Queue execution has a 30-second deadline, one-second WaitDelay and independent 64 KiB stdout/stderr caps. Start without a shell; inherit the MCP working directory/environment, including passed `CODEX_HOME`. Neither branch-specific code nor message text chooses executable, argv or working directory.

Only fixed bounded diagnostic reasons, expected version/path metadata and outcome fields are persisted. Unexpected raw process output is not copied into observations or wake-file error details, to avoid persisting credential-bearing output. Queue stdout never reaches the MCP protocol stdout.

Reservation publication/read-back precedes `cmd.Start`; Unix syncs the wake directory before start. This proves cooperative process/shim restart accounting. First-parent directory publication, answer/consumption sync and Windows directory sync do not establish whole-host power-loss durability; document that reservation loss can permit another wake after such a crash. Do not claim the quota survives that case.

## Observability and safe recovery

W3a emits fixed observations for eligible root/stream following, refused/unusable state, known command failure, unknown result and worker panic, without mail text, auth material or raw process output. Native acceptance uses an isolated single-root setup plus these observations and the exact generation-qualified anonymous connection to establish arming without fetching mail. The wake file supplies attempt ID, full queued prompt, outcome and consumption. W3b distinguishes configured from armed/delivered and presents old outstanding state and recovery; existing `doctor --live` exec probes are not an idle-wake PASS.

An outstanding wake has no expiry that frees its guard. Resume a still-live session so its queued prompt hook can consume it. Remove the exact wake file only after permanently closing that session and stopping its MCP child. Removing a live guard can admit duplicate queue entries. A model may ignore the note; a cancelled/lost hook note can remain unannounced after the two bounded opportunities. Session unload/daemon stop/reboot ends the listener; closed-session wake is outside this scope.

## Review findings and behavioral tests

All eight revision-2 findings remain acceptance requirements:

1. Hold an old hub response while pruning/recreating told state at the same revision; it cannot overwrite newer telling or reserve a wake. Also change `twice` without revision and require progress once writers stop.
2. Create two binary failures F1/F2, then fail read-back/directory sync and abort persistence; only F2's ID is addressed and the defined counted-failure/unknown fallback holds.
3. Windows permissive protected, wrong-user, null and incorrect-inheritance DACLs are corrected or refused before a wake file is published.
4. Replace/drop binding between the last snapshot and final admission; process start serializes with that transition. Waiting for a started process does not block heartbeat/binding.
5. Many aborted attempts and shim restarts keep state bounded, keep spent slots, preserve mark and eventually progress.
6. Unix `0500`, `0000` and restrictive umask still produce exactly private searchable `0700` directories before publication.
7. Mixed identities, duplicate IDs, multiple/non-newest outstanding attempts are rejected unchanged; consumed compacted and late-aborted records retain quota.
8. Recovery guidance never instructs removal of a live guard; prompt consumption precedes network/repository failure and releases only the exact full prompt.

Additional tests cover every excluded arming mode and disabled path; no hook evidence/unknown hub identity/null pending; all trigger/settle/backoff/retry deadlines; two-process admission; stream/generation replacement; retained outstanding state after restart/reset; consumed-before-command-return races; filesystem failure boundaries; binary/version/output/deadline classification; environment/argv/cwd; an anonymous real-hub connection with no monitor/consumer cursor; and panic isolation. Each test asserts behavior, with 100% meaningful Go coverage on Linux/macOS/Windows. Captured client payloads are never edited to fit a test.

## Validation and publication

Publish signed Conventional Commits and a draft PR with exact candidate SHA and truthful pending gates. Before ready, rebase on then-current main and run the full local `make test`, `race`, `cov`, `cov-check`, `check-all`, `fixtures` matrix; web checks only for console changes. Platform owners run bounded native checks at the published SHA; do not duplicate their runs.

Actual idle acceptance is separate from unit/CLI acceptance: isolated hub/token/repo/home and normal trusted hooks; one root Iris tool call to arm; finished Stop/idle checkpoint; a real incoming message; stream and reservation; same UUID queued UserPromptSubmit with exact prompt; same rollout turn's inbox result containing that delivery ID; consumed mark; no extra wake after settlement. Preserve all links and timestamps. Repeat with explicit custom Codex home. Busy-next-turn and timer enqueue are different measurements. Default stays off while hosted-model action, short/long-thread costs or required platforms remain unmeasured.

References: [accepted ADR 017](../../adr/017-wake-codex-by-queue.md), [existing MCP queue record](codex-mcp-queue-2026-11-10.md), [official queue contract](https://learn.chatgpt.com/docs/cli/reference).

