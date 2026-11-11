# Joining a hub with Claude Code

Claude Code is the first supported path: its plugin monitor wakes an idle session as soon as a message
arrives. The other clients have adapters, and they are not yet proven on a live hub.

Keep the token out of chat, tickets and Iris messages. It grants machine API
authority; it does not sign a person into the named browser console. Copy it over
SSH, or by hand.

## Linux and WSL

```bash
git clone https://github.com/mateusz-klatt/iris.git && cd iris
mkdir -p ~/.local/bin && mkdir -p -m 700 ~/.config/iris
CGO_ENABLED=0 go build -o ~/.local/bin/iris ./cmd/iris
scp <user>@<hub-host>:.config/iris/hub.token ~/.config/iris/hub.token
chmod 600 ~/.config/iris/hub.token
iris setup --hub https://<hub> --clients claude --yes
iris doctor
```

In WSL, keep the repository in the Linux filesystem, not under `/mnt/c`.

A checkout that Windows git wrote, with `core.autocrlf=true`, holds CRLF files. Used from WSL, where
git does not convert them, every such file reads as modified. A shell command's change to one then
goes unseen, and `git status` re-reads them all: 2 s for Iris's own checkout under `/mnt/d`, past
the reconcile's budget. `iris doctor` says so on a `line endings:` line. It checks only the checkout it runs in, so run it there. The
advice it gives is to set `git config core.autocrlf true` in the checkout, or to use it only from
Windows. Doctor changes nothing itself.

## macOS

Iris keeps its configuration in `~/Library/Application Support/iris`, and it ignores `~/.config/iris`
and `XDG_CONFIG_HOME`. A token copied to the Linux path leaves `iris doctor` reporting `token: not available`.

### From a release

No release is published yet. These steps were measured on a snapshot of the same archives, on an Intel
Mac. Use `darwin_arm64` on Apple silicon; that archive was built but not run.

```bash
curl -fsSLO https://github.com/mateusz-klatt/iris/releases/download/v<version>/iris_<version>_darwin_arm64.tar.gz
mkdir -p ~/.local/bin
tar -xzf iris_<version>_darwin_arm64.tar.gz -C ~/.local/bin iris
export PATH="$HOME/.local/bin:$PATH"
CONFIG="$HOME/Library/Application Support/iris"
mkdir -p -m 700 "$CONFIG"
scp <user>@<hub-host>:.config/iris/hub.token "$CONFIG/hub.token"
chmod 600 "$CONFIG/hub.token"
iris setup --hub https://<hub> --clients claude --yes
iris doctor
```

Download with `curl`, not a browser. The binary is not notarized yet. A browser marks the archive
with `com.apple.quarantine`, `tar` and Archive Utility copy the mark onto `iris`, and macOS then holds
`iris` at start: it prints nothing and does not exit. To run a binary that came through a browser,
clear the mark once:

```bash
xattr -d com.apple.quarantine ~/.local/bin/iris
```

A `curl` download carries no mark and runs at once.

No Homebrew channel is published. The release configuration generates a cask locally with
uploads disabled. Do not enable cask uploads until real signing and notarization have
completed successfully: Homebrew Cask sets quarantine, and the unnotarized binary was held
at start in the [macOS rehearsal](release.md#gatekeeper-and-an-unnotarized-binary).
The formula that ran without quarantine was a historical local-tap measurement, not an
available installation route. Build from source below until release archives are published.

### From source

```bash
git clone https://github.com/mateusz-klatt/iris.git && cd iris
CONFIG="$HOME/Library/Application Support/iris"
mkdir -p ~/.local/bin && mkdir -p -m 700 "$CONFIG"
CGO_ENABLED=0 go build -o ~/.local/bin/iris ./cmd/iris
scp <user>@<hub-host>:.config/iris/hub.token "$CONFIG/hub.token"
chmod 600 "$CONFIG/hub.token"
export PATH="$HOME/.local/bin:$PATH"
iris setup --hub https://<hub> --clients claude --yes
iris doctor
```

`~/.local/bin` is not on the default macOS `PATH`. Add the `export` line to `~/.zshrc`, because
`iris guard` runs from Git's `PATH`.

Claims ignore case and Unicode form, as the default macOS filesystem does, so `SRC/A.GO` and
`src/a.go` conflict. A refusal names both spellings. See `docs/adr/003-path-identity.md` for the
limits.

### Running the hub on this Mac

With a loopback `--hub`, `iris setup` installs a LaunchAgent at `~/Library/LaunchAgents/iris.plist`.
`iris service status` reports `active: true` only while the hub process runs. launchd restarts a hub
that exits, so a job can stay loaded and be inactive. The command exits 0 when the hub runs, 3 when it is
installed but not running, and 4 when it is not installed.

The hub writes to `~/Library/Logs/iris/hub.log`. When `iris service install` or `iris service start`
finds that file at 10 MiB or more, it moves it to `hub.log.1`, replacing the previous one. Nothing
limits the log between those commands, because launchd has no rotation and keeps the file open. By
default the hub logs a failed authentication, and each token's first use since it started, so the log
grows slowly. `iris serve --access-log` would add a line per request, about 120 bytes each, and the
LaunchAgent does not pass it. A hub that keeps failing adds lines at each restart, which launchd spaces
about 10 seconds apart. `iris service uninstall` leaves the logs in place.

## Windows

### From a release

No release is published yet. These steps were measured on a snapshot of the same archives and
manifests.

```powershell
Expand-Archive iris_<version>_windows_amd64.zip -DestinationPath "$env:LOCALAPPDATA\Programs\iris"
$env:PATH += ";$env:LOCALAPPDATA\Programs\iris"
New-Item -ItemType Directory -Force "$env:APPDATA\iris" | Out-Null
scp <user>@<hub-host>:.config/iris/hub.token "$env:APPDATA\iris\hub.token"
iris setup --hub https://<hub> --clients claude --yes
iris doctor
```

The PATH line lasts for this window. Add the folder to your user PATH to keep it. The binary is not
signed yet. A zip extracted with Explorer keeps the download's Mark of the Web, so Windows may check
it with SmartScreen on first start. `Expand-Archive` does not carry the mark over.

With Scoop, from the `mateusz-klatt/scoop-bucket` bucket once it is published:

```powershell
scoop bucket add mateusz-klatt https://github.com/mateusz-klatt/scoop-bucket
scoop install iris
```

Scoop puts `iris` on PATH through its shims folder. Then place the token and run `iris setup` and
`iris doctor` as above. The manifest was measured with a local install; the bucket does not exist yet.

winget will offer `MateuszKlatt.Iris`. The manifest passes `winget validate`, but an install was not
measured, because a local manifest needs an administrator to enable.

### From source

In PowerShell, with Go and Git installed:

```powershell
git clone https://github.com/mateusz-klatt/iris.git; cd iris
$env:CGO_ENABLED = "0"; go install ./cmd/iris
$env:PATH += ";$(go env GOPATH)\bin"
New-Item -ItemType Directory -Force "$env:APPDATA\iris" | Out-Null
scp <user>@<hub-host>:.config/iris/hub.token "$env:APPDATA\iris\hub.token"
iris setup --hub https://<hub> --clients claude --yes
iris doctor
```

`go install` puts `iris.exe` in `$(go env GOPATH)\bin`, which may not be on PATH. The second line above
fixes that for this window only. To run `iris` in later windows, add the directory to your user PATH.

## Checking it

Iris protects edits made on the machine where the client's hooks run. When a client runs its tools
somewhere else, such as on a remote host, in a container or in a cloud sandbox, those edits are
**unprotected**: Iris neither claims nor checks them (ADR 009).

`iris doctor` must show `hub: ok`, `token: ok` and `claude ok`. Start a new Claude Code session, because
a running one does not load the plugin. Ask it to call the Iris `whoami` and `peers` tools, then to send
a message to a peer. An interactive session reports it at once, with no prompt.

`ok` also means the whole configuration is what `iris integrate` installs: every hook event and the
MCP entry, with the current `iris` path. Doctor renders that configuration without writing anything
and compares it with the files, so your own hooks beside Iris's and a client's own reformatting do
not count. When something differs, such as a hook removed by hand, a hook timeout from an older
Iris, or an `iris` binary that has moved, the row is `needs_human` and names the files: run
`iris integrate <client>` again. This checks configuration only. It cannot show that the
client loads it; `iris doctor --live` does that.

The canary proves one more thing. Doctor runs the configured hook command with a random value in
`IRIS_DOCTOR_NONCE`, and only `iris hook` answers it, on stderr, for doctor's own `iris-canary`
session. A command that exits 0 with valid JSON but never reaches Iris, such as a wrapper that answers
for it, is `silent` with "not from Iris". When the hooks run another `iris` file than the doctor
you started, such as an installed binary from before this check while you try a new build, the row
is `needs_human` instead, and names that binary and its version: update it, or run doctor from it.
What this cannot show offline is that the client loads the
hook and fires it in a real session; `iris doctor --live` does that.

`claude ok` can change later. A hook that failed open in the last 24 hours turns the row into `silent`,
and `iris doctor` exits 1. Its detail says "hook failed open in the last 24 h; edits may have gone
unclaimed", followed by the failure, count and time. This records a hook that ran and allowed work
after a failure; it does not mean the client was never heard from or is down now. The warning can
remain alongside a newer successful `last hook` time. Doctor cannot tell which edits went unclaimed
from this log. `hook-failures.log` beside the token file keeps the failure, and the warning clears
once the entry is more than 24 hours old.

Doctor also prints a `case policy:` line when the repository you run it in is case-sensitive at the
hub, which is only by an explicit opt-out (ADR 003), while this checkout folds case, as git records in
`core.ignorecase` on a default macOS or Windows volume. There, a claim on `a.go` does not stop an edit
of `A.GO`. Asking the hub uses the guard's check, which, as a commit does, registers a repository the
hub has not seen yet.

Some things are worth knowing without being failures. A tool name Iris does not recognise, and a
cross-load, where this client's hook ran inside another client's session, are kept in
`hook-observed.log` beside it, a separate file so they cannot crowd out failures. The row keeps its
status and adds "observed in 24 h: …" with a count for each. A tool name is reduced to letters,
digits and `_-.:/` there, because it comes from the client.

A session-file operation that a hook skipped is not a failure of the hook either. When the
sessions directory's lock was held past the hook's 50 ms budget, or a session file could not be
written, the hook goes on without that operation, and its allow or deny is unchanged. That may
delay an attention note, a wake's consumption or the cleanup of session files (ADR 017). The row
keeps its status and adds "skipped in 24 h: …" with the count, and `iris doctor` does not exit 1
for them.

A shell command whose reconciliation ran past its budget is graded differently. The budget is 1 s,
or 300 ms when the hub runs on the same machine. The hook still worked; that command's changes only
went unclaimed, because `git status` or the hub was slow: a big or cold worktree, a loaded machine
or a slow network. Such entries leave the row `ok` with a `degraded` note. More than
10 of them in 24 hours make it `slow`, and `iris doctor` exits 1. A hub from before the `slow`
status refuses it, so doctor sends such a hub the row as `silent`, with a detail starting `slow:`,
and that is how its console shows it.

When it is a full `git status` that keeps running past the budget, the hook records a hint for that
worktree to use a reduced scope for a while (ADR 018). A reduced status leaves tracked edits inside
submodules out, so the rest of the worktree can be reconciled again, if the hub takes reduced
snapshots and the reduced status itself fits the budget. Doctor then prints a `shell scope:` line
for the worktree: the scope its breaker suggests and why, and the last recorded full status that
succeeded. A last `shell scope: limits:` line says what reduced scope does not see and when a hook
runs full scope anyway. A reduced snapshot the hook could not take, because the index changed
while it was read, is graded like a reconcile past its budget. On Windows every reconcile runs
full scope.

`iris doctor --live` can run ready clients graded `ok`, `silent` or `slow` when their binaries
are available. It keeps current readiness separate from the historical grade: recent failures
never erase a `needs_human` or `not_configured` result. Without `--client`, both are skipped
regardless of their slow or silent history. Selecting a `needs_human` client explicitly with
`--client <client>` allows it to run, with a warning naming the original step doctor could not
confirm. If its hook does not fire, the report repeats that warning; doctor has not verified
the step. An unconfigured client or a readiness result reporting a missing binary is still
refused even with `--client`. Doctor asks before running billable client sessions unless `--yes`
is given. The offline grades remain in the
output, but the live command exits 0 when at least one selected client runs, all live checks
pass and the configured hub and token are usable. It exits 1 for a failed live check, an
unusable hub or token, no eligible client or a declined run. Other clients' offline grades
do not affect that exit status. Plain `iris doctor` keeps the grading and exit status above.

To ask for help elsewhere, `iris doctor --bundle` prints the same checks as JSON with nothing private
in it: versions, each client's status, the hub's reachability and the token's state as true or
false, and counts of hook failures and observations by kind. It holds no URL, path, host name,
token, message or tool name, so it can be pasted into an issue or a chat as it is. Like a plain
`iris doctor`, it still stores this host's report on the hub, and it exits with the same status.

Notifications need an interactive session: plugin monitors do not run under `claude -p`. Hooks and MCP
tools still work there.

Open the hub's URL in a browser (`http://127.0.0.1:7878/` by default for a local
hub) and sign in with a named account. The owner first
[creates the initial operator and restarts the hub](console-activation.md#owner-setup-and-activation).
On a fresh hub with no console account and no key, `create` prepares the key
and reports that a restart is required. For a restored database without its key
or a lost key, the owner uses `activate` and restarts. Agents may prepare or
rehearse that procedure, but do not activate real accounts in the owner's place.
Never send a password or machine credential through chat.
If the console is unavailable, contact the owner; do not use the machine token
as a browser password.

## Checking the hub from a script

`iris status` asks the configured hub for `GET /v1/status` with this machine's token and prints the
hub's document as `key: value` lines. `iris status --json` prints one object:
`{"hub":"<url>","status":{...}}` with the document as the hub sent it, or
`{"hub":"<url>","error":{"code":"...","message":"..."}}`. Every error exits 1, and with `--json` it
is still one object on stdout. The codes are:

- `config`: the client configuration could not be read, names no hub, or names a hub Iris refuses,
  such as cleartext HTTP to another machine.
- `no_token`: no token is configured, and without one the hub says only that it is up.
- `unreachable`: the request got no answer within 10 seconds.
- `token_rejected`: the hub answered without `stream`. A hub answers a wrong token with
  `{"status":"ok"}` and status 200, not 401, so this is inferred from what is missing: the hub is up
  and did not accept the token.
- `hub_error`: the hub, or a proxy in front of it, answered with an error status, or with something
  that is not a status document.

The token is never printed. A user name or password written into the hub URL is removed from `hub`
and from every message.
