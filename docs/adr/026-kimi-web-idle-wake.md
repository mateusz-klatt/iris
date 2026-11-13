# 026: Kimi idle wake through the local web API

Status: accepted for opt-in implementation on the owner's direction, 2026-11-12; revised after
review on 2026-11-13. Changes D7. Extends the positive-idle and single-wake policy of ADR 023 to
Kimi without changing Codex or the other clients.

## Problem

Kimi hooks can announce Iris activity only on a later turn. Kimi 2.6.0 also has a local web
server with an authenticated REST API. Its session resource reports activity and its prompt
endpoint accepts an idempotent, client-named prompt. That can wake a session which still has an
Iris MCP child, but only with positive session identity and without taking another process's lock.

## Decision

### Configuration and binding

`iris mcp --client kimi` leaves the native runner off until native delivery and prompt-hook
consumption have acceptance evidence. `--wake` opts in and `--wake=false` disables it. Fresh
`iris integrate kimi` writes `--wake=false`; `--wake` writes `--wake`, which setup and ordinary
reintegration preserve. Existing bare MCP entries remain off. An existing child keeps its startup
arguments until its MCP connection restarts.

The integration resolves `KIMI_CODE_HOME` to an absolute path in the Iris MCP environment. It does
not write the web token into that environment or into any journal.

Only the MCP process positively bound by the single-session process rung to a hook-registered
Kimi session can arm the runner. Fallback registrations, explicit overrides, undecided processes,
lookup-only bindings and missing native session ids cannot. The hub's binding response includes
`session_id` so this check does not infer the native id from an Execution or mailbox.

### Server preflight and idle evidence

The runner reads `$KIMI_CODE_HOME/server.token` and candidates under `server/instances/`. It accepts
only a literal loopback address, refuses redirects, bounds every response, checks the registry id
against its filename, and requires the registry pid to name a live Kimi process no newer than the
registry start time. Linux rejects zombie and dead process states in that lookup. It calls
`/api/v1/healthz` without credentials before sending the token, then requires a nonempty live server
id and a matching version through `/api/v1/meta`. Kimi 2.6.0 uses different registry and live ids.
The server and client versions must match. A real Kimi 2.6.1 initialize reports `clientInfo.version`
as `0.0.0`. Only an empty value or exactly that sentinel triggers a bounded `kimi --version` command,
resolved through PATH when the MCP child starts. The command has a two-second deadline, bounded
output and strict numeric `x.y.z` parsing. Other reported versions are used unchanged. Each candidate
has an independent five-second health/metadata budget, so a stalled endpoint cannot consume a later
candidate's budget; total
preflight time grows with the candidate count. Each candidate also has a cloned transport that
permits one successful dial. Health, metadata, idle reads and the prompt use that connection; if it
closes, a later call fails rather than reconnecting to a replacement listener. Discarded candidates
close their idle connections. Iris starts no web server, resumes no session and never calls
`:takeover`.

The exact session must answer `/api/v1/sessions/{session_id}` with the same id, `busy:false`,
`main_turn_active:false`, `archived:false` and `pending_interaction:"none"`. Busy, archived,
pending, locked, unavailable, mismatched or unknown state defers without spending wake quota.
Authoritative attention and told state are read again after the idle observation before admission.

### Prompt and accounting

The prompt is the existing fixed, content-free Iris notification with its correlation token; no
message body or hub payload enters it. `prompt_id` is `iris-` plus the SHA-256 of that exact prompt.
Native answers `running` and `queued` are accepted submissions. A known refusal has the shared
single retry; an unknown or malformed response retains the outstanding guard. A response that never
arrives reaches the 30-second queue deadline and is unknown; it neither clears the guard nor permits
another wake. Binding heartbeats continue while the response is pending. The exact prompt hook
consumes that guard. Decoding accepts a text string or exactly one text part; composite and
non-text prompts never match. One automatic wake per attention identity remains the limit.

Kimi 2.6.1 answers `40931`, SESSION_OCCUPIED, while another Kimi process holds the session's lock,
as the Kimi TUI does while the session is open in it. kimi web cannot prompt such a session, so **a
session open in the Kimi TUI cannot be woken this way**. This was measured on kimi 2.6.1, whose own
message for the code says the session is in use by another instance. On the idle read or the prompt
POST, `40931` submits nothing and spends no allowance: Iris records the fixed reason "Kimi session
is open in another Kimi process", tries again after sixty seconds and never takes it as evidence
that it may remove the lock. `40901`, SESSION_BUSY, on the prompt POST is an unspent abort checked
again after 15 seconds.
The runner has no `:takeover` call. Token text, native error messages and response payloads are
not logged.

## Limits

A running `kimi web` using the same home is a prerequisite. Its API is experimental, so version
validation and the live schema are part of the compatibility boundary. Iris does not supervise that
server and does not start a closed or unloaded session. After the MCP child ends no listener remains;
ordinary hooks and durable mail still work.

Process and health checks plus the single-connection transport reduce accidental token disclosure
to stale or reused ports, but do not authenticate the endpoint before the bearer token is sent. A
local process imitating the health response on the initial connection can still receive it. Once the
verified connection closes, wake fails rather than trusting a replacement listener. The version
fallback names a binary resolved from PATH, not necessarily the binary behind the already running
Kimi process. A different PATH target can therefore produce a false mismatch; it fails closed without
a prompt and does not prove which version the running client loaded.

Idle observation and binding admission are not atomic with prompt submission. Connect and request
writing have a separate two-second deadline under the binding lock. On timeout Iris cancels the
request and joins its transport before unlocking, so it cannot be written late. After a write,
response headers and body are awaited after unlocking, up to the 30-second queue deadline. Binding
can change after submission starts, and a person can start a turn or fetch mail between idle
observation and submission, so an accepted prompt can run later or find
no work. Queue acceptance proves neither a completed turn nor an inbox fetch. Iris cannot promise zero
billable empty turns. A session open in another Kimi process, such as the TUI, cannot be woken while
that process holds its lock: `40931` is tried again every sixty seconds and never taken over. Active
sessions are checked again after 15 seconds. Native TUI refresh, cross-platform behavior and model
cost require separate acceptance evidence.

## Verification

Tests use an authenticated fake Kimi HTTP server and the real hub/store. They cover process and
health verification before token disclosure, a real Linux zombie, one connection across all calls,
reconnect refusal after that connection closes, and transport completion at the short start
deadline. A real resolver heartbeat completes while the prompt response is held. The unedited Kimi
2.6.1 initialize fixture feeds the version regression: it failed before the fallback and passed after
it. Command failure, timeout, malformed and overflowing output, missing or relative PATH results and
server mismatch all fail before a prompt. Further cases cover version and identity checks, loopback
and redirect refusal, exact idle evidence, running and queued
acceptance, stable idempotency, known refusals, quota-free `40901` and `40931` deferrals and their
retries, unknown
outcomes and separate Kimi guards. Prompt tests use the real capture's shape to prove exact guard
consumption and reject composite or non-text parts. Installation tests
cover opt-in, explicit preference persistence, custom homes and broken files. Binding tests prove
that only the hook session of the single-session client process can arm the runner. No fixture
invokes `:takeover` or spends model credits.

An isolated real Kimi 2.6.0 web server passed preflight and the exact idle read, then accepted the
fixed prompt. With no provider configured it ended as `last_turn_reason:"failed"` with zero turns,
input and output tokens and zero cost. Repeating the same `prompt_id` did not create another turn.
This validates the HTTP adapter and idempotency, not native model delivery, live prompt-hook
consumption or TUI refresh.

Run the full local gates. Record any real web-server or TUI acceptance separately, and do not
present an HTTP fixture as proof of native model delivery or cross-platform support.
