# 028: Antigravity idle wake through a model-armed helper

Status: accepted and verified live on 2026-11-13; amended on 2026-11-13 to turn wake on by default
on the owner's direction (~20:20 UTC), and on 2026-11-14 by
[ADR 031](031-wake-only-in-interactive-sessions.md): `Stop` asks only an interactive session
(measurement 16). Changes D7 for Antigravity only. Extends the single-wake policy of ADR 017 and
ADR 023 without changing the other clients.

## Problem

Antigravity CLI (`agy`) hooks and its stdio MCP child (`iris mcp --client antigravity`) cannot
start a model turn while a session is idle, so a loaded session learns of new Iris mail or freed
claims only on a later turn.

`agy` provides `agy agentapi send-message <conversation_id> <message>`, which dials the session's
in-process Language Server (`ANTIGRAVITY_LS_ADDRESS`) using a per-process secret
(`ANTIGRAVITY_CSRF_TOKEN`). Clean-environment measurements on `agy 1.5.8` showed why neither
`SessionStart` nor `iris mcp` can run that command directly:

- Top-level `agy` has no `ANTIGRAVITY_*` variables in its initial process environment, writes no
  port or token file to disk, and does not start workspace plugin sidecars in CLI mode (`1.5.8`).
- `iris mcp --client antigravity` receives no `ANTIGRAVITY_*` variables.
- All six lifecycle hooks (`SessionStart`, `PreInvocation`, `PreToolUse`, `PostToolUse`,
  `PostInvocation` and `Stop`) receive `ANTIGRAVITY_CONVERSATION_ID` alone.
- Only subprocesses started by the `run_command` tool receive `ANTIGRAVITY_LS_ADDRESS` and
  `ANTIGRAVITY_CSRF_TOKEN` alongside `ANTIGRAVITY_CONVERSATION_ID`. Earlier experiments saw those
  variables in hooks because an inner `agy --print` had been launched from an outer Antigravity
  `run_command` shell, inheriting the outer shell's environment.

## Measurements

All measurements ran on Linux/WSL2 with `agy 1.5.8` (`gemini-3.8-flash-low`) after stripping every
`ANTIGRAVITY_*` variable from the outer environment.

1. **Delivery while idle.** Sent 2.0 s after `Stop` (`fullyIdle: true`), `agy agentapi send-message`
   exited `0` in `905 ms` and started a new model turn about `45 ms` later. In `transcript.jsonl`
   (and `transcript_full.jsonl`, which `transcriptPath` names), the message was recorded as a
   `SYSTEM_MESSAGE` step (`source: "SYSTEM"`, `type: "SYSTEM_MESSAGE"`), not `USER_INPUT`. When
   `PreInvocation` ran for that woken turn, `lastUserInput` still held the earlier user prompt and
   the `SYSTEM_MESSAGE` step had not yet been flushed to disk; by `PostInvocation` and `Stop` it
   was present on disk.
2. **Delivery while busy.** Sent 1.0 s into a 4 s `run_command`, `send-message` exited `0` in
   `713 ms` without interrupting the running tool. `agy` queued it and injected it as a
   `SYSTEM_MESSAGE` step right after `PostToolUse`, before the next model call (`invocationNum: 1`)
   of the same turn, finishing in a single `Stop`.
3. **Conversation identity.** `ANTIGRAVITY_CONVERSATION_ID` in hook and `run_command` environments
   equaled `conversationId` in hook stdin JSON and the `<recipient_id>` accepted by
   `agy agentapi send-message`.
4. **Detached helper lifetime.** A helper detached into a new session with standard streams closed
   (`/dev/null`) survived `run_command` exit and all Turn 1 hooks, woke the idle session (`634 ms`
   in the unsandboxed `BypassSandbox` run) and exited when the host `agy` process ended.
5. **Stale credentials and dead server.** Against a live Language Server with a wrong or stale
   `ANTIGRAVITY_CSRF_TOKEN`, `send-message` exited `1` (`Unauthenticated: invalid CSRF token`;
   missing token: `missing CSRF token`). After `agy` exited, dialing the old
   `ANTIGRAVITY_LS_ADDRESS` exited `1` (`Unavailable: connect: connection refused`).
6. **Arming point and `continuationActive` drift.** Without `--dangerously-skip-permissions`, a
   `PreInvocation` `injectSteps` `systemMessage` asking the model to start the helper produced zero
   tool calls (`gemini-3.8-flash-low` answered the prompt and stopped), whereas `Stop` returning
   `{"decision":"continue","reason":"..."}` caused the model to call `run_command` right after
   Turn 1's answer. Antigravity's hook documentation states that `continuationActive` is `true`
   when `executionNum > 0`, but on `agy 1.5.8` `continuationActive` remained `false` across
   `executionNum` `0` through `4`.
7. **Terminal sandbox.** Under `agy --sandbox` (and `auto` mode), `run_command` executes in a
   namespace sandbox that sees only namespace PIDs (`[1, 31]`), kills detached children when the
   foreground command exits, cannot access `~/.config/iris`, and gets `connection refused` dialing
   `127.0.0.1:<LS_PORT>`. Setting `BypassSandbox: true` runs the helper on the host.
8. **Permission matcher (`permissions.allow`) in a clean `HOME`.** Tested on `agy 1.5.8` (`-p
   --sandbox`, `toolPermission: "request-review"`, clean `HOME` with no inherited allow rules):
   - `command(<iris-path> watch --client antigravity --wake)` denied an unallowed `/bin/echo`
     control (with `PostToolUse` still receiving `"error": ""` on auto-denial) and blocked `&&` and
     newline command chains, **but allowed appended arguments (`--extra`), prefixed shell variable
     assignments (`FOO=1 <cmd>`), and the `env` wrapper (`env FOO=1 <cmd>`) outside the sandbox**.
   - Even `command(regex:\A<escaped-cmd>\z)` rejected `env FOO=1 <cmd>` **while still allowing
     `FOO=1 <cmd>` and `--extra` outside the sandbox**.

The Windows measurements ran on Windows 11 with `agy 1.5.8` on 2026-11-14. They used a scratch
`-i` conversation and manual `run_command` calls with `BypassSandbox: true`, and the user approved
each one at the keyboard.

9. **Command job.** `run_command` runs each command in a job, on its own desktop
   `WinSta0\exebox-<id>`. `agy.exe`, its MCP child and the hook processes are in no job. A child
   that the command started with `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS` was gone as soon as
   the command completed. The launcher had already taken its success path, and `PostToolUse`
   latched `failed`.
10. **Breakaway refused.** With `CREATE_BREAKAWAY_FROM_JOB` added, `CreateProcess` was refused:
    agy's job does not allow breakaway.
11. **Reparenting.** A helper created under the verified `agy` process, through
    `PROC_THREAD_ATTRIBUTE_PARENT_PROCESS`, had `agy.exe` as its parent, was in no job and outlived
    the command.
12. **Desktop.** The reparented helper kept the desktop of the process that created it.
    - `<agy-exe> agentapi send-message`, started from that helper, died at startup with
      `0xC0000142` (`STATUS_DLL_INIT_FAILED`) and wrote nothing. The attempt was classified
      `unknown` within 47–55 ms, and nothing was queued.
    - Started without a console (`DETACHED_PROCESS`), it answered `submitted` in 182 ms. The wake
      arrived as a `SYSTEM_MESSAGE` with `sender=system` and opened a turn, the guard was consumed
      12.7 s later, and the woken model fetched its inbox and replied. The helper submitted 10.1 s
      after the message was created, which is the settle period.
    - `--no-wake` stopped the helper 0.12 s after the command ran. When `agy` closed, the helper
      exited by itself 0.46 s later.
    - `QueryFullProcessImageName` resolved `C:\Users\<user>\AppData\Local\agy\bin\agy.exe` from
      the `agy` ancestor.
13. **Approval on Windows.** agy's safety monitor flagged `BypassSandbox: true` at severity 6 on
    every run. It asked the user even when a persistent `command(...)` rule matched, and its
    confirmation appeared 35–45 s after the model's request. Choosing "always allow" in that
    confirmation wrote `command(<iris-path> watch --client antigravity --wake)` into
    `settings.json`; agy did not write it otherwise.
14. **Refused Language Server on Windows.** A refused connection reads `connectex: No connection
    could be made because the target machine actively refused it.` where Unix prints
    `connect: connection refused`.
16. **Print mode was asked too** (2026-11-14, iris 7850676, agy 1.5.8, marker `default`). holzera
    sent "reply with exactly PONG" through the delegation bridge's `agy -p`, and `Stop` asked
    anyway.
    - Without `--dangerously-skip-permissions`, agy denied the `run_command` and the answer stayed
      `PONG`. The `arm_requested` record stayed behind for an agy process that had exited.
    - With it, the command ran, a helper armed for a headless process, and the delegation
      returned `PONG\nPONG`.

    WSL then measured what could tell the modes apart. It used a scratch workspace with capture
    hooks, and Iris isolated by a scratch token path and a refused hub port.
    - **The hook sees no mode.** `Stop`, `PreInvocation` and `PostInvocation` have the same keys
      under `-p`, a prompt piped into a bare `agy`, and `-i` in a terminal. The three `Stop`
      captures are `stop_print.json`, `stop_piped.json` and `stop_interactive.json`. The hook
      environment differs only in `ANTIGRAVITY_CONVERSATION_ID` and the terminal's own variables.
    - **The grammar.** agy's own parse errors, which start no session, show Go's flag rules:
      - `-x` and `--x` are the same, and `=value` works.
      - A flag that takes a value takes the next argument even when it starts with `-`.
      - The booleans are `-c`/`--continue`, `--dangerously-skip-permissions`,
        `--disable-slash-commands`, `--new-project`, `--remote-agent`, `--remote-control` and
        `--sandbox`. Every other root flag takes a value, `-p`/`--print`/`--prompt` and
        `-i`/`--prompt-interactive` among them.
      - agy refuses a positional prompt ("Prompts are read only from -p/--print,
        -i/--prompt-interactive, or stdin"), so its only positional arguments are subcommands,
        `acp` among them.
    - **A piped prompt runs headless without a print flag.** `echo … | agy --model …` printed
      `PONG` and exited.
    - **`-i` needs a terminal.** Without one, bubbletea fails to open `/dev/tty` and agy exits
      before any hook runs.
    - **Where the input points.** In the interactive TUI, agy's fd 0 was `/dev/pts/N`, and the hook
      had a controlling terminal. In the headless runs, fd 0 was `/dev/null` or a pipe. agy points
      its own fd 1 and 2 at its log file in every mode.
    - **Workspace trust.** In an untrusted workspace the `-i` prompt ran behind the trust dialog
      with only the user's hooks. The workspace hooks loaded after trust was given.

## Decision

### Default-on configuration, three-state marker, no automatic command allow rule, and teardown

Following live verification on an interactive own-session `agy 1.5.8` run on Linux/WSL2 on
2026-11-13 (`Stop` armed the detached helper after one interactive approval prompt, `agy agentapi
send-message` submitted the fixed Iris notification in `124 ms` at `21:18:01Z` and started a turn
with zero human input, the wake guard was consumed, and the helper stayed live across a second
consecutive idle wake at `21:19:28Z`) and the owner's direction of 2026-11-13 at about 20:20 UTC
(native wake is on by default for every CLI whose implementation is complete and verified live,
while `--no-wake` disables it and reintegration preserves an explicit choice), **wake for
Antigravity is on by default when using the OS account's default Iris configuration directory**.

Because Antigravity's `permissions.allow` matcher cannot enforce whole-invocation matching without
permitting environment assignments or appended arguments (measurement 8), **Iris still writes no
`command(...)` allow rule to `settings.json`**. `settings.json` retains only `mcp(iris/*)`, so
moving `<iris-path>` leaves no orphaned Iris `command(...)` permission rule behind (`iris integrate
antigravity` updates hook entries by marker as before). Turning wake on by default therefore carries
an explicit interactive cost: **one approval prompt per armed conversation** (and again after
`/clear`, which starts a new `conversationId`). Iris offers no flag to write a `command(...)` allow
rule automatically.

To follow the fleet three-state rule (`[decided 20261113-1]`: **explicit on**, **explicit off**,
**no choice**) so that a marker written by default is never mistaken on disk for an explicit
`--wake` or `--no-wake` choice, `antigravity-wake` beside `hub.token` records three distinct values:

- **`default` (no choice, wake active):** written by a fresh `iris integrate antigravity`,
  `iris integrate --all` or `iris setup` when the resolved Iris configuration directory matches the
  OS account's pinned default directory and no explicit `on` or `off` marker is present. When
  environment overrides resolve a different directory, plain `iris integrate antigravity` and
  `iris setup` install hooks, MCP and `mcp(iris/*)` without writing `antigravity-wake`, because the
  hardened helper runs only against the OS account's default directory.
- **`on` (explicit on, wake active):** written by `iris integrate antigravity --wake`, refusing
  (with both paths named) unless the resolved Iris configuration directory matches the OS account's
  pinned default directory. Plain `iris integrate antigravity` and `iris setup` keep `on` untouched.
- **`off` (explicit off, wake disabled):** written by `iris integrate antigravity --no-wake`, which
  also stops any live Antigravity wake helpers recorded under the helper state directory (verifying
  `(helper_pid, helper_start_ms)` via `proc.Lookup` before terminating, then conditionally removing
  their owned records), and always also writes `off` and stops recorded helpers in the pinned
  default directory idempotently when the environment resolves another location. Plain
  `iris integrate antigravity` and `iris setup` keep `off` untouched.
- **Missing marker (unintegrated or removed, wake disabled):** evaluates `false` (fail-closed).
  Unrecognized text in `antigravity-wake` also evaluates `false` in hooks and helpers, is reported
  as `ErrBroken` by plain `Install` and `Drift`, and is left untouched on `--remove`.
- **Mapping of prior installations:**
  - An absent `antigravity-wake` from an earlier opt-in-era plain `iris integrate antigravity` or
    `iris setup` maps to **no choice**: wake stays off while the file is absent, `iris doctor`
    reports configuration drift on `antigravity-wake`, and running `iris integrate antigravity` or
    `iris setup` writes `default`, turning wake on.
  - An existing `on` written by `iris integrate antigravity --wake` maps to **explicit on** and is
    kept as `on` with zero drift.
  - An existing `off` written by `iris integrate antigravity --no-wake` (or written to an untracked
    or split pinned directory by an earlier `--remove`) maps to **explicit off** and is kept as
    `off` until `--wake` (or `--remove` followed by a fresh `integrate`).
- `iris integrate --remove antigravity` removes Iris's wake marker (`default`, `on` or `off`) and
  likewise stops any live recorded Antigravity wake helpers (and idempotently writes `off` and stops
  recorded helpers in the pinned default directory when the environment differs or `StateDir` did
  not track the marker), alongside removing `mcp(iris/*)`, hooks and the MCP entry.

### Per-process state, authoritative current session, and `Stop` arming request

Per-process Antigravity state lives in a `0700` directory beside `sessions/` (mode `0600` files,
owner-only DACL on Windows), outside `sessions/` so generation pruning does not touch active
records. One state file per `(agy pid, start_ms)` of the nearest `agy` ancestor (`proc.Nearest`),
coordinated under a local file lock within the 50 ms hook budget and without hub calls, holds:

- `current_session_id` and a bounded `superseded_session_ids` ring: `SessionStart` is the
  authoritative conversation activation — when `SessionStart` runs with
  `ev.SessionID != current_session_id`, it moves any prior `current_session_id` into
  `superseded_session_ids`, removes `ev.SessionID` from `superseded_session_ids` (supporting
  explicit `--conversation` resume), and sets `current_session_id = ev.SessionID`. Non-`SessionStart`
  hooks (`PreInvocation`, `PreToolUse`, `PostToolUse`, `PostInvocation` and `Stop`) ignore any
  `ev.SessionID` present in `superseded_session_ids` so a late trailing hook from a retired
  conversation cannot overwrite `current_session_id` or arm a helper after `/clear` (while an unseen
  `ev.SessionID` if `SessionStart` was skipped retires the prior `current_session_id` into
  `superseded_session_ids` and advances `current_session_id`);
- `helper`: either a live entry `(helper_pid, helper_start_ms, session_id, attempt_id)`, an
  `arm_requested(session_id)` marker, or a `failed` latch.

Only when the hook's Iris configuration directory matches the OS account's pinned default directory
and the local wake marker enables wake (`on` or `default`) does `Stop` consider asking for the
helper, and only when every gate holds:

- `fullyIdle` is `true`;
- `terminationReason` is `"NO_TOOL_CALL"` (shutdown, cancellation, errors and missing reasons never
  ask);
- `executionNum` is `0` and `continuationActive` is `false`, enforcing at most one ask per turn
  despite the `agy 1.5.8` `continuationActive` drift and staying valid if a future `agy` sets
  `continuationActive: true`;
- the nearest `agy` ancestor `(pid, start_ms)` is known, `<iris-path>` contains no newline, control
  or `"` characters (otherwise fail open without asking), the `(agy pid, start_ms)` record is not
  marked `failed` or `arm_requested(conversationId)`, and no live process matching
  `(helper_pid, helper_start_ms)` for this `conversationId` exists (`proc.Lookup`, rejecting dead
  and zombie states);
- the `agy` ancestor runs an interactive session
  ([ADR 031](031-wake-only-in-interactive-sessions.md), measurement 16). Its arguments, read from
  the process and parsed by agy's grammar, must have no `-p`/`--print`/`--prompt`, no subcommand
  and no flag Iris does not know. Then either `-i`/`--prompt-interactive` is present, or the
  process reads a terminal. Otherwise `Stop` records no request, writes
  `wake not asked: <reason>` to the observed log, and allows.

If `Stop` (`fullyIdle: true`, `terminationReason: "NO_TOOL_CALL"`) instead sees
`arm_requested(conversationId)` with still no live helper for `conversationId` — covering a model
that ignored the earlier request without calling `run_command` — `Stop` latches `failed` and allows
(`{}`). When all arming gates pass, `Stop` records `arm_requested(conversationId)` (terminating any
superseded live helper from an earlier `session_id` first) and returns
`{"decision":"continue","reason":"..."}` asking the model once to run
`<quoted-iris-path> watch --client antigravity --wake` synchronously via `run_command` (not
`IsDaemon`) with `BypassSandbox: true` (when `run_command` exposes `BypassSandbox`), quoting
`<iris-path>` for the platform shell (`sh` single-quoting on Unix; `cmd` double-quoting on Windows)
when it contains whitespace or shell metacharacters. Any read, lock or parse failure in `Stop`
allows (`{}` with exit `0`, D3).

### Hardened helper (`iris watch --client antigravity --wake`)

Even if a user persists a `command(...)` rule of their own in Antigravity's TUI, the helper never
trusts caller-supplied arguments, executable paths or Iris configuration overrides:

1. **Exact arguments.** `iris watch` for Antigravity wake requires `os.Args[1:]` to equal
   `["watch", "--client", "antigravity", "--wake"]`; any extra flag or positional argument exits
   `1` before detaching.
2. **Pinned Iris configuration.** Both parent and detached child ignore `IRIS_HUB_URL`,
   `IRIS_TOKEN_FILE`, `IRIS_EXECUTION`, `XDG_CONFIG_HOME`, `HOME`, `USERPROFILE` and `APPDATA`
   environment variables, resolving the default Iris configuration directory from OS user account
   records on Unix (`user.LookupId` for `os.Getuid()`, without `$HOME` fallback) and the process
   token's user profile directory (`GetUserProfileDirectory` -> `<profile>\AppData\Roaming\iris`)
   on Windows, and reading `client.json` and `hub.token` only from that directory so a modified
   command environment cannot redirect the hub URL, bearer token path, wake marker or session
   directory.
3. **Executable from the `agy` ancestor process only.** The helper never reads
   `ANTIGRAVITY_AGENTAPI_EXE` or `PATH`. It takes the executable path exclusively from the verified
   host `agy` ancestor `(pid, start_ms)`:
   - on Linux, `os.Readlink("/proc/<pid>/exe")` (rejecting `" (deleted)"`), verified via `os.Stat`
     as an absolute path to an existing regular file;
   - on Windows (`QueryFullProcessImageName` in `internal/proc`, measured live, measurement 12)
     and macOS (`proc_pidpath` / `kern.procargs2` executable path, unmeasured live for
     Antigravity wake), resolved from the `agy` ancestor process and verified as an absolute
     existing regular file, failing closed otherwise.
4. **Loopback Language Server check.** Require non-empty `ANTIGRAVITY_CONVERSATION_ID` and
   `ANTIGRAVITY_CSRF_TOKEN`, require `ANTIGRAVITY_CONVERSATION_ID` to match the hook-published
   `current_session_id` for `(agy pid, start_ms)`, and validate `ANTIGRAVITY_LS_ADDRESS` as a
   loopback `host:port` reachable by a short TCP dial. On `agy 1.5.8`, Antigravity binds its
   Language Server listener on `127.0.0.1:<port>` while exporting `ANTIGRAVITY_LS_ADDRESS=localhost:<port>`
   to `run_command` shells (measured live in `-i` mode); the helper accepts literal `127.0.0.1` or
   `::1` as well as `localhost`, dialing literal `127.0.0.1:<port>` and `[::1]:<port>` directly
   without DNS or `/etc/hosts` and passing the verified literal loopback `host:port` onward.
5. **Minimal scrubbed child environment and secret handling.** Both the parent and the detached
   child call `debug.SetTraceback("none")`. Because Go evaluates `GOTRACEBACK` before `main()` and
   `debug.SetTraceback` cannot lower a level inherited from the environment, an inherited
   `GOTRACEBACK` in `run_command` still governs the short-lived parent launcher until it exits; the
   parent omits `GOTRACEBACK` when constructing the explicit, minimal `Env` for both the detached
   `iris` child and `<agy-exe> agentapi send-message` — carrying only
   `ANTIGRAVITY_CONVERSATION_ID`, `ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_CSRF_TOKEN` and minimal
   trusted OS runtime variables (`SYSTEMROOT` on Windows, OS-account `HOME` on Unix), while
   stripping `LD_*`, `DYLD_*`, `GOTRACEBACK` and all other caller environment variables — so
   `debug.SetTraceback("none")` governs the detached helper from runtime startup. Iris never writes
   `ANTIGRAVITY_CSRF_TOKEN` to disk, logs or `argv` and never logs raw subprocess stdout or stderr.

If preflight passes and `(agy pid, start_ms)` already holds a live `(helper_pid, helper_start_ms)`
for this `conversationId`, the parent preserves that live entry untouched and exits `0`. Otherwise,
within a bounded 2-second startup budget, the parent generates a 128-bit `attempt_id`, spawns the
detached child in a new session (`stdin`, `stdout`, `stderr` on `os.DevNull`), looks up
`(helper_pid, helper_start_ms)` via `proc.Lookup`, durably writes and syncs
`(helper_pid, helper_start_ms, session_id, attempt_id)` under the helper lock (replacing only a
missing, dead, `arm_requested`, `failed` or superseded-`session_id` entry, never a live helper for
the current `conversationId`), re-verifies `proc.Lookup`, and only then exits `0`.

On Windows the child is created under the verified `agy` process instead of the launcher, through
`PROC_THREAD_ATTRIBUTE_PARENT_PROCESS`. agy runs each command in a job that ends with the command
and forbids breakaway (measurements 9–11), and the child must outlive the command.
- The launcher opens `agy` only for the spawn, with `PROCESS_CREATE_PROCESS` and
  `PROCESS_DUP_HANDLE`. On that same handle it checks `start_ms` and that `agy` is still running,
  and it releases the handle after the spawn.
- A refusal fails the spawn and latches `failed`.
- The child takes `agy`'s job and token, but it keeps the launcher's desktop, which is the command
  sandbox's own. That is why `<agy-exe> agentapi send-message` starts without a console
  (measurement 12).
- On Unix the launcher stays the parent; the new session already separates the child.

The detached child binds only to the hook-registered Execution of that single-session `agy` process
whose `session_id` matches `ANTIGRAVITY_CONVERSATION_ID` (never registering a fallback Execution).
Before **every** binding heartbeat renewal (`Maintain`) and before **every** wake admission, the
child checks `proc.Lookup(agy_pid)` against `start_ms`, checks that the local wake marker enables
wake (`on` or `default`), and checks that the hook-published `current_session_id` for
`(agy pid, start_ms)` still equals its `ANTIGRAVITY_CONVERSATION_ID` and that the helper entry still
names its own `(helper_pid, helper_start_ms, attempt_id)`. If any check fails — `agy` exited, `--no-wake` or
`--remove` ran, or `/clear` switched conversations (even though `binding.Resolver` refresh in
`internal/binding/binding.go` would otherwise retain the cached Execution) — the helper stops at
once without renewing the old Execution's lease and without sending a wake, removes the helper
entry **only if** it still carries its own `(helper_pid, helper_start_ms, attempt_id)`, and exits.

### Conditional failure latch, ordering with `PostToolUse`, and manual re-arming

Every publish, `failed` latch and cleanup mutation on `(agy pid, start_ms)` is conditional under
the helper directory lock and **never overwrites a live helper for the current `conversationId`**:

1. **Parent writes before exit `0`.** A successful launcher writes and syncs
   `(helper_pid, helper_start_ms, session_id, attempt_id)` before exiting `0`. On any host-side
   preflight, spawn, lookup or write failure, the parent terminates its spawned child if any,
   writes `failed` **only if** no live helper already owns `(agy pid, start_ms)` for the current
   `conversationId`, and exits `1`.
2. **`PostToolUse` runs after process exit.** On `agy 1.5.8`, `PostToolUse` fires after the
   `run_command` process exits, and auto-denied commands still carry `"error": ""` (measurement 8).
   When `PostToolUse` sees a `run_command` whose unquoted command matches the exact 4-token
   `<iris-path> watch --client antigravity --wake` invocation:
   - if `(agy pid, start_ms)` holds a live `(helper_pid, helper_start_ms)` for this
     `conversationId` (`proc.Lookup`), `PostToolUse` preserves that live record (even if `error` is
     non-empty from a denied or failing duplicate call);
   - otherwise — no live helper for this `conversationId` is recorded after the command exited
     (covering permission denial in headless `-p` or interactive rejection, `BypassSandbox`
     refusal, or exit `1` inside the sandbox where `~/.config/iris` is unmounted) — `PostToolUse`
     writes `failed` for `(agy pid, start_ms)`.

Once `(agy pid, start_ms)` is marked `failed`, `Stop` stops asking for that `agy` process. A person
can re-arm without restarting `agy` by asking the session to run `<quoted-iris-path> watch --client
antigravity --wake` once via `run_command` (with `BypassSandbox: true` when sandboxed): on success,
the parent's conditional publish replaces `failed` with the new live helper entry before exiting
`0`. Running the command from an external shell outside `agy` cannot re-arm because it lacks
`ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_CSRF_TOKEN` and the `agy` ancestor.

### Hook idle state, wake quota, classification, and incremental guard scanning

Hooks track idle state and `transcript_path` per `(antigravity, conversationId)` beside the helper
state:

- `SessionStart` and `PostInvocation` keep `transcript_path` current when non-empty.
- `PreInvocation` clears idle (`false`) before each model call.
- `Stop` sets idle (`true`) only when `fullyIdle` is `true` and `Stop` is not returning
  `decision: "continue"`, recording `transcriptPath` alongside its regular-file identity and byte
  size as `(transcript_path, file_id, idle_offset)`; any other `Stop` clears idle.

Wake admission and quota go through `internal/wake` (`antigravity` state store). All hub reads,
process checks, marker reads and subprocess waits happen **outside** the shared `sessions` lock
(`snapshot -> checks/hub -> snapshot/compare -> publish` within the 50 ms file-only lock budget;
`answer` merges by immutable `attempt.ID` after the command finishes). Final admission right before
executing `<agy-exe> agentapi send-message <conversationId> <prompt>` requires:

- local wake marker enabled (`on` or `default`);
- live `agy` process `(pid, start_ms)` whose hook-published `current_session_id` equals
  `ANTIGRAVITY_CONVERSATION_ID` and whose helper record still matches this helper's
  `(helper_pid, helper_start_ms, attempt_id)`;
- local idle `true` for `(antigravity, conversationId)`;
- `<agy-exe>` from the `agy` ancestor process still an existing regular file.

Any unmet check defers (or exits if disabled/superseded) without spending wake quota. One automatic
wake per attention identity remains the limit, using the fixed, content-free Iris notification and
correlation token.

Like `internal/wake/command.go` for Codex, exit `1` is not generically retried:

- `"submitted"`: exit `0` with empty `stderr` and valid JSON `stdout` confirming `sendMessage` for
  `recipientId == conversationId`.
- `"failed"` (eligible for the shared single retry): exit `1` with empty `stderr` and valid JSON
  `stdout` whose `error` matches a measured deterministic pre-enqueue rejection (`code =
  Unauthenticated`, `code = InvalidArgument`, `code = NotFound`, or `code = Unavailable` with
  `connect: connection refused`).
- `"unknown"` (retains the outstanding guard with no retry): every other exit status, non-JSON
  output, non-empty `stderr`, mid-RPC transport reset, `DeadlineExceeded` / `Internal` / `Unknown`
  gRPC status, signal exit or 30-second command timeout.

To consume the guard without missing messages when tool or model output grows large — including
when a busy-race turn exhausts `PostInvocation` budgets and returns to `idle` before the cursor
reaches `SYSTEM_MESSAGE` (`internal/wake/engine.go`) — both `PostInvocation` and the detached
helper run **incremental, attempt-associated forward scanning** outside the `sessions` lock:

- **Cursor initialization:** when a reservation for `attempt.ID` is published, Iris records
  `(attempt_id, transcript_path, file_id, scanned_offset, skipping_line: false)` initialized from
  `idle_offset` at reservation time (or `scanned_offset: 0` if `file_id` is absent).
- **Hook + helper continuation passes:** `PostInvocation` runs one bounded pass (`<= 4 MiB` within
  the hook deadline) while an outstanding `antigravity` guard exists, and **the detached helper
  itself** runs bounded `4 MiB` continuation passes on its poll loop whenever an outstanding
  `antigravity` guard exists for its `conversationId` and `scanned_offset` trails
  `transcript_path`'s current size (continuing across ticks while the session is `idle` until the
  cursor catches up to EOF or the guard is consumed).
- **Rotation or truncation reset:** each pass opens `transcript_path` without following terminal
  symlinks (`O_NOFOLLOW` on Unix) and verifies a regular file via `fstat`. If `current_file_id !=
  file_id` (rotation/replacement) or `current_size < scanned_offset` (truncation), the pass resets
  its cursor to `(file_id: current_file_id, scanned_offset: 0, skipping_line: false)` from byte `0`.
- **Half-line budget cuts and oversized lines:** complete `\n`-terminated lines up to `64 KiB` are
  parsed and advance `scanned_offset` past `\n`. If the `4 MiB` budget (or EOF) cuts a line in half
  while `<= 64 KiB` of that line has been read, the pass stops **without** advancing
  `scanned_offset` past the start of that partial line (`skipping_line: false`), so the next `4 MiB`
  pass re-reads the line cleanly from its start once flushed. Once a single line exceeds `64 KiB`
  without `\n`, it cannot hold Iris's short fixed wake message: `skipping_line` becomes `true` and
  `scanned_offset` advances to the budget cut across `4 MiB` passes until `\n` advances
  `scanned_offset` past the newline and clears `skipping_line = false`.
- **Exact-match evidence:** a complete `<= 64 KiB` line matches only when it parses as JSON with
  `source == "SYSTEM"` and `type == "SYSTEM_MESSAGE"` whose `content` matches the
  `<SYSTEM_MESSAGE>\n[Message] timestamp=... sender=<sender> priority=...
  content=<payload>\n</SYSTEM_MESSAGE>` envelope with `<sender>` equal to `conversationId` (measured
  in headless `-p` mode on `agy 1.5.8`) or `"system"` (measured in interactive `-i` mode on
  `agy 1.5.8`) and `<payload>` equal to the outstanding attempt's whole `Message` byte-for-byte —
  never by substring match and never from `USER_INPUT` or composite steps.
- **Merging progress by `attempt.ID`:** under the short `sessions` lock, a pass result applies
  **only** if the stored outstanding guard still carries `attempt.ID == pass.attempt_id` (discarding
  stale passes after consumption or replacement). A matching pass consumes that attempt's guard; a
  non-matching pass on the same `file_id` advances `(scanned_offset, skipping_line)` monotonically
  (`pass.scanned_offset > stored.scanned_offset`, or equal offset setting `skipping_line = true`),
  or updates `(file_id, scanned_offset, skipping_line)` when the pass observed rotation or
  truncation of `stored.file_id`.

## Limits

- **No wake before arming, and one interactive approval + shell step per armed conversation.**
  Neither `SessionStart` nor `iris mcp` has the Language Server address or token, so nothing wakes a
  session before its first turn reaches `Stop` and arms the helper. Because Iris writes no
  `command(...)` allow rule, Antigravity prompts the user once per armed conversation (and again
  after `/clear`, which creates a new `conversationId`) to approve the unsandboxed helper command.
  A headless run is not asked at all ([ADR 031](031-wake-only-in-interactive-sessions.md)). Before
  that, a `-p` run was asked too: denied without `--dangerously-skip-permissions`, and run with it,
  arming a helper for a headless process and changing the run's answer (measurement 16).
- **Print mode is told from the process, not from agy.** A flag Iris does not know turns the request
  off until Iris learns it. On Linux the input check is exact. On macOS and Windows it is whether
  the hook has a terminal or console, so a headless run without a print flag whose process still
  has one is asked once: for instance, a prompt piped into agy by a person at a terminal (ADR 031).
- **Choosing "Always allow" in Antigravity weakens the sandbox for that binary.** If a user
  persists a `command(...)` allow rule themselves, Antigravity's matcher permits `VAR=value`
  assignments, `env` wrappers and extra arguments on that binary outside the sandbox (measurement
  8). Iris's exact-`argv`, ancestor-exe, pinned-config and scrubbed-`Env` checks confine what `iris
  watch` itself will do, not what Antigravity's rule syntax allows.
- **Request, not enforcement; enterprise policy may refuse sandbox bypass.** A model may ignore the
  `Stop` request or omit `BypassSandbox: true`, and enterprise policies may block unsandboxed
  commands (`unsandboxed(...)` review or disabled bypass).
- **Disabling wake cannot recall an already-queued message.** `--no-wake` and `--remove` terminate
  live helpers and block new admissions, but a `send-message` already accepted by `agy` remains in
  `agy`'s conversation queue.
- **Busy-turn race.** Hook idle recording and helper submission are not atomic. If the user starts
  a turn between them, `agy` queues the message and injects it as `<SYSTEM_MESSAGE>` into the
  active turn (measurement 2), so the model may handle it in that turn or spend an extra step. Iris
  cannot promise zero billable empty turns.
- **Same-user secret and loopback trust boundary (D12).** Iris keeps `ANTIGRAVITY_CSRF_TOKEN` out of
  its own files, logs, detached-helper crash tracebacks and `argv` (while a caller-supplied
  `GOTRACEBACK` in `run_command` still applies to the short-lived parent launcher because Go reads
  `GOTRACEBACK` before `main()`), and passes it only in the scrubbed `Env` of the helper and
  `<agy-exe> agentapi`; same-OS-user processes can still inspect `/proc/<pid>/environ` or process
  memory, and OS core dumps or swap outside Iris remain governed by the host OS.
  `ANTIGRAVITY_LS_ADDRESS` comes from `agy`'s environment and is checked only to be literal
  loopback; Iris cannot authenticate the loopback listener before `agentapi` sends the token.
- **Live `agy` process, default config location, and platform scope.** After `agy` exits or
  restarts, old Language Server addresses and tokens fail with exit `1` (measurement 5), and
  upgrading `agy` in place on Linux causes `/proc/<pid>/exe` to carry `" (deleted)"`, stopping wakes
  until `agy` restarts. Because the helper ignores `HOME`, `XDG_CONFIG_HOME`, `APPDATA`,
  `IRIS_HUB_URL` and `IRIS_TOKEN_FILE` environment overrides, non-default Iris locations configured
  only through environment variables (rather than `client.json` in the OS account's default config
  directory) cannot use Antigravity wake. Measurements ran with `agy 1.5.8` on Linux/WSL2 and on
  Windows 11 (measurements 9–14). On macOS, process-image lookup and wake delivery have not been
  measured live.
- **Windows asks every time.** On Windows, agy's safety monitor asks for confirmation of every
  arming, even when a persistent `command(...)` rule matches, and the confirmation appears 35–45 s
  after the request (measurement 13). Its "always allow" choice writes the prefix rule described
  above.
- **Windows desktop and job.** On Windows the helper lives on the command sandbox's desktop. It
  depends on two agy behaviours: that agy itself runs in no job, and that a console-less program
  can run on that desktop. Both were measured on `agy 1.5.8`, and a later agy may change either.

## Verification

Unit and integration tests at 100% statement coverage verify:

- `iris integrate antigravity` and `iris setup` default-on (`default`) installation, explicit
  `--wake` (`on`), `--no-wake` (`off`) and `--remove` three-state marker management, preservation of
  explicit `on` and `off` choices across reintegration and `setup`, absence of any Iris-written
  `command(...)` allow rule in `settings.json`, and live helper termination on `--no-wake` and
  `--remove`;
- `Stop` arming gates (`fullyIdle`, `terminationReason == "NO_TOOL_CALL"`, `executionNum == 0`,
  `!continuationActive`, wake marker `default`/`on`/`off`/missing, shell quoting / unsafe-path
  refusal, live helper check via `proc.Lookup`, and D3 fail-open on errors);
- hardened helper preflight: exact `os.Args` enforcement, rejection of `IRIS_HUB_URL` /
  `IRIS_TOKEN_FILE` / `IRIS_EXECUTION` / `XDG_CONFIG_HOME` overrides, `agy` ancestor executable
  resolution (rejecting env/`PATH` overrides and non-regular or deleted files), loopback LS check,
  and scrubbed child `Env` with no `ANTIGRAVITY_CSRF_TOKEN` on disk, in logs or in `argv`;
- conditional `(agy pid, start_ms)` ownership: parent-before-exit durable write, preservation of a
  live helper when a duplicate command fails or is denied in `PostToolUse` (including `"error":
  ""` auto-denial), single-attempt `failed` latching when no helper is live, conditional cleanup by
  `(helper_pid, helper_start_ms, attempt_id)`, and manual re-arming via `run_command`;
- hook-published `current_session_id` protected by `superseded_session_ids` against stale trailing
  hooks from retired conversations, checked before every heartbeat renewal and wake admission
  (`/clear` stops lease renewal and exits the superseded helper immediately), and enabled wake
  marker (`on` or `default`) re-verified at final admission;
- lock-free hub/subprocess execution, `GOTRACEBACK` stripped from detached-helper and `<agy-exe>`
  environments, narrow `"failed"` vs `"unknown"` classification of `agentapi send-message` exits,
  and incremental forward scanning by both `PostInvocation` and helper continuation passes —
  including when a busy-race turn exceeds `PostInvocation`'s `4 MiB` budget and returns to `idle`
  before the cursor reaches `SYSTEM_MESSAGE`, half-line `4 MiB` budget cuts, rotation/truncation
  resets, monotonic cursor merging by `attempt.ID`, and strict `SYSTEM` + `SYSTEM_MESSAGE`
  whole-message matching.

Run the full local WSL gates (`make test`, `make race`, `make cov`, `make cov-check`,
`make check-all` and `make fixtures`) before implementation review.
