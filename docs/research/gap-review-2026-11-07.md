# Iris gap review — 2026-11-07

Produced by Codex (gpt-6-astra, ultra effort, read-only sandbox) at claude-holzera-iris's request,
relayed by claude-home-iris. The working directory was this repository, so `AGENTS.md` applied,
and the prompt restated the clean-room rule: no excluded project was opened, searched or recalled.
Findings are evidence from reading the code at the commit below, not reproduced failures. Each one
is a candidate for its own issue and PR.

**Status since the review:** C5 (a release woke every agent) is fixed by #105, merged the same day.

## Method

Reviewed commit `e3d07d2f7c3533fed9ac3023e9a020214a41c9fb` against the requirements, numbered architecture decisions, section 5 invariants, ADRs, API and guard documentation, status document, and per-client research notes. Inspection covered the requested Go packages, CLI entry points, React console, and relevant tests. No files were changed or created; tests were inspected but not executed because they create files.

Public documentation checked included [Claude Code](https://code.claude.com/docs/en/hooks), [Codex](https://learn.chatgpt.com/docs/hooks), [Copilot CLI](https://docs.github.com/en/copilot/reference/hooks-reference), [Cursor](https://cursor.com/docs/hooks), [Antigravity](https://antigravity.google/docs/hooks), [Grok](https://docs.x.ai/build/features/hooks), [Kimi Code](https://moonshotai.github.io/kimi-code/en/customization/hooks), and [MCP](https://modelcontextprotocol.io/specification/2025-11-25/server/tools). The clean-room restrictions were observed throughout, including delegated review.

Findings exclude the status document’s acknowledged release and measurement gaps. **P1** means a core guarantee can fail or integration can damage client operation; **P2** means a material functional or operational gap. Confidence describes the evidence, not whether a failure was reproduced during this review. The failure sequences below are established by code inspection.

## A. Unmet or partly met requirements

### A1. Explicit claims do not protect automatically ignored paths — P1 · High confidence

**Evidence:** R5 requires pre-edit collision detection. D10 explicitly preserves explicit claims on ignored paths at [docs/architecture.md:304](../../docs/architecture.md:304). However, [internal/store/claims.go:202](../../internal/store/claims.go:202) removes ignored paths before the conflict loop at line 214.

**Why it matters:** Agent A can explicitly claim `go.sum`, a changelog, or generated output, yet agent B’s ordinary editing hook receives an allow. A repository-wide explicit prefix claim is also bypassed for these paths.

**Next step:** Check existing claims against every requested path. Apply ignore rules only when deciding whether to create an automatic claim. Test exact and prefix claims against ignored-path hook touches.

### A2. A resumed session can inherit another session’s mailbox — P1 · High confidence

**Evidence:** R4 requires messages to survive recipient restarts. [internal/store/sessions.go:277](../../internal/store/sessions.go:277) computes a new generation for the original session, but lines 282–314 independently select the first free agent slot. Inbox retrieval filters by that selected agent at [internal/store/messages.go:87](../../internal/store/messages.go:87).

**Why it matters:** Sessions A and B occupy slots 1 and 2. Mail arrives for B. Both sessions end, then B resumes first with its original session ID. B takes slot 1, leaving its mail in slot 2 and inheriting A’s mailbox. The records survive, but recipient continuity does not.

**Next step:** Preserve the prior durable mailbox across session generations. Define what happens when that mailbox is already occupied, and test restart order changes across concurrent slots.

### A3. Claim metadata can inject forged guidance into hook context — P1 · High confidence

**Evidence:** R10 applies to agent-written text reaching another model. [internal/store/claims.go:67](../../internal/store/claims.go:67) permits carriage returns and newlines in claim paths. [internal/hook/context.go:69](../../internal/hook/context.go:69) inserts the path verbatim into the SessionStart narrative. Its warning labels message bodies untrusted, without delimiting this metadata.

**Why it matters:** A claimed relative path containing a newline and imperative text becomes a standalone line inside Iris-generated context. Safe sender names and the MCP inbox’s `untrusted_data` wrapper do not cover this path. This is a prompt-context integrity failure, not an assertion that Iris can isolate processes sharing an OS user.

**Next step:** Quote or escape displayed metadata and delimit it as data. Add adversarial context tests covering newline-bearing paths and other externally supplied display fields.

### A4. The promised next-turn attention fallback is absent — P1 · High confidence

**Evidence:** R6 and [D7](../../docs/architecture.md:201) promise attention through prompt or post-tool context. [internal/hook/runner.go:259](../../internal/hook/runner.go:259) permits network activity only for session lifecycle events and pre-edit tools. Ordinary prompt and post-tool hooks return without checking messages. Session context is generated at startup, at line 159.

**Why it matters:** Mail arriving after startup remains invisible unless the model polls MCP or an external monitor works. This removes the advertised fallback for clients without idle-session monitors and for Claude’s headless mode. Public [Kimi](https://moonshotai.github.io/kimi-code/en/customization/hooks) and [Claude](https://code.claude.com/docs/en/hooks) documentation explicitly provides prompt-time context mechanisms.

**Next step:** Implement bounded attention checks on each client’s supported events. Test mail arriving after startup and surfacing at the next eligible event without acknowledging it.

### A5. Watch cursors can advance past notifications that were never emitted — P1 · High confidence

**Evidence:** Two failure boundaries violate R6 and D7:

- [internal/watch/watch.go:56](../../internal/watch/watch.go:56) can queue an event during the coalescing window without printing it. Nevertheless, [line 327](../../internal/watch/watch.go:327) persists its cursor immediately.
- [internal/store/api.go:127](../../internal/store/api.go:127) initializes a monitor cursor before its pending-mail notification is delivered. [internal/hub/hub.go:382](../../internal/hub/hub.go:382) does this before socket reservation, upgrade, and the initial reset write.

Once a cursor exists, [internal/store/api.go:120](../../internal/store/api.go:120) returns it with `pending: 0`.

**Why it matters:** A watch process dying during coalescing loses queued attention. A failed first connection or reset write can similarly consume the only wake for preexisting mail. Restarting the monitor skips those events even though nobody received the notification.

**Next step:** Checkpoint emitted notifications rather than merely received frames, or persist the pending coalescing state. Make initial attention recoverable until its delivery is confirmed.

### A6. Reintegrating can delete unrelated user hooks — P1 · High confidence

**Evidence:** R3 requires preservation of client configuration. [internal/integrate/jsonfile.go:42](../../internal/integrate/jsonfile.go:42) constructs a group containing only Iris; lines 53–56 replace an entire existing group whenever any nested handler matches Iris. Claude, Codex, and Grok use this helper.

**Why it matters:** A user-added handler beside Iris in the same group disappears on the next integration or repair. Group properties also disappear. This can silently remove approval checks, formatting, or other automation.

**Next step:** Replace only Iris’s handler entries, preserving siblings and appropriate group properties. The mixed-group fixture already present in [internal/integrate/remove_test.go:129](../../internal/integrate/remove_test.go:129) should also be exercised during reintegration.

### A7. Integration can turn valid client TOML into invalid configuration — P1 · High confidence

**Evidence:** D14 promises validation before saving. [internal/integrate/toml.go:18](../../internal/integrate/toml.go:18) checks NUL bytes and marker pairing, but does not parse the document. [internal/integrate/codex.go:78](../../internal/integrate/codex.go:78) appends an Iris table through this mechanism; Grok uses the same approach.

**Why it matters:** A valid manually configured, unmarked `[mcp_servers.iris]` table becomes a duplicate table. The resulting configuration is invalid and can prevent the client from starting, directly undermining R3 and R8.

**Next step:** Validate the complete input and output TOML. Merge supported existing entries or refuse without writing. Test unmarked existing Iris tables, malformed TOML, and interrupted multi-file installation.

### A8. Repository identity can merge distinct repositories — P1 · High confidence

**Evidence:** D11 and section 5 prohibit ambiguous automatic joins. Two paths violate that rule:

- [internal/store/sessions.go:125](../../internal/store/sessions.go:125) discards URL ports through `Hostname()`. Different services at the same hostname and repository path receive the same alias.
- When root identity becomes available later, [line 160](../../internal/store/sessions.go:160) adds a root alias without updating `Repository.root_commit`. Subsequent contradiction checks at line 187 still inspect the empty column.

**Why it matters:** Distinct endpoints with equal or unavailable root evidence can merge. A repository initially registered without a root can also accept a contradictory root after learning its actual identity. Messages and claims then cross repository boundaries incorrectly.

**Next step:** Preserve endpoint distinctions that cannot safely be normalized. Persist newly learned root evidence and consult it during later joins. Test nondefault ports and “unknown root → root A learned → root B presented.”

### A9. Retry identity is not preserved across user-facing failure boundaries — P1 · High confidence

**Evidence:** The MCP shim generates an omitted retry key at [internal/mcpserver/server.go:176](../../internal/mcpserver/server.go:176) and reveals it only in the eventual response at line 196. The console generates a fresh key on every submission at [web/src/App.tsx:270](../../web/src/App.tsx:270), including retries; it also lacks an in-flight submission guard.

**Why it matters:** If a send commits but its MCP response is lost, the caller never learns the generated key. Retrying creates another message. The console has the same problem after an ambiguous network failure, and repeated clicks can create separate sends. R4’s deduplication holds only when a stable key actually reaches both attempts. MCP explicitly allows completion to race with cancellation and late responses to be ignored. [MCP cancellation specification](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation)

**Next step:** Establish and retain the operation key before the side effect. Require it for reliably retryable MCP mutations, retain it with console drafts, and prevent concurrent submission of the same operation.

### A10. Fetching mail suppresses attention before successful receipt or handling — P1 · High confidence

**Evidence:** [internal/store/messages.go:103](../../internal/store/messages.go:103) commits increased delivery attempts before the HTTP response is consumed. [internal/store/api.go:104](../../internal/store/api.go:104) considers only `attempt=0` pending attention. Expiry clears the execution assignment but preserves the attempt at [internal/store/store.go:283](../../internal/store/store.go:283).

**Why it matters:** A lost inbox response, or a recipient crash before handling the fetched mail, leaves an unacknowledged delivery available for retrieval but no longer eligible for attention. The record is durable; the recipient can remain unaware of it indefinitely.

**Next step:** Separate delivery attempts, active processing, and settled handling. Requeue attention after failed delivery or recipient loss while retaining delivery IDs and stale-ack protection.

### A11. Message size limits disagree across the store, MCP client, and console — P2 · High confidence

**Evidence:** The store accepts 16 KiB bodies and inbox pages of 100 messages at [internal/store/messages.go:20](../../internal/store/messages.go:20) and line 87. [internal/hubclient/client.go:175](../../internal/hubclient/client.go:175) truncates every response at 1 MiB before JSON decoding. The console validates JavaScript string length rather than UTF-8 bytes at [web/src/App.tsx:639](../../web/src/App.tsx:639).

**Why it matters:** A valid page of 64 full-sized bodies already exceeds the client limit before JSON overhead. Retrieval fails after delivery attempts have changed, compounding A10. Separately, non-ASCII drafts can pass console validation and then fail the server’s byte limit with a generic error.

**Next step:** Define consistent byte limits, return byte-bounded pages with continuation, detect overflow explicitly, and validate console drafts using encoded size.

### A12. Senders cannot observe the handling acknowledgement required by R4 — P2 · High confidence

**Evidence:** R4 requires the sender to distinguish delivered from acted upon. [docs/api.md:123](../../docs/api.md:123) instead defines acknowledgement as acceptance, not action. The MCP tool says the same at [internal/mcpserver/server.go:67](../../internal/mcpserver/server.go:67). Recipient states are exposed through the operator thread view at [internal/store/operator.go:242](../../internal/store/operator.go:242), without a corresponding sender-facing MCP status or receipt mechanism.

**Why it matters:** An agent coordinating dependent work cannot determine whether its request was received or handled through Iris’s agent interface. A human inspecting the console is not equivalent to the sender receiving that information.

**Next step:** Resolve the acceptance-versus-handling semantics explicitly. Provide sender-visible status or receipt events, with a separate handled state if acknowledgement continues to mean acceptance.

### A13. Human unread messages cannot be cleared — P2 · High confidence

**Evidence:** Human unread counts exclude only acknowledged deliveries at [internal/store/operator.go:158](../../internal/store/operator.go:158). Thread reads are read-only at line 219. [internal/hub/operator.go:170](../../internal/hub/operator.go:170) exposes no operator acknowledgement route. Normal acknowledgement requires a live execution, whereas the human execution is permanently ended at [internal/store/operator.go:34](../../internal/store/operator.go:34).

**Why it matters:** Reading or replying does not clear human unread counts. The oversight interface accumulates stale attention indicators and cannot distinguish outstanding requests from handled ones.

**Next step:** Add an explicit operator read/acknowledgement operation with defined semantics, and test unread transitions after reading, acknowledging, and replying.

### A14. Live refreshes discard reply drafts and can default replies back to the human — P2 · High confidence

**Evidence:** Every watch frame increments `threadRevision` at [web/src/App.tsx:163](../../web/src/App.tsx:163). The thread effect depends on that revision and unconditionally replaces the reply draft with `body: ""` at [line 221](../../web/src/App.tsx:221). It also selects the last message’s sender as recipient.

**Why it matters:** An unrelated claim or session event can erase a reply while the operator is typing. When the latest message was sent by `human`, the default recipient becomes `human`, so a follow-up can be addressed back to the operator.

**Next step:** Initialize drafts when opening a thread, preserve dirty drafts during refresh, and derive the intended counterpart independently of the last sender. Test incoming events during composition and follow-ups after a human message.

### A15. Message history becomes inaccessible through pagination gaps — P2 · High confidence

**Evidence:** The console always requests 50 threads at [web/src/App.tsx:181](../../web/src/App.tsx:181) and provides no continuation control. The server’s `before` filter at [internal/store/operator.go:177](../../internal/store/operator.go:177) compares timestamps alone, although ordering also includes thread ID.

**Why it matters:** Older threads disappear from ordinary browsing. Even API consumers that paginate skip threads sharing the boundary item’s millisecond timestamp. Search is not a complete replacement for browsing unknown historical work.

**Next step:** Implement console continuation and a composite `(last_at, thread_id)` cursor. Test more than one page with identical timestamps.

### A16. A claimless dead session can remain displayed as active — P2 · High confidence

**Evidence:** [internal/store/store.go:249](../../internal/store/store.go:249) changes expired executions to `lost`, but only expired claims generate events in the following loop. [internal/hub/operator.go:150](../../internal/hub/operator.go:150) sends UI updates only when the event high-water changes.

**Why it matters:** A session with no claims can die without triggering a console refresh. Its active status persists until unrelated traffic changes the event log, weakening R6’s human signal and R7’s live oversight.

**Next step:** Emit events for externally visible execution-state transitions. Test expiry with no claims and no other activity.

### A17. Shell reconciliation bypasses recovery proof and hides its failures — P2 · High confidence

**Evidence:** Normal hooks use generation-aware registration at [internal/hook/runner.go:236](../../internal/hook/runner.go:236). Shell reconciliation instead registers directly at [internal/hook/reconcile.go:46](../../internal/hook/reconcile.go:46). Its error, panic, and timeout paths return allow without recording a hook failure.

**Why it matters:** During hub recovery, reconciliation can fail despite valid saved session proof. Hub outages, revoked tokens, Git failures, and deadlines can also disable it without the diagnostics promised by R8 and D3. This is separate from the accurately documented limitations of porcelain-status comparison.

**Next step:** Share the normal registration and diagnostic paths while preserving fail-open behavior. Test shell and Code Mode reconciliation across restart, token rejection, panic, and timeout.

## B. Decisions and invariants without an adequate failing test

These are gaps in behavioral proof, not claims that the corresponding areas have no tests. Several directly explain why the defects above survive substantial coverage.

### B1. Concurrent multi-path atomicity is not exercised — P1 · High confidence

**Evidence:** Section 5 requires concurrent overlapping grants and atomic multi-path grants. [internal/store/store_test.go:90](../../internal/store/store_test.go:90) races 50 single-path explicit claims. The multi-path and rename checks beginning at line 123 are sequential.

**Why it matters:** Separate concurrency and atomicity tests do not prove their interaction.

**Next step:** Race overlapping multi-path touches, including prefix, case-folded, and rename boundaries; assert one winner and no partial grants.

### B2. Reconnect tests do not prove notification delivery across process loss — P1 · High confidence

**Evidence:** [internal/watch/reconnect_test.go:41](../../internal/watch/reconnect_test.go:41) reconnects one Runner, preserving its in-memory state. [internal/hub/stream_failure_test.go:87](../../internal/hub/stream_failure_test.go:87) injects initial-write failures but checks resource release, not subsequent delivery.

**Why it matters:** D7 and the section 5 reconnect guarantee remain unproved at the boundaries responsible for A5.

**Next step:** Replace the Runner after a persisted-but-held event, and reconnect after failed initial reset writes. Require eventual notification without depending on unrelated traffic.

### B3. Durable mailbox continuity is not tested across slot churn — P1 · High confidence

**Evidence:** [internal/store/store_test.go:537](../../internal/store/store_test.go:537) checks concurrent slots and parent relationships. It does not resume those sessions in a different order after they end.

**Why it matters:** D9’s durable mailbox model can break while slot allocation and delivery persistence tests independently pass.

**Next step:** Send distinct mail to concurrent slots, end them, resume in every relevant order, and assert mailbox identity and message ownership remain stable.

### B4. Explicit ignored-path protection is tested against the wrong operation — P1 · High confidence

**Evidence:** [internal/store/store_test.go:171](../../internal/store/store_test.go:171) tests an explicit claim against another explicit claim on `go.sum`. It never tests another execution’s hook `touch`.

**Why it matters:** The test does not exercise D10’s promise on the actual pre-edit path, allowing A1.

**Next step:** Assert deny behavior through hook touches for exact, prefix, and repository-wide explicit claims covering ignored paths.

### B5. Repository identity tests omit evolving evidence and nondefault endpoints — P1 · High confidence

**Evidence:** [internal/store/remote_alias_test.go:5](../../internal/store/remote_alias_test.go:5) covers equivalent spellings and default SSH port normalization. Its later tests cover gaining a remote, without subsequently contradictory root evidence.

**Why it matters:** Section 5’s “ambiguous URL cannot join automatically” invariant is not protected against A8.

**Next step:** Add distinct-port cases and registration sequences where root identity is learned after initial creation.

### B6. Retry tests assume the caller receives the generated key — P1 · High confidence

**Evidence:** [internal/mcpserver/server_test.go:213](../../internal/mcpserver/server_test.go:213) verifies returned generated keys; line 249 verifies keys in received error responses. Neither loses the MCP response after commit. Console send tests at [web/src/App.test.tsx:158](../../web/src/App.test.tsx:158) exercise successful submissions.

**Why it matters:** D9 and section 5’s retry guarantee are tested inside a narrower boundary than users experience.

**Next step:** Commit a send, discard the response, recreate the client, and retry. Assert one logical message. Add the equivalent console failure-and-retry test.

### B7. The live-doctor test passes without any MCP call — P2 · High confidence

**Evidence:** The fake client at [internal/doctor/live_test.go:87](../../internal/doctor/live_test.go:87) registers, touches, and prints its execution ID without invoking MCP. Nevertheless, [line 124](../../internal/doctor/live_test.go:124) requires the result to report that MCP tools answered.

**Why it matters:** The test’s success condition cannot establish D14’s claimed end-to-end MCP proof.

**Next step:** Require evidence unique to an actual MCP operation, and add a negative case that echoes the hook-provided ID while MCP is unavailable.

## C. Documentation promises the implementation does not keep

### C1. Redirects bypass the promise that bearer tokens never travel in cleartext — P1 · High confidence

**Evidence:** D1 prohibits non-loopback cleartext credentials. [internal/hubclient/client.go:146](../../internal/hubclient/client.go:146) validates only the initial URL, then attaches the token and calls an ordinary HTTP client at line 170. The hook client at [internal/hubclient/hedge.go:24](../../internal/hubclient/hedge.go:24) also has no redirect policy.

**Why it matters:** The HTTP client follows redirects and preserves authorization for the same hostname. An HTTPS response redirecting to HTTP on that hostname can transmit the bearer token in cleartext despite initial URL validation. This follows from the inspected client behavior; no live credential transmission was attempted.

**Next step:** Reject redirects for authenticated hub operations, or validate every destination and prohibit credential-bearing downgrades and unintended authority changes. Add redirect regression tests.

### C2. `doctor --live` can report MCP success from hook output alone — P2 · High confidence

**Evidence:** [internal/doctor/live.go:208](../../internal/doctor/live.go:208) marks `ToolsAnswered` when client output contains a registered execution ID. [internal/hook/context.go:19](../../internal/hook/context.go:19) already supplies that ID in SessionStart context. [docs/architecture.md:460](../../docs/architecture.md:460) incorrectly describes it as obtainable only through `whoami`.

**Why it matters:** A model can echo the handle, or verbose client output can contain it, while MCP is missing or disconnected. The diagnostic then provides false assurance.

**Next step:** Use a challenge or observation available only through the tested MCP path. Correct the documentation and the test described in B7.

### C3. Offline doctor can report “ok” for a partial or ineffective integration — P2 · High confidence

**Evidence:** [internal/doctor/doctor.go:222](../../internal/doctor/doctor.go:222) obtains one canary command and runs it outside a Git repository. [Line 311](../../internal/doctor/doctor.go:311) accepts exit zero and valid JSON, without proving interception. For example, [internal/integrate/cursor.go:64](../../internal/integrate/cursor.go:64) locates a session-start hook without validating the complete pre-edit and MCP configuration.

**Why it matters:** A working session-start command alongside a missing edit hook or MCP configuration can appear healthy. A guarded command emitting fallback JSON can also pass without running Iris.

**Next step:** Distinguish configuration completeness, command execution, client loading, and measured interception. Validate all required entries and report unproved runtime behavior explicitly.

### C4. Promised hook diagnostics and console doctor execution are missing — P2 · High confidence

**Evidence:** D3, D5, D14, and D16 promise drift reporting, cross-load reporting, last-hook evidence, and a button to run doctor. In practice:

- Unknown tools only reach stderr at [internal/hook/runner.go:286](../../internal/hook/runner.go:286).
- Cross-loads return allow without a durable observation.
- `LastHookAt` is declared but never populated at [internal/doctor/doctor.go:44](../../internal/doctor/doctor.go:44).
- The console button at [web/src/App.tsx:363](../../web/src/App.tsx:363) only refreshes stored data.
- The sensitive-repository warning promised by [ADR 003:45](../../docs/adr/003-path-identity.md:45) has no doctor implementation.

**Why it matters:** The diagnostic surface cannot explain several advertised forms of protection loss, and refreshing an old report cannot establish current health.

**Next step:** Persist bounded, host-attributed observations with timestamps and expose them in doctor. Implement the promised checks or explicitly narrow the documentation, including the console button’s role.

### C5. Claim releases wake every agent, rather than relevant waiters — P2 · High confidence

**Evidence:** [docs/architecture.md:209](../../docs/architecture.md:209) limits attention to mail and claims someone is waiting for. [internal/store/store.go:293](../../internal/store/store.go:293) instead broadcasts repository events to every agent. [internal/watch/watch.go:95](../../internal/watch/watch.go:95) turns every release into a notification. There is no waiter tracking.

**Why it matters:** Routine releases and expiry can wake unrelated sessions, including the releasing agent. At the specified scale, one cleanup can trigger many unnecessary model turns. Claude’s public documentation confirms that monitor output reaches the model as notifications. [Claude plugin monitors](https://code.claude.com/docs/en/plugins/components#monitors)

**Next step:** Track interest established by denied conflicts and notify only relevant live waiters. Test that unrelated agents and the releasing execution remain silent.

### C6. Documented repository policy controls are unavailable — P2 · High confidence

**Evidence:** [docs/architecture.md:109](../../docs/architecture.md:109) promises a fail-closed opt-in; [line 304](../../docs/architecture.md:304) describes `claim.ignore` and configurable conflict actions. Ignore rules are hardcoded at [internal/store/claims.go:88](../../internal/store/claims.go:88). `SetRepositoryPolicy` has only test callers, and the hook has no persisted fail-closed policy.

**Why it matters:** Users cannot select the documented outage behavior, customize automatic-claim exclusions, or configure conflict handling through a supported interface.

**Next step:** Define and expose validated repository configuration, respecting client capability limits, or mark these controls as deferred. This finding does not challenge ADR 003’s deliberate choice to keep the case-policy opt-out internal.

### C7. Code Mode reconciliation does not create the automatic claims promised by the architecture — P2 · High confidence

**Evidence:** The client capability discussion says detected Codex JavaScript writes are automatically claimed. [internal/store/reconcile.go:63](../../internal/store/reconcile.go:63) only checks existing conflicts and returns a warning; it creates no claim.

**Why it matters:** A detected write to an unclaimed file leaves it unclaimed, so another agent can immediately obtain permission to edit it. The documented post-write ownership behavior does not exist.

**Next step:** Implement bounded post-write claiming for identifiable paths or correct the promise. Preserve the existing, accurate warnings about shared worktrees and unchanged porcelain status in [docs/guard.md:37](../../docs/guard.md:37).

### C8. MCP-only fallback does not universally preserve messaging when hooks never ran — P2 · High confidence

**Evidence:** [docs/architecture.md:181](../../docs/architecture.md:181) promises MCP-only registration when hooks are absent. [internal/binding/binding.go:107](../../internal/binding/binding.go:107) requires a positively recognized single-session process for one fallback path. [internal/proc/proc.go:113](../../internal/proc/proc.go:113) explicitly cannot recognize Codex that way on macOS or Windows. Codex also lacks the session environment-variable rung.

**Why it matters:** Without hooks, an explicit handle, or launcher-provided identity, those sessions receive `unbound` and cannot use the promised messaging fallback. The suggested hook-provided handle is unavailable precisely in this case.

**Next step:** Narrow the guarantee and document a usable fallback, or add a positively identified registration mechanism that can later reconcile with hook identity.

## D. Capabilities missing from the specification

These are proposed requirements, not claims that the implementation violates an existing detailed contract.

### D1. Distinguish the client machine from the remote execution target — P2 · Medium confidence

**Evidence:** R2 covers multiple machines without defining remote execution-target identity. Iris registers the local hostname and locally discovered repository at [internal/hook/runner.go:356](../../internal/hook/runner.go:356). Kimi explicitly documents that hooks run on the local client machine and inspect the local carrier directory even when tools operate in a remote environment. [Kimi hooks](https://moonshotai.github.io/kimi-code/en/customization/hooks)

**Why it matters:** Coordination must identify the files actually being changed. Local hook location is insufficient evidence of the target repository or filesystem.

**Next step:** Specify separate client-host and execution-target identities, path mapping, and evidence required to claim protection. Where mapping is unavailable, require an explicit unsupported-mode diagnostic. Capture real remote-target payloads before classifying a particular session as protected.

### D2. Define host-scoped, expiring capability and health information — P2 · High confidence

**Evidence:** The target deployment spans approximately five machines at [docs/spec/requirements.md:19](../../docs/spec/requirements.md:19). Yet [internal/store/operator.go:60](../../internal/store/operator.go:60) reads one global doctor report, and line 81 overwrites that singleton. The report rows have no host identity. Public client documentation distinguishes configuration scopes, enabled plugins, and runtime availability; for example, [Claude MCP management](https://code.claude.com/docs/en/mcp).

**Why it matters:** A successful doctor run on one laptop can replace evidence from another machine. “Claude is healthy” is insufficient when hooks, MCP, and monitors differ by host and session.

**Next step:** Require health records keyed by host and client installation, with observation time, freshness, version, and separately measured capabilities. Define how sessions reference that evidence and how stale or unreachable installations appear to the operator.

### D3. Define retention and restore guarantees for a continuously running hub — P2 · High confidence

**Evidence:** Normal session and touch operations append events at [internal/store/api.go:93](../../internal/store/api.go:93). Expired claims remain stored, and idempotency records accumulate. `PruneEvents` at [line 207](../../internal/store/api.go:207) has only test callers. [docs/deploy.md:29](../../docs/deploy.md:29) gives useful backup guidance, but the requirements define no retention horizons or restore acceptance criteria.

**Why it matters:** Continuous coordination produces continuous durable state. Unbounded growth eventually affects availability, while unsafe cleanup can invalidate retry guarantees. Restoring an old database can also resurrect obsolete delivery or ownership state unless recovery behavior is defined.

**Next step:** Specify retry-safe retention horizons, bounded event history, archival/deletion policy, disk-pressure behavior, and a tested restore procedure that invalidates stale execution authority. Retain the existing guidance against copying a live database without its WAL state.

### D4. Define aggregate model-output budgets and partial-consumption semantics — P2 · High confidence

**Evidence:** R10 bounds individual input, but does not define aggregate model-facing output. [internal/store/messages.go:87](../../internal/store/messages.go:87) returns complete message bodies in batches; [internal/mcpserver/server.go:101](../../internal/mcpserver/server.go:101) returns the result as text and structured content. Claude documents output limits and moves sufficiently large successful results into files. [Claude MCP output limits](https://code.claude.com/docs/en/mcp#mcp-output-limits-and-warnings)

**Why it matters:** A successfully returned tool result does not establish that the model received all its contents in usable context. A default inbox page can contain hundreds of kilobytes before metadata, independently of the transport failure in A11.

**Next step:** Specify bounded summaries, explicit retrieval of message bodies, byte budgets, continuation cursors, and delivery semantics that remain correct when results are truncated, externalized, or only partly read.

## Ranked top 10

| Rank | Finding | Priority | Reason |
|---:|---|---|---|
| 1 | **A1 — Explicit claims bypassed on ignored paths** | P1 | Ordinary editing hooks can allow a known ownership conflict. |
| 2 | **A2 — Resumed session receives the wrong mailbox** | P1 | Recipient restart can strand mail and cross session identities. |
| 3 | **C1 — Redirects can transmit bearer tokens in cleartext** | P1 | Initial URL validation does not uphold the transport guarantee. |
| 4 | **A3 — Claim metadata becomes forged hook guidance** | P1 | Untrusted agent input crosses into Iris-generated model context. |
| 5 | **A4 — Next-turn attention is absent** | P1 | The principal fallback for clients without monitors is missing. |
| 6 | **A5 — Watch checkpoints undelivered notifications** | P1 | Process and connection failures can permanently lose attention. |
| 7 | **A6 — Reintegration deletes unrelated hooks** | P1 | Routine repair can silently remove user automation. |
| 8 | **A7 — Integration can invalidate client TOML** | P1 | Installing Iris can prevent the underlying client from starting. |
| 9 | **A8 — Ambiguous repository identities merge** | P1 | Claims and messages can be coordinated against the wrong repository. |
| 10 | **A9 — Retry keys do not survive ambiguous outcomes** | P1 | A committed send can be repeated as a different message. |