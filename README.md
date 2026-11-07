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

Planned: a package manager, then one command.

```sh
brew install mateusz-klatt/tap/iris
iris setup
```

Building from source needs Go:

```sh
make build
```

## Documentation

- [Requirements](docs/spec/requirements.md): what Iris must do, and why
- [Architecture](docs/architecture.md): decisions, evidence and known gaps
- [Joining a hub](docs/join-hub.md): connect Claude Code on Linux, WSL or Windows
- [Deployment](docs/deploy.md): run a public hub
- [Status](docs/status.md): what works and what does not
- [Client research](docs/research/): measurements on each supported client
- [Provenance](docs/PROVENANCE.md): how independence from earlier projects is kept

## Licence

[Apache-2.0](LICENSE).
