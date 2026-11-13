# Antigravity notifications while idle

Iris can ask an interactive Antigravity CLI (`agy`) session, at the end of a turn, to start a
detached `iris watch --client antigravity --wake` helper for that conversation. When new Iris
attention arrives while the conversation is recorded idle, the helper submits a fixed content-free
notification through `<agy-exe> agentapi send-message`. Each accepted wake can start a model turn
and spend model budget.

## Enable or disable wake

Fresh integration and ordinary reintegration leave Antigravity wake off. Enable or disable it
explicitly with:

```sh
iris integrate antigravity --wake
iris integrate antigravity --no-wake
```

`--wake` writes `on` to `antigravity-wake` in the Iris configuration directory, refusing (and naming
both paths) if environment overrides resolve a directory different from the OS account's default Iris
directory. `--no-wake` writes `off`. Setup and bare `iris integrate antigravity` keep an existing
`on` or `off` choice. Both `--no-wake` and `iris integrate --remove antigravity` stop live
Antigravity wake helpers after verifying each helper process by PID and start time, and always also
write `off` and stop recorded helpers in the OS account's default Iris directory idempotently.

## Approval and arming

Antigravity passes `ANTIGRAVITY_LS_ADDRESS` and `ANTIGRAVITY_CSRF_TOKEN` to `run_command` shells,
not to hooks or MCP servers. When the hook's Iris directory matches the OS account's default
directory, wake is `on`, and a conversation reaches an idle `Stop` (`fullyIdle: true`,
`terminationReason == "NO_TOOL_CALL"`, `executionNum == 0`, `continuationActive == false`) with no
live helper for that `conversationId` and no `failed` latch on that `(agy pid, start_ms)`, `Stop`
returns `decision: "continue"` once asking the model to invoke:

```sh
'<iris-exe>' watch --client antigravity --wake
```

in `run_command` with `BypassSandbox: true` (required on Linux/WSL2 so the helper is not placed in
an isolated network namespace where the host Language Server is unreachable).

Iris **never writes a `command(...)` allow rule** to `~/.gemini/antigravity-cli/settings.json`. In
`agy 1.5.8`, `command(...)` matches by command prefix and allows extra arguments, `VAR=value`
assignments and `env` wrappers outside the sandbox. Leaving `settings.json` with `"mcp(iris/*)"`
alone means Antigravity prompts the user once per armed conversation (and again after `/clear`,
which starts a new `conversationId`). Do not choose a persistent `command(...)` rule for `iris` if
you want Antigravity's command sandbox to keep gating other `iris` invocations.

If the prompt is denied, ignored, fails preflight or runs in headless `-p` mode without interactive
approval, Iris latches `failed` on that `(agy pid, start_ms)` process record. Because the latch is
per `(agy pid, start_ms)` rather than per conversation, `Stop` does not ask again on that `agy`
process — including after `/clear` — until either `'<iris-exe>' watch --client antigravity --wake`
is run manually in `run_command` with `BypassSandbox: true` and starts a live helper, or `agy`
restarts.

## Hardened helper and guard consumption

The helper accepts only the exact arguments `watch --client antigravity --wake`. It resolves its
hub configuration and token strictly from the OS account's default Iris configuration directory
(ignoring `HOME`, `XDG_CONFIG_HOME`, `APPDATA`, `IRIS_HUB_URL`, `IRIS_TOKEN_FILE` and
`IRIS_EXECUTION`), resolves `<agy-exe>` from the verified `agy` ancestor process image rather than
`PATH` or `ANTIGRAVITY_AGENTAPI_EXE`, pins `ANTIGRAVITY_LS_ADDRESS` (`127.0.0.1`, `::1`, or
`localhost` dialed directly against literal loopback IPs without DNS) to a reachable literal
loopback `host:port`, and passes `ANTIGRAVITY_CSRF_TOKEN` only in the scrubbed environment of the
detached child and `<agy-exe> agentapi`.

Wake delivery enters Antigravity as a `SYSTEM` / `SYSTEM_MESSAGE` transcript step rather than firing
`BeforeAgent`. Both `PostInvocation` hooks and the detached helper's poll loop scan forward through
`transcript_full.jsonl` from `idle_offset` in bounded `4 MiB` passes, handling rotation, truncation
and oversized lines, and consume the wake guard only when the whole notification payload matches a
`SYSTEM` / `SYSTEM_MESSAGE` envelope (`sender=<conversationId>` in headless `-p` mode or
`sender=system` in interactive `-i` mode on `agy 1.5.8`).

## Limits

- **No wake before first `Stop`.** Nothing can wake a conversation before its first turn reaches
  `Stop` and arms the helper.
- **Request, not enforcement.** The model may ignore the `Stop` request or omit
  `BypassSandbox: true`, and enterprise policies may disallow unsandboxed commands.
- **Already-queued messages cannot be recalled.** `--no-wake` and `--remove` terminate live helpers
  and block new submissions, but a message already accepted by `agy` stays in its queue.
- **Busy-turn race.** Idle state recording and `agentapi send-message` are not atomic; if a turn
  starts between them, `agy` queues the message as `<SYSTEM_MESSAGE>` into the active turn, and
  Iris cannot promise zero billable empty turns.
- **Same-user loopback boundary and default config path.** Same-OS-user processes retain normal OS
  process visibility, Iris cannot authenticate the loopback listener before `agentapi` sends the
  CSRF token, in-place upgrades of a running `agy` binary on Linux mark `/proc/<pid>/exe` deleted
  until `agy` restarts, and non-default Iris locations set only via environment variables cannot use
  Antigravity wake. Measurements ran on Linux/WSL2 with `agy 1.5.8`; macOS and Windows remain
  unmeasured live. See [ADR 028](adr/028-antigravity-idle-wake.md) for the full design and
  measurements.
