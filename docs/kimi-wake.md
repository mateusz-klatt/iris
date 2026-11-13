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

Install or update Iris, then restart the Kimi session's MCP connection:

```sh
iris integrate kimi
```

Fresh integration enables wake. Disable or explicitly enable future sessions with:

```sh
iris integrate kimi --no-wake
iris integrate kimi --wake
```

`--no-wake` records `--wake=false`. Setup and ordinary reintegration preserve that choice. A bare
`iris mcp --client kimi` also enables wake; disable a manually configured server with
`iris mcp --client kimi --wake=false`. Existing bare entries therefore take the enabled default.
`iris integrate --remove kimi` removes Iris's integration.

For a custom home, the integration writes the same absolute or inherited `KIMI_CODE_HOME` into the
Iris MCP environment. Start `kimi web` with that home too. When several instances are registered,
Iris tries their loopback endpoints until it finds one with a valid live server id and a version
matching `/api/v1/meta`. Registry and live ids can differ in Kimi 2.6.0. The initialized client and
server versions must match.

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
A live `40931` conflict defers or refuses the submission; Iris does not remove the other process's
lock or risk interleaved session history.

## Limits and recovery

Iris does not start or supervise `kimi web`, resume a closed session or retain a listener after the
MCP child ends. The API is experimental, so the version check is deliberate. Without a compatible
server, wake waits and the existing hook notifications and Iris tools continue to work.

The idle read and prompt submission are not atomic. A person can start a turn or fetch mail between
them, so an accepted prompt can run later and find no work. Queue acceptance proves neither a
completed turn nor an inbox fetch, and Iris cannot guarantee zero empty or billable turns.

An accepted or uncertain wake retains its guard until its exact prompt hook consumes it. Resume
that session first. There is no timeout that clears an outstanding guard. Do not remove a live
guard or clear all session state to repair it. The current guard-inspection report is the Codex-era
report; native Kimi arming, TUI refresh and delivery are not proved by `iris doctor --live`.

The shared runner and fake-server protocol tests run without model credits. An isolated real Kimi
2.6.0 server also accepted the adapter's prompt; without a provider it failed before a model turn,
with zero tokens and zero cost. Real TUI delivery, cross-platform acceptance and model cost need
separate evidence. See
[ADR 026](adr/026-kimi-web-idle-wake.md) for the decision and [Kimi's Server API reference](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/server-api.html)
for the native protocol.
