# 031: Ask a model to arm wake only in an interactive session

Status: accepted on 2026-11-14 by the coordinator (holzera), who set the rule and chose the
detection below for v0.1.3. Drafted by the WSL Claude. Amends [ADR 025](025-grok-stop-monitor.md)
for Grok and [ADR 028](028-antigravity-idle-wake.md) for Antigravity, and binds
[ADR 030](030-kimi-one-shot-wake.md) for Kimi before Kimi's wake turns on by default.

## Problem

Several wakes ask the model to arm something:
- Grok's Stop asks for a monitor (ADR 025).
- Antigravity's Stop asks for a helper (ADR 028).
- Kimi's prompt hook asks for a one-shot watcher (ADR 030).
- Cursor's Stop follows up (#338).

Each hook runs in every session of its client. That includes print-mode runs started by scripts and
by the delegation bridges agents use to consult each other. holzera measured it on 2026-11-14 with
iris 7850676, through the claude-delegator bridges:

- **`agy -p`** with `--dangerously-skip-permissions` ran the arming command, armed a helper for a
  headless process and returned `PONG\nPONG`. Without that flag it added a denied step and left an
  `arm_requested` record for a process that had exited.
- **`grok -p`**, read-only, appended a paragraph about a monitor it could not start. Grok's wake has
  been on by default since v0.1.2, so every headless `grok` run on an integrated host is affected.

An arming request in a headless run changes the answer of a run that nobody watches, and its whole
point, a wake later on, cannot happen there.

## Decision

**Iris asks a model to arm wake only in an interactive session.** A print-mode, exec or
protocol-server run gets no arming request, and Iris writes the reason to the observed log as
`wake not asked: <reason>`.

### Detection

Measure first. A signal in the hook payload or the environment, proved by real captures of both
modes, would be preferred. There is none:
- On agy 1.5.8 the `Stop` payload has the same keys under `-p`, a piped prompt and `-i`.
  `internal/hook/testdata/antigravity/stop_print.json`, `stop_piped.json` and
  `stop_interactive.json` are the captures.
- The hook environment differs only in the conversation id and the terminal's own variables.
- Grok's payloads carry no such field either (grok-home-iris). Its `permissionMode` does not tell
  print mode from the TUI.

So Iris reads the verified client process, the one whose pid and start time the hooks already
record:
- **Its arguments**, through `proc.Args`: `/proc/<pid>/cmdline` on Linux, `kern.procargs2` on macOS,
  `ProcessCommandLineInformation` on Windows. The pid and start time are checked before and after
  the read. The client's own grammar parses them.
- **Whether it reads a terminal**, when the arguments leave the mode to the input, through
  `proc.ReadsTerminal`:
  - On Linux, where the process's fd 0 points. It reads the client's own input and recognises a
    terminal by its device path under `/dev`, so a terminal reached through another path counts
    as none.
  - On macOS, whether the client process has a controlling terminal: its `e_tdev` from
    `kern.proc.pid`, which is not `NODEV`.
    - The hook's own `/dev/tty` was the first choice, and it failed. Grok 1.0.77 starts its hooks
      without its terminal. An interactive `grok` on 415d9cc, run over SSH with a pty, was logged
      "input is not a terminal", while agy 1.5.9's hook opened `/dev/tty`.
    - The process's own terminal does not depend on how a client spawns its hooks.
  - On Windows, the process's own `StandardInput`, read through its PEB and duplicated into the
    hook. Only a character handle whose `GetConsoleMode` succeeds is a terminal: pipes, files and
    NUL are not. A null or invalid handle is not a terminal; failure to open, read or duplicate is
    an unreadable input. This is option A.
- **What cannot be told means no request.** That covers arguments that cannot be read, a flag Iris
  does not know, an input that cannot be read, and a hook whose nearest client process is not the
  client. A request made in a headless run changes its answer, while a request not made only delays
  wake, and the person can still ask the model to arm it.
- A terminal check alone is not enough: a print-mode run can be started from a terminal.

The delegation bridges need no help from the terminal check. They pass the prompt as `-p`
(`agy -p …`, `grok --verbatim -p …`) with standard input ignored. On Unix they also start the
child with `detached`, so it has no controlling terminal.

### Per client

| Client | Not a session | Interactive | State |
|---|---|---|---|
| Antigravity (agy 1.5.8) | `-p`, `--print`, `--prompt`; any subcommand, `acp` among them | `-i`/`--prompt-interactive`, which needs a terminal (ADR 028, measurement 16); otherwise a terminal for input, because a prompt piped into a bare `agy` runs headless | Done here |
| Grok (1.0.77) | `-p`/`--single`, `--prompt-file`, `--prompt-json`; any subcommand, `agent` among them | Otherwise a terminal for input; a first positional argument that is not a subcommand is the session's prompt | Done here |
| Kimi (2.6.1) | `-p`/`--prompt`; any subcommand, `acp` and `web` among them | A terminal for input, the client's own; see "Kimi" below | Done by amendment, before Kimi's default-on (v0.1.4) |
| Cursor (2026.11.13-eacade1) | `-p`/`--print`; any subcommand, `mcp` and `agent` among them | Otherwise a terminal for input. A positional word that is not a subcommand is a prompt. The process table also carries `--use-system-ca` and the `index.js` launcher; see "Cursor" below | Done in #338 |
| Copilot (1.0.112) | `copilot -p` | A host with canvases; see "Copilot" below | Done by amendment |
| Codex | `codex exec` | | Probably safe and unmeasured: an exec thread is `notLoaded` in the app-server daemon, so the worker defers. The probe that settles it: a `codex exec` run that receives attention before it exits, with its answer and the daemon's thread list compared. |
| Claude | `claude -p` | | The macOS probe settles it: `claude -p "reply with exactly PONG"` on an integrated Mac passes when the answer is unchanged, no `iris watch --client claude` child runs during the call, and the observed log gains nothing beyond the hooks |

The grammars, as measured:
- **agy uses Go's flag package**, shown by its own parse errors, which start no session:
  - `-x` and `--x` are the same, and `=value` works.
  - A flag that takes a value takes the next argument even when it starts with `-`.
  - A missing value and `---x` are errors.
  - agy refuses a positional prompt, so its positional arguments are subcommands.
- **Grok uses clap**, per its `--help`:
  - Long options have two dashes. Short options have one, and may be grouped (`-cp`).
  - A short option that takes a value takes the rest of its group or the next argument.
  - `--resume` and `--worktree` take a value only when one is attached or the next argument is
    not an option.
  - `--` ends the options.
  - Usage is `[PROMPT] [COMMAND]`: the first positional argument is a subcommand or the session's
    prompt, and one after the prompt is a subcommand.
- **On Windows the command line is one string.** Iris splits it by `CommandLineToArgvW`'s rules.
  Runtimes disagree about a doubled quote (`""`); Rust, which Grok is written in, reads it
  differently. So a command line holding one is refused as unreadable rather than risk hiding a
  print flag.

### Cursor

Measured on 2026-11-14 from `cursor-agent` 2026.11.13-eacade1, using `--help` and parse
errors that exit before a session, plus the argv of the already running WSL session. No new
session was started for the grammar.
- `-p` and `--print` are booleans. `--print=hi` is `unknown option`. `-pf` is print mode
  (`No prompt provided for print mode`).
- Booleans refuse `=value`: `--trust=true` is `unknown option`.
- `--output-format` without a value is `argument missing`. `--not-a-real-flag` and `-Q` are
  `unknown option`.
- `--` ends the options. A positional word that is not a subcommand is a prompt, so the
  session is interactive only when its input is a terminal.
- The CLI rejects `--use-system-ca`, but a live process's cmdline was
  `cursor-agent --use-system-ca <version>/index.js --resume`. The wrapper accepts
  `--use-system-ca` (`--use-system-ca --help` exits 0). The grammar used on that argv treats
  `--use-system-ca` as a boolean and a positional path as the launcher, not a subcommand.

### Kimi

Amended on 2026-11-14, with the coordinator's decision. WSL measured Kimi 2.6.1 in a throwaway
`KIMI_CODE_HOME`, with Iris isolated.
- **The prompt hook sees no mode.** `SessionStart` has the same keys under `-p` and in the TUI.
  `UserPromptSubmit` does too, except that the TUI's carries a `session_title`, which it sets after
  the first prompt. That is no mode flag. The captures are
  `internal/hook/testdata/kimi/userpromptsubmit_print.json` and `userpromptsubmit_tui.json`.
- **Grammar.** Kimi's parser is commander.js, read from its parse errors:
  - `-p`/`--prompt <prompt>` is print mode.
  - A required value takes the next argument even when it starts with a dash.
  - Booleans refuse `=value`. Short options group (`-cy`, `-cp x`) and take attached values
    (`-px`).
  - `-S`/`--session [id]` takes a value only when the next argument is not an option.
  - Positional arguments are subcommands; any other word is an unknown command.
- **A piped prompt is not headless.** A prompt piped into a bare `kimi` opens the TUI, so only
  `-p` runs headless.
- **Kimi blanks its own argv.** A Kimi process started with arguments renames itself, and
  `/proc/<pid>/cmdline` then reads `kimi-code` followed by an empty string for each argument. That
  happens for a `-p` run, for a TUI started with flags, and for a long-running session alike. Only
  a bare `kimi` keeps `kimi`.
  - That exact form, argv[0] `kimi-code` and every other entry empty, means "flags unreadable,
    decide by the input". Any other unreadable or unknown argv still means no request.
  - Readable argv is parsed by the grammar above, and `-p` or a subcommand excludes it.
- **Kimi starts its hooks without its terminal**, so the hook's own terminal says nothing about
  Kimi. The input check is the client's own: fd 0 on Linux, and the controlling terminal on macOS.
  - On Windows, option B checks the hook's console, so it would never ask Kimi. Kimi there depends
    on option A, the client's `StandardInput`, or on argv from the PEB command line, which is
    still to be verified on Windows.
- **The gap.** `kimi -p` typed at a terminal reads a terminal and its argv is blanked, so it is
  asked once. Its answer may then mention the watcher. The delegation bridges start Kimi
  detached, with no terminal for input, so they are not asked.

### Copilot

Amended on 2026-11-14, with the coordinator's decision.

The extension of ADR 024 is not a hook: Copilot CLI loads it into every session, a `copilot -p`
run included. Measured on WSL with Copilot CLI 1.0.112, a `copilot -p` run started a child
`iris watch --client copilot`. Its answer stayed `PONG` only because no attention arrived during
its 4 s, and a run that received mail could gain the extension's enqueued turn.

A probe extension placed beside Iris's recorded what the host gives at `joinSession`:

| | `copilot -p` | interactive TUI |
|---|---|---|
| `session.capabilities.ui.elicitation` | true | true |
| `session.capabilities.ui.canvases` | false | true |
| environment `COPILOT_LOADER_PID` | absent | present |
| the extension's own stdin is a terminal | no | no |

So the extension starts its watcher only when `session.capabilities.ui.canvases` is exactly `true`.
Anything else, including a missing value or a getter that throws, means no watcher and nothing
sent. It is not told either. The first version logged one line to the session, but the live check
on 1.0.112 showed that `copilot -p` prints a session log line into its answer: "● Iris wake not
started: not an interactive session." followed by `PONG`. So a declined session gets no log line,
and the extension writes no reason anywhere: it has no channel that stays out of a headless
answer. `COPILOT_LOADER_PID` agreed in the measurement, but it is undocumented, and the rule does
not use it.

This is a capability of the host standing in for a mode the SDK does not declare. It fails in two
directions:
- a future print mode that gains canvases would start the watcher;
- a host without canvases, possibly an ACP host, gets no wake.

## Limits

- **macOS counts a terminal, not the client's input.** A headless run with no print flag whose
  process still has a terminal is asked once: for instance, a prompt piped into agy by a person at
  a terminal. Linux and Windows check the input itself. macOS confirms that interactive agy and
  Grok are asked, and that `-p` runs are not.
- **Windows may need a shared console for a positive answer.** `GetConsoleMode` on the duplicated
  handle may succeed only when the hook shares the client's console. On agy 1.5.9 (Windows 11) the
  hook of an interactive agy gets a positive answer and is asked, and `echo … | agy` is not.
  Interactive Grok on Windows is unmeasured: on Grok 1.0.77 its agent never accepted a prompt
  (`prompt.ack_timeout`), so no turn ended. Whether its hooks share the console, and so whether it
  is asked, is unknown. Without a shared console the answer is false, so there is no ask.
- **The Windows check targets windows/amd64 and windows/arm64.** A WOW64 target may make the PEB
  read fail, which means no ask.
- **A flag Iris does not know turns the request off** until Iris learns it. A client release that
  adds a flag disables the request for sessions that use it, and the observed log says
  `wake not asked: unknown argument`.
- **A process can rewrite its own arguments.** Neither agy nor Grok does. A client that did would be
  read by what it wrote.
- **Some interactive forms are not asked.** Grok's `dashboard` subcommand opens a TUI, but a
  subcommand is not asked. An agy session whose input was redirected while it still runs its TUI
  through `/dev/tty` is not asked on Linux.

## Verification

- Tests cover every flag form of both grammars, including `=value` and text after `--`. They also
  cover the reasons, and the Stop of both clients from real captures:
  - a print-mode ancestor gets no `continue` or `block` and no `arm_requested` record;
  - an interactive one is asked once.
- Live checks:
  - WSL: a delegated `agy -p` and `grok -p` return their answers unchanged and leave no record.
  - antigravity-home-iris: its interactive session still arms once.
  - Windows, agy 1.5.9: an interactive agy still arms once, and a bare agy with piped input is not
    asked. Interactive Grok is unmeasured there: on Grok 1.0.77 its agent did not accept a prompt.
  - macOS: the controls above, on the branch's build.
