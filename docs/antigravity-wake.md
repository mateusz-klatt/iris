# Antigravity notifications while idle

Iris can ask an interactive Antigravity CLI (`agy`) session, at the end of a turn, to start a
detached `iris watch --client antigravity --wake` helper for that conversation. When new Iris
attention arrives while the conversation is recorded idle, the helper submits a fixed content-free
notification through `<agy-exe> agentapi send-message`. Each accepted wake can start a model turn
and spend model budget.

## Enable or disable wake

Fresh `iris integrate antigravity` and `iris setup` enable Antigravity wake by default when the
resolved Iris configuration directory matches the OS account's default Iris directory, writing
`default` to `antigravity-wake` beside `hub.token`. Iris never writes a `command(...)` allow rule,
and it does not decide whether agy asks the user before each arming (including after `/clear`).
agy's permission mode and its safety monitors decide that: agy may ask, run the arming command
without asking, or refuse it before a prompt (see [Approval and arming](#approval-and-arming)).
Override the preference explicitly with:

```sh
iris integrate antigravity --no-wake
iris integrate antigravity --wake
```

`antigravity-wake` uses three on-disk states so a default installation is never mistaken for an
explicit choice:

- `default` — no explicit choice (wake enabled), written by `iris integrate antigravity`,
  `iris integrate --all` and `iris setup` on the OS account's default Iris directory when no
  explicit `on` or `off` marker is present. (If environment overrides resolve a different directory,
  plain `iris integrate antigravity` and `iris setup` install hooks and MCP without writing
  `antigravity-wake`, since the hardened helper runs only against the OS account's default
  directory.)
- `on` — explicit opt-in (`--wake`, wake enabled), refusing (and naming both paths) if environment
  overrides resolve a directory different from the OS account's default Iris directory.
- `off` — explicit opt-out (`--no-wake`, wake disabled).

`iris setup` and bare `iris integrate antigravity` keep an existing `on` or `off` choice untouched.
Prior installations without `antigravity-wake` map to no explicit choice: wake remains off while the
file is absent (`iris doctor` reports configuration drift on `antigravity-wake`), and running
`iris integrate antigravity` or `iris setup` writes `default` and enables wake; existing `on` and
`off` files keep their explicit meaning. Both `--no-wake` and `iris integrate --remove antigravity`
stop live Antigravity wake helpers after verifying each helper process by PID and start time, and
always also write `off` (when split or untracked on `--remove`, and always on `--no-wake`) and stop
recorded helpers in the OS account's default Iris directory idempotently.

## Approval and arming

Antigravity passes `ANTIGRAVITY_LS_ADDRESS` and `ANTIGRAVITY_CSRF_TOKEN` to `run_command` shells,
not to hooks or MCP servers. When the hook's Iris directory matches the OS account's default
directory, wake is enabled (`default` or `on`), and a conversation reaches an idle `Stop`
(`fullyIdle: true`,
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
assignments and `env` wrappers outside the sandbox. Do not choose a persistent `command(...)` rule
for `iris` if you want Antigravity's command sandbox to keep gating other `iris` invocations.

Whether the arming command waits for user approval (including after `/clear`, which starts a new
`conversationId`) depends on agy's permission mode and its safety monitors. Iris does not decide
it, and leaving `settings.json` with `"mcp(iris/*)"` alone does not guarantee a prompt.

Measured on macOS on 2026-11-14 with `agy 1.5.9`, in an interactive session whose status bar
showed `auto` (a mode not set in `settings.json`), agy asked nothing:

- The `Stop`-requested arming ran with `BypassSandbox: true` without a prompt.
- As a control, the model ran `iris version`. With `BypassSandbox: false` the sandbox refused it
  (`operation not permitted`, exit 127). The model retried with `BypassSandbox: true`, and the
  command ran without a prompt although no allow rule matched it.
- The transcript shows delays it does not explain. The bypassed `iris version` started 36 s after
  the model's step, and the sandboxed attempt 35 s after its step. The arming command ran 44 s
  from start to completion.

That run cannot show what a `command(...)` rule changes. The host had
`command(<iris-path> watch --client antigravity --wake)` in `settings.json`, but in that mode agy
asked nothing either way, so the rule's own effect is unmeasured. It also shows that in that mode
the sandbox does not gate other `iris` invocations, because the model can request the bypass
itself.

On Windows with `agy 1.5.8`, the safety monitors flagged `BypassSandbox: true` and asked for
confirmation even when a persistent rule matched; the confirmation appeared 35–45 s after the
request. They also refused a `Stop`-requested arming without asking the user after flagging the
hook-initiated request as "rogue persistence". Another such request reached the user as a normal
confirmation, so the monitors are nondeterministic. Iris does not reword its request to get past
a safety classifier. The "always allow" choice writes the `command(...)` prefix rule described
above. To keep the sandbox gating other `iris` invocations, allow the helper once each time instead.

`Stop` asks only an interactive session ([ADR 031](adr/031-wake-only-in-interactive-sessions.md)).
Iris reads the `agy` process's arguments.
- A run with `-p`, `--print` or `--prompt`, or with a subcommand such as `acp`, gets no request.
- A run with `-i` is interactive.
- A run without either is interactive only when it reads a terminal, because agy reads a piped
  prompt from standard input and runs headless.
- A flag Iris does not know, or arguments it cannot read, also mean no request.

Each skipped request is noted as `wake not asked: <reason>` in the observed log. On Linux and
Windows the terminal check reads agy's own input. On macOS it asks whether the agy process has a
controlling terminal, so there a prompt piped into agy by a person at a terminal is asked once.

If the prompt is denied, ignored or fails preflight, Iris latches `failed` on that
`(agy pid, start_ms)` process record. Because the latch is
per `(agy pid, start_ms)` rather than per conversation, `Stop` does not ask again on that `agy`
process — including after `/clear` — until either `'<iris-exe>' watch --client antigravity --wake`
is run manually in `run_command` with `BypassSandbox: true` and starts a live helper, or `agy`
restarts.

## Hardened helper and guard consumption

The helper accepts only the exact arguments `watch --client antigravity --wake`. It resolves its
hub configuration and token strictly from the OS account's default Iris configuration directory
(ignoring `HOME`, `XDG_CONFIG_HOME`, `APPDATA`, `IRIS_HUB_URL`, `IRIS_TOKEN_FILE` and
`IRIS_EXECUTION`), resolves `<agy-exe>` from the verified `agy` ancestor process image rather than
`PATH` or `ANTIGRAVITY_AGENTAPI_EXE` (on Linux, if `/proc/<pid>/exe` is marked `" (deleted)"` after
an in-place update, it verifies `(pid, start_ms)`, opens `/proc/<pid>/exe`, checks `fstat` for a
regular file, rechecks `(pid, start_ms)`, and executes `agentapi send-message` from that open
descriptor via `/proc/self/fd/3` rather than running whatever newer file now sits at the old path),
pins `ANTIGRAVITY_LS_ADDRESS` (`127.0.0.1`, `::1`, or `localhost` dialed directly against literal
loopback IPs without DNS) to a reachable literal loopback `host:port`, and passes
`ANTIGRAVITY_CSRF_TOKEN` only in the scrubbed environment of the detached child and
`<agy-exe> agentapi`.

Wake delivery enters Antigravity as a `SYSTEM` / `SYSTEM_MESSAGE` transcript step rather than firing
`BeforeAgent`. Both `PostInvocation` hooks and the detached helper's poll loop scan forward through
`transcript_full.jsonl` from `idle_offset` in bounded `4 MiB` passes, handling rotation, truncation
and oversized lines, and consume the wake guard only when the whole notification payload matches a
`SYSTEM` / `SYSTEM_MESSAGE` envelope (`sender=<conversationId>` in headless `-p` mode or
`sender=system` in interactive `-i` mode on `agy 1.5.8`).

The runner records its client in each guard's optional top-level `client` field for doctor
diagnostics; this field does not affect guard validity. `iris doctor` labels Antigravity guards
`antigravity wake:`. A guard without a readable recorded client is labelled `wake guard:` and says
`client not recorded`; an unrecognized value says `client not recognized`.

For an outstanding Antigravity guard, continue that agy conversation so a hook can scan its
transcript. If the notification never reached the conversation, the guard stays. Remove the exact
guard file only after that conversation is permanently closed and its wake helper has stopped;
removing a live guard can admit a duplicate notification.

## Limits

- **No wake before first `Stop`.** Nothing can wake a conversation before its first turn reaches
  `Stop` and arms the helper.
- **Request, not enforcement.** The model may ignore the `Stop` request or omit
  `BypassSandbox: true`, and enterprise policies may disallow unsandboxed commands. agy's own
  safety monitors can refuse arming before asking the user. Iris then latches `failed` and does
  not ask again in that agy process; the user can still arm by asking the model to run the command.
- **Already-queued messages cannot be recalled.** `--no-wake` and `--remove` terminate live helpers
  and block new submissions, but a message already accepted by `agy` stays in its queue.
- **Busy-turn race.** Idle state recording and `agentapi send-message` are not atomic; if a turn
  starts between them, `agy` queues the message as `<SYSTEM_MESSAGE>` into the active turn, and
  Iris cannot promise zero billable empty turns.
- **Same-user loopback boundary, in-place updates, and default config path.** Same-OS-user
  processes retain normal OS process visibility, Iris cannot authenticate the loopback listener
  before `agentapi` sends the CSRF token, and non-default Iris locations set only via environment
  variables cannot use Antigravity wake. On Linux, when an in-place update unlinks the running
  `agy` binary and leaves `/proc/<pid>/exe` marked `" (deleted)"`, Iris falls back to executing
  `agentapi send-message` from an open descriptor to that running image (`/proc/self/fd/3`), and
  reports `"the running agy executable was deleted, commonly by an in-place update; restart agy if
  wake stays unavailable"` in `iris doctor` if that fallback fails. This fallback is Linux-only and
  has explicit limits:
  - The open descriptor preserves the running process's executable identity; it adds no security
    boundary against same-OS-user processes (already inside the same-user trust boundary).
  - The unlinked image executed from that descriptor is still the old `agy` binary code until `agy`
    restarts.
  - `agentapi` runs with `/proc/self/fd/3` as both its exec path and `argv[0]`. `agy 1.5.14` was
    verified live, but a client or later version that locates resources through `argv[0]` could
    behave differently.
  - Queuing a message via `agentapi send-message` does not prove the model acts on it.
  Measurements ran with `agy 1.5.8` on Linux/WSL2 and on Windows 11 (and deleted-descriptor live
  verification with `agy 1.5.14` on Linux/WSL2); macOS has not been measured live.
- **Windows.**
  - agy runs each command in a job that ends with the command and forbids breakaway. Iris therefore
    starts the helper under the `agy` process it has verified, not under the command.
  - agy also gives each command a temporary desktop that it destroys when the command ends.
    Inheriting that desktop caused `agentapi send-message` to fail loading `user32` with
    `0xC0000142`; removing the console did not fix it. Iris creates the helper on
    `<winsta>\Default` when the launcher's desktop is another one (the bare `Default` when agy
    names the desktop alone, as 1.5.8 does), and refuses malformed values. Empty or `Default`
    values inherit.
  - `<agy-exe> agentapi send-message` runs without a console only to avoid opening a visible
    console window on `<winsta>\Default`.
  - A console window that flashes when agy starts belongs to agy's own background updater
    (`agy --bg-updater`, measured on agy 1.5.9). Iris starts nothing before the first prompt.
  - Wake depends on agy running in no job and on agy's own desktop being `<winsta>\Default`.
    Both were measured on `agy 1.5.8`; a later agy may change them. See
    [ADR 028](adr/028-antigravity-idle-wake.md) for the full design and measurements.
