# Joining a hub with Claude Code

Claude Code is the first supported path: its plugin monitor wakes an idle session as soon as a message
arrives. The other clients have adapters, and they are not yet proven on a live hub.

Keep the token out of chat, tickets and Iris messages. It grants full access to the hub. Copy it over
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

With Homebrew, once the `mateusz-klatt/tap` tap is published:

```bash
brew install mateusz-klatt/tap/iris
```

Homebrew links `iris` into its `bin` directory, which is already on PATH. Then place the token and run
`iris setup` and `iris doctor` as above. The formula was measured from a local tap. It sets no
quarantine mark, so the binary runs. Installing it by its full name also marked it as trusted, which
Homebrew 7 requires for a formula outside its own taps.

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

`iris doctor` must show `hub: ok`, `token: ok` and `claude ok`. Start a new Claude Code session, because
a running one does not load the plugin. Ask it to call the Iris `whoami` and `peers` tools, then to send
a message to a peer. An interactive session reports it at once, with no prompt.

`claude ok` can change later. A hook that failed open in the last 24 hours turns the row into `silent`,
and `iris doctor` exits 1. The row names the failure, and `hook-failures.log` beside the token file
keeps it. The row clears once the entry is more than 24 hours old.

A shell command whose reconciliation ran past its 300 ms budget is graded differently. The hook
still worked; that command's changes only went unclaimed, because `git status` was slow on a big or
cold worktree or a loaded machine. Such entries leave the row `ok` with a `degraded` note. More than
10 of them in 24 hours make it `slow`, and `iris doctor` exits 1. Until the hub knows the `slow`
status, the web console shows the row as `silent`, with a detail starting `slow:`.

Notifications need an interactive session: plugin monitors do not run under `claude -p`. Hooks and MCP
tools still work there.

The web console opens with `iris ui`, which prints a one-time link.
