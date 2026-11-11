# Iris

Iris coordinates coding agents that work at the same time. Sessions from different CLIs, on
different machines and in different worktrees of one repository can see each other, exchange
durable messages, and claim files before editing them. A human watches and steers it all from
one web UI.

**Status: pre-release.** One hub is in daily use by agents on macOS, Linux under WSL and Windows.
Nothing is released yet. See [Status](docs/status.md) for what is measured and what is not.

It supports Claude Code, Codex CLI, Antigravity CLI, Kimi Code, Grok CLI, Cursor Agent and
GitHub Copilot CLI on Linux, macOS, Windows and WSL.

## How it works

One Go binary plays every role. `iris serve` is the hub: it owns a SQLite database and serves
an HTTP API and the web UI. Each agent session reaches it three ways: hooks act automatically
before edits, MCP tools let the model act explicitly, and `iris watch` carries attention
signals. On Claude Code a new message wakes an idle session in about a third of a second. A git
pre-commit guard is the last checkpoint.

Iris coordinates cooperating agents. It prevents and reports collisions; it is not a sandbox.

## Installing

Nothing is released yet, and no package channel is published. Build from source for now;
release archives are the intended first installation route once a release is published.
See [Snapshots and releases](docs/release.md) for the prepared artifacts and their limits.

Building the binary with the web console needs Go 1.27.4 or newer, Node.js 26 and npm,
and Make with a POSIX shell and utilities (Git Bash on Windows). From the repository root:

```sh
make build
```

Copy the resulting `iris` binary to a stable directory on your `PATH`, then run:

```sh
iris setup
iris doctor
```

Setup shows the hub and client configuration before writing it. To join an existing hub,
follow [Joining a hub](docs/join-hub.md) for the token location and platform-specific steps.
`make build` rebuilds the console before compiling the binary. Generated console files
are ignored, so building a clean checkout leaves its source state clean without cleanup.
The local version label is `dev` with the commit and source state; `make build VERSION=...`
overrides the label. Node.js is only needed to build the console, not to run Iris.

Plain `go build ./...` and `go test ./...` also work without Node. On a fresh checkout,
that binary reports `console missing` in `iris version` and serves build instructions
with HTTP 503 at console routes. If an index was built earlier, plain Go builds embed it
and report `console unverified`: this does not prove its assets are complete or current.

Open the hub's URL in a browser to use the console (`http://127.0.0.1:7878/` by
default for a local hub), then sign in with a named account. The owner must first
[activate the console and restart the hub](docs/console-activation.md#owner-setup-and-activation).

## Documentation

- [Requirements](docs/spec/requirements.md): what Iris must do, and why
- [Architecture](docs/architecture.md): decisions, evidence and known gaps
- [Joining a hub](docs/join-hub.md): connect Claude Code on Linux, WSL or Windows
- [Deployment](docs/deploy.md): run a public hub
- [Status](docs/status.md): what works and what does not
- [Client research](docs/research/): measurements on each supported client

## Licence

[Apache-2.0](LICENSE).
