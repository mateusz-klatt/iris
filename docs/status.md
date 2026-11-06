# Status

Last updated 2026-11-06. Nothing is released.

## First milestone

From `docs/spec/requirements.md`:

| Step | State | Evidence |
|---|---|---|
| Two sessions register and see each other | Done on Linux | Claude Code and Copilot live. Windows: tests only |
| A message is received and acknowledged, and the sender sees it | Done on Linux | Live, across vendors |
| A claimed file is refused before the edit | Done on Linux | Live, across vendors. Deny proved in all seven clients' hooks and in a regression test for each |
| Dropped connection recovers every event in order | Done in tests | Reconnect during a burst, no loss and no duplicates |
| Hook latency measured on three systems | Linux only | p50 27.6 ms, p95 42.6 ms with a durable write |

Linux, macOS and Windows all pass the test suite in CI, and six targets cross-build.

## What exists

`iris serve`, `hook`, `mcp`, `watch`, `ui`, `integrate`, `setup`, `doctor`, `service`, and the
operator console.

## Not done yet

- `iris guard` and reconciliation after shell edits (D12), so shell edits are unprotected.
- A real client on Windows or macOS.
- `iris integrate` against real clients: the Claude plugin install, Codex's `/hooks` review, and
  whether Antigravity loads a user-level hook file.
- The Claude plugin monitor with the real `iris watch`. The mechanism is measured, the pairing is not.
- Reading `hook-failures.log` in `iris doctor`.
- Release pipeline: GoReleaser configuration, package channels, signing.
- Translations of the new console strings, and layout polish.
- Legal review of the clean-room record.
