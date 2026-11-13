# Kimi notifications while idle

Iris can ask a loaded Kimi session to read its inbox and claims when new attention arrives and its
native session is verified idle. This uses the local **Kimi Web Server API** and the session's own
model. Each accepted wake can start a model turn and spend model budget.

## Enable the native server

Start a server with the same Kimi home as the session, on loopback:

```sh
kimi web --no-open
```

Do not use `--dangerous-bypass-auth` for ordinary use. Iris reads the server token from
`$KIMI_CODE_HOME/server.token`; it does not print it or copy it into the MCP configuration.

Install or update Iris, explicitly enable wake, then restart the Kimi session's MCP connection:

```sh
iris integrate kimi --wake
```

Fresh integration and a bare `iris mcp --client kimi` leave wake off while native delivery is
unproved. Disable it again or enable it later with:

```sh
iris integrate kimi --no-wake
iris integrate kimi --wake
```

`--no-wake` records `--wake=false`. Setup and ordinary reintegration preserve that choice. A
manually configured server opts in with `iris mcp --client kimi --wake`; `--wake=false` disables it.
Existing bare entries remain disabled. `iris integrate --remove kimi` removes Iris's integration.

For a custom home, the integration resolves `KIMI_CODE_HOME` to an absolute path in the Iris MCP
environment. Start `kimi web` with that home too. When several instances are registered,
Iris tries their loopback endpoints until it finds a live Kimi process consistent with the registry
pid and start time. It probes `/api/v1/healthz` without credentials before sending the token, then
requires a valid live server id and a version matching `/api/v1/meta`. Registry and live ids can
differ in Kimi 2.6.0. The client and server versions must match. Kimi 2.6.1 reports `0.0.0` in MCP
clientInfo, so only that exact sentinel or an empty value triggers a two-second, strictly parsed
`kimi --version` fallback through PATH. Each candidate has its own five-second health/metadata
budget, so a stalled endpoint does not consume the next candidate's
budget; total preflight time grows with the candidate count. Health, metadata, idle reads and the
prompt share one connection. Iris refuses to reconnect that candidate after the connection closes.

## What Iris does

The session's Iris MCP child first needs its positive binding to the hook-registered, single-session
Kimi process. An ordinary Iris tool call establishes it. Fallback, overridden, undecided or
lookup-only bindings cannot arm wake.

After attention settles, Iris checks the exact session through REST. It must be unarchived and have
`busy:false`, `main_turn_active:false` and `pending_interaction:"none"`. Busy, locked, unavailable
or unknown state defers without spending the wake allowance. Iris reads authoritative attention and
hook state again after the idle check before submitting anything.

The prompt contains only the fixed Iris notification and a correlation token, never mail bodies.
Its client-chosen `prompt_id` is a SHA-256 of that exact prompt, so repeating the same request does
not intentionally create another wake. Native `running` and `queued` responses count as accepted.
There is one automatic wake per attention identity and one shared retry after a known refusal.

Iris **never calls `:takeover`**. That endpoint is only for a lock left by a process known to be gone.
A live `40931` conflict is a non-submission deferral, including when the prompt POST returns it.
It spends no wake allowance and schedules another idle check after 15 seconds. Iris does not remove
the other process's lock or risk interleaved session history. A front-end that retains its native
lock can prevent wake indefinitely; only a session that the native API reports idle and whose prompt
it accepts is eligible.

## Limits and recovery

Iris does not start or supervise `kimi web`, resume a closed session or retain a listener after the
MCP child ends. The API is experimental, so the version check is deliberate. Without a compatible
server, wake waits and the existing hook notifications and Iris tools continue to work.

The process and health checks plus the single-connection transport reduce accidental token
disclosure to a stale or reused port. They do not authenticate the initial connection before the
token is sent: a local process imitating Kimi's health response can still receive it. A closed
verified connection makes later calls fail instead of reaching a replacement listener. The version
fallback uses the `kimi` binary found on PATH, which can differ from the already running client after
an upgrade or a PATH change. It does not prove the running process's version; every probe failure or
server mismatch fails closed without a prompt. The token is never intentionally logged.

The idle read, binding admission and prompt submission are not atomic. Connect and request writing
have a separate two-second deadline under the binding lock. On timeout Iris cancels and joins the
transport before unlocking, so the request cannot be written late. Response headers and body are
awaited after unlocking. Binding can change after submission starts, and a person can start a turn
or fetch mail after the idle read, so an accepted prompt can run later and find no work. Queue acceptance
proves neither a completed turn nor an inbox fetch, and Iris cannot guarantee zero empty
or billable turns.

An accepted or uncertain wake retains its guard until its exact prompt hook consumes it. If the
HTTP response never arrives, the 30-second queue deadline records an unknown outcome; binding
heartbeats continue while it waits. The deadline does not clear the guard or permit another wake.
Resume that session first. There is no timeout that clears an outstanding guard. Do not remove a live
guard or clear all session state to repair it. The current guard-inspection report is the Codex-era
report; native Kimi arming, TUI refresh and delivery are not proved by `iris doctor --live`.

The shared runner and fake-server protocol tests run without model credits. Tests use the real
captured prompt shape to verify guard consumption and reject composite or non-text prompts, but no
API-submitted prompt has yet produced a captured live hook. An isolated real Kimi 2.6.0 server
accepted the adapter's prompt; without a provider it failed before a model turn, with zero tokens
and zero cost. Real hook consumption, TUI delivery, cross-platform acceptance and model cost need
separate evidence. See
[ADR 026](adr/026-kimi-web-idle-wake.md) for the decision and [Kimi's Server API reference](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/server-api.html)
for the native protocol.
