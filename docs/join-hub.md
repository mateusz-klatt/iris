# Joining a hub with Claude Code

Claude Code is the first supported path: its plugin monitor wakes an idle session as soon as a message
arrives. The other clients have adapters, and they are not yet proven on a live hub.

Keep the token out of chat, tickets and Iris messages. It grants full access to the hub. Copy it over
SSH, or by hand.

## Linux and WSL

```bash
git clone https://github.com/mateusz-klatt/iris.git && cd iris
mkdir -p ~/.local/bin ~/.config/iris
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

```bash
git clone https://github.com/mateusz-klatt/iris.git && cd iris
CONFIG="$HOME/Library/Application Support/iris"
mkdir -p ~/.local/bin "$CONFIG"
CGO_ENABLED=0 go build -o ~/.local/bin/iris ./cmd/iris
scp <user>@<hub-host>:.config/iris/hub.token "$CONFIG/hub.token"
chmod 600 "$CONFIG/hub.token"
export PATH="$HOME/.local/bin:$PATH"
iris setup --hub https://<hub> --clients claude --yes
iris doctor
```

`~/.local/bin` is not on the default macOS `PATH`. Add the `export` line to `~/.zshrc`, because
`iris guard` runs from Git's `PATH`.

Claims compare paths exactly. The default macOS filesystem does not, so `SRC/A.GO` and `src/a.go`
are one file but two claims, and neither the hooks nor `iris guard` see a conflict between them. The
same applies to a name written in two Unicode forms. Spell paths as Git does.

### Running the hub on this Mac

With a loopback `--hub`, `iris setup` installs a LaunchAgent at `~/Library/LaunchAgents/iris.plist`.
`iris service status` reports `active: true` only while the hub process runs. launchd restarts a hub
that exits, so a job can stay loaded and be inactive.

The hub writes to `~/Library/Logs/iris/hub.log`. When `iris service install` or `iris service start`
finds that file at 10 MiB or more, it moves it to `hub.log.1`, replacing the previous one. Nothing
limits the log between those commands, because launchd has no rotation and keeps the file open. The
hub writes little, but a hub that keeps failing adds lines at each restart, which launchd spaces about
10 seconds apart. `iris service uninstall` leaves the logs in place.

## Windows

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

Notifications need an interactive session: plugin monitors do not run under `claude -p`. Hooks and MCP
tools still work there.

The web console opens with `iris ui`, which prints a one-time link.
