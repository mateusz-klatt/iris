# Iris wake of a loaded Copilot CLI session

2026-11-12, Linux under WSL 2 (`Linux 6.6.87.2-microsoft-standard-WSL2 x86_64`), Copilot CLI
1.0.109. This measures [ADR 024](../../adr/024-copilot-extension-wake.md) with the real extension
loader, the real `iris watch` and a real model turn. No part of the Copilot SDK was replaced.

## Setup and boundaries

Each run used its own `COPILOT_HOME`, a throwaway git repository as the workspace, and a private
hub on `127.0.0.1` with its own token and database. Copilot ran in a terminal multiplexer as
`copilot --experimental --model gpt-6-luna --reasoning-effort low`, with the workspace trusted from
its folder-trust prompt. A second Iris identity, reached through `iris mcp --client claude`, sent
the mail. `iris integrate copilot` installed the extension, with the test build first on `PATH`.

Two builds were measured. The first run used a build of a working tree before Iris began marking
the extension files it writes (see "Installer"). The final run used a `CGO_ENABLED=0` build of this
change on top of `2c1cfe0`, after the extension's entry file was reduced to the call that starts
the watcher. A column below says which run each row comes from.

The model was real, so these runs spent credits. A woken turn is the model's own work; its cost
was not isolated from the rest of the session.

## Results

| Case | Run | Observed |
|---|---|---|
| Extension loads | first, final | After the folder-trust prompt, with `--experimental`, the extension host started the extension and the extension started `iris watch --client copilot` as its child. |
| Session identity | first, final | The watcher received the joined session's id as `COPILOT_AGENT_SESSION_ID`. It registered that session's Execution itself, with no hook having run. The Execution's client pid is the extension host's. |
| Idle wake | first, final | The session's event log recorded a `user.message` with source `automation-iris` about 0.12 s after the helper that sends the mail was started. The model then called the inbox and claims tools. |
| Further wakes | first | Later wakes after idle worked in the same session. |
| Busy session | first, final | Mail sent in the middle of a turn did not interrupt it. The wake message was recorded 145 ms (first) and 132 ms (final) after the turn ended. |
| Coalescing | first | Two wakes 30 s apart (22:28:23 and 22:28:53 UTC) came from `iris watch`'s window: the second reported more deliveries than the first, and the extension adds no delay of its own. |
| `/clear` | first | The extension and its watcher reloaded. The earlier Execution was `ended` and a new one was registered under the same Agent. |
| Resume with unread mail | first, final | `copilot --continue` kept the session id and registered a new Execution generation. The unread mail was woken, right after a `memory` user message with no content (2 to 7 ms later). In the final run that was about 15 s after the extension joined; the cause of that delay was not isolated. |
| Graceful exit | first, final | `/exit` left no extension host or `iris watch` process within about 2 s. |
| `kill -9` of the CLI | first | The extension host exited within about 2 s and the orphaned watcher within 10 s. Whether the hub then ended the Execution that fast was not measured separately. |
| Environment | first | The extension inherited the CLI's environment, including `IRIS_*`, `XDG_CONFIG_HOME` and token-bearing variables such as `GH_TOKEN`. The SDK documentation says sensitive variables are removed; that was not observed. |

## The woken turn

A wake ran two or three assistant turns: one with calls to the inbox and claims tools, one with
the reply, and in two wakes of the final run an acknowledgement of the delivery in between. The
prompt asks only to look at inbox and claims, and Iris acknowledges nothing, yet the model chose
to acknowledge a delivery as handled in those two wakes. In the first run's wakes it left the
mail unacknowledged. Iris cannot make that choice for the model.

The wake message did not appear as a user message in the Copilot timeline. The model's tool calls
and its reply did.

## Installer

On the last build measured: a first install wrote both extension files with a header that carries
the SHA-256 of the rest of the file. A second install changed nothing. An install from another
`iris` path rewrote the extension file with the new path. A file edited by hand was refused with a
message that names it, exit status 1. `iris integrate --remove copilot` then deleted the file that
was unchanged and kept the edited one, and an install after restoring it succeeded.

A change made after review has that removal also name the kept file and exit with status 1 until
the file is moved aside. Tests cover it; it was not measured live.

The first run's build left an extension file behind after a removal when `iris` was found at a
different path than at installation. That is the reason for the header.

## Not measured

- Copilot's command sandbox. `sandbox.sandboxExtensions` defaults to true and applies when the
  command sandbox is on. The host lacked `slirp4netns`, which the Linux sandbox needs, so
  whether `iris watch` can then reach the hub is unknown.
- macOS and Windows, and any Copilot CLI version other than 1.0.109.
- Non-interactive `copilot -p`. The CLI documentation says it loads a user's extensions, so the
  extension would also start `iris watch` there.
- Reporting through `session.log` with a live CLI: monitor start failure, an unexpected exit, a
  refused send and the monitor's status lines are covered by the extension's own tests only.
- The cost of a wake in credits.
