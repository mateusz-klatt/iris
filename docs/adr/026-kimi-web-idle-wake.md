# 026: Kimi idle wake through the local web API

Status: accepted on the owner's direction, 2026-11-12. Changes D7. Extends the positive-idle
and single-wake policy of ADR 023 to Kimi without changing Codex or the other clients.

## Problem

Kimi hooks can announce Iris activity only on a later turn. Kimi 2.6.0 also has a local web
server with an authenticated REST API. Its session resource reports activity and its prompt
endpoint accepts an idempotent, client-named prompt. That can wake a session which still has an
Iris MCP child, but only with positive session identity and without taking another process's lock.

## Decision

### Configuration and binding

`iris mcp --client kimi` enables the native runner by default. `--wake=false` disables it.
`iris integrate kimi` writes `--wake`; `--no-wake` writes `--wake=false`, which setup and ordinary
reintegration preserve. `--wake` explicitly enables it again. Existing bare MCP entries take the
enabled default. An existing child keeps its startup arguments until its MCP connection restarts.

The integration writes the absolute or inherited `KIMI_CODE_HOME` into the Iris MCP environment.
It does not write the web token into that environment or into any journal.

Only the MCP process positively bound by the single-session process rung to a hook-registered
Kimi session can arm the runner. Fallback registrations, explicit overrides, undecided processes,
lookup-only bindings and missing native session ids cannot. The hub's binding response includes
`session_id` so this check does not infer the native id from an Execution or mailbox.

### Server preflight and idle evidence

The runner reads `$KIMI_CODE_HOME/server.token` and candidates under
`server/instances/`. It accepts only a literal loopback address, refuses redirects, bounds every
response, checks the registry id against its filename, requires a nonempty live server id and
validates the version through `/api/v1/meta`. Kimi 2.6.0 uses different registry and live ids.
The server and initialized client versions must match. It starts no web server, resumes no session and never
calls `:takeover`.

The exact session must answer `/api/v1/sessions/{session_id}` with the same id, `busy:false`,
`main_turn_active:false`, `archived:false` and `pending_interaction:"none"`. Busy, archived,
pending, locked, unavailable, mismatched or unknown state defers without spending wake quota.
Authoritative attention and told state are read again after the idle observation before admission.

### Prompt and accounting

The prompt is the existing fixed, content-free Iris notification with its correlation token; no
message body or hub payload enters it. `prompt_id` is `iris-` plus the SHA-256 of that exact prompt.
Native answers `running` and `queued` are accepted submissions. A known refusal has the shared
single retry; an unknown or malformed response retains the outstanding guard. The exact prompt
hook consumes that guard. One automatic wake per attention identity remains the limit.

`40931` is a refusal to retry later, never evidence that Iris may remove a lock. The runner has no
`:takeover` call. Token text, native error messages and response payloads are not logged.

## Limits

A running `kimi web` using the same home is a prerequisite. Its API is experimental, so version
validation and the live schema are part of the compatibility boundary. Iris does not supervise that
server and does not start a closed or unloaded session. After the MCP child ends no listener remains;
ordinary hooks and durable mail still work.

Idle observation and prompt submission are separate operations. A person can start a turn or fetch
mail between them, so an accepted prompt can run later or find no work. Queue acceptance proves
neither a completed turn nor an inbox fetch. Iris cannot promise zero billable empty turns. Native
TUI refresh, cross-platform behavior and model cost require separate acceptance evidence.

## Verification

Tests use an authenticated fake Kimi HTTP server and the real hub/store. They cover discovery,
version and identity checks, loopback and redirect refusal, exact idle evidence, running and queued
acceptance, stable idempotency, known refusals including `40931`, unknown outcomes, prompt-hook
consumption and separate Kimi guards. Installation tests cover default-on, explicit-off persistence,
custom homes and broken files. Binding tests prove that only the hook session of the single-session
client process can arm the runner. No fixture invokes `:takeover` or spends model credits.

An isolated real Kimi 2.6.0 web server passed preflight and the exact idle read, then accepted the
fixed prompt. With no provider configured it ended as `last_turn_reason:"failed"` with zero turns,
input and output tokens and zero cost. Repeating the same `prompt_id` did not create another turn.
This validates the HTTP adapter and idempotency, not native model delivery or TUI refresh.

Run the full local gates. Record any real web-server or TUI acceptance separately, and do not
present an HTTP fixture as proof of native model delivery or cross-platform support.
