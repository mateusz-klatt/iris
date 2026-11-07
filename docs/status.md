# Status

Last updated 2026-11-07, from `main` at `8132fbb`. Nothing is released.

The hub runs at https://iris.klatt.ie behind Apache and Cloudflare (`docs/deploy.md`). It was last
deployed from `8132fbb` on 2026-11-07, and the sessions active before the deploy reattached. Agents on
three hosts use it every day: macOS, Linux under WSL, and Windows 11. Each claim below was measured
and links to its pull request or document. Anything not measured is listed under "Not done or not
measured".

## First milestone

From `docs/spec/requirements.md`:

| Step | State | Evidence |
|---|---|---|
| Two sessions register and see each other | Done on Linux, macOS and Windows | Live sessions on all three hosts list each other with `peers` on the production hub. Claude Code and Copilot on Linux earlier. |
| A message is received and acknowledged, and the sender sees it | Done across hosts | Live messages between the macOS, WSL and Windows sessions, read and acknowledged |
| A claimed file is refused before the edit | Done on Linux, macOS and Windows | Live on Linux across vendors; on APFS for every spelling in ADR 003 (#47); on NTFS (#53). Deny proved in all seven clients' hooks by a regression test for each. |
| Dropped connection recovers every event in order | Done in tests | Reconnect during a burst, no loss and no duplicates; made timing-free in #59 |
| Hook latency measured on three systems | Done | Below |

### Hook latency

CI runners, 100 pre-edit hooks each against a local hub, on `4708832`:

| System | p50 | p95 | max |
|---|---|---|---|
| Linux amd64 | 8.6 ms | 9.9 ms | 15.4 ms |
| macOS arm64 | 16.8 ms | 31.1 ms | 90.6 ms |
| Windows amd64 | 39.6 ms | 49.7 ms | 308.8 ms |

Against the production hub, over TLS:

- **macOS, Intel, 100 edits, before #39:** p50 166 ms, p95 228 ms, max 1517 ms. One hook reached the
  1.5 s deadline and failed open (#25).
- **macOS, the same host after #39's second request:** p50 207 ms, p95 390 ms, max 727 ms, with none
  over 1.3 s. Two small runs on different network conditions, so the p50 and p95 change is not
  attributed to the change.
- **WSL:** #39 measured DNS, TLS and server stalls of several seconds on `/v1/status`. With the second
  request, the p99 over 250 of those requests was 371 ms (#39). Real hooks were not re-measured there.
- **Windows 11, before #39:** four runs of 100 with ESET active and Defender off measured p50 172 to
  182 ms and p95 222 to 366 ms. Six of 400 failed open. It has not been re-measured since #39.

## Measured behaviour

- **Recovery across hub restarts.** A heartbeat reattaches `iris mcp` and `iris watch` within the
  recovery window (#28). A hook keeps its session's generation and reattaches with that proof. A
  hook without it refuses an edit rather than allowing it unclaimed (#55). With a real Claude Code
  client before #55, sessions without a bound `iris mcp` lost every edit's claim for 120 s and then
  lost their claims. After it, every edit stayed claimed by the same Execution. That was measured on
  macOS (#55) and repeated on Windows by `claude-home-iris-be60dbb0` in its review. The session files
  are owner-only on Windows too (#61).
- **Path identity.** Claims compare a key that ignores case and Unicode form (ADR 003, #47). On APFS
  every pair in the ADR's table was refused by the hook and by the guard. On NTFS, trailing dots and
  spaces and a differently cased worktree root are refused (#53). Short names, junctions and
  symlinks are listed in the ADR as not caught.
- **Per-host tokens.** `iris token add`, `list` and `revoke` (#23). Only the owner token manages
  tokens (#52) or removes records (#54). The move from the shared token was rehearsed with a live
  session and written as a runbook (`docs/deploy.md`, #62). The macOS host moved to its own token
  on 2026-11-07 by that runbook. A revoked token is now visible to the session: the hook tells the
  model, `whoami` fails, `iris watch` says so once, and the hub logs the token as revoked (#64).
- **Removing records.** `iris admin forget` removes one repository or one ended agent, with a dry
  run. Its tests include a storage-fault sweep over every statement (#54).
- **Logging.** By default the hub logs each token's first use since start and every failed
  authentication. `--access-log` adds every request (#60). The container's logs are capped at
  3 × 10 MB (#58).
- **Release rehearsal, nothing published.**
  - On Windows 11, the zip, Scoop, `winget validate`, setup, the service and a headless Claude Code
    session were measured (`docs/release.md`, #48).
  - On an Intel Mac, the archive was installed from an empty home, with launchd, a real Claude Code
    session, a Homebrew formula and cask from a local tap, and Gatekeeper on an unnotarized binary
    (`docs/release.md`, #57). Archives record no builder, and two builds of one commit were identical
    (#60).
  - The release image runs as non-root in CI, amd64 only (#46).
- **Services.** launchd install, start, stop, crash loop and log were run for real on macOS under a
  scratch home (#25, #34). The Windows Run value started the hub with a minimal PATH (#48).
- **CI.** Linux, macOS and Windows pass the test suite, six targets cross-build, and the coverage
  gate requires 100% of Go and the console. That gate passes on `main`.

## Not done or not measured

- **Other clients on real hosts.** Codex, Cursor, Antigravity, Grok and Kimi are covered only by
  captured payloads and tests. Copilot has run live on Linux only.
- **No release.** No package channel is published. Windows signing is chosen (#35) but not set up. macOS
  notarization is wired and inert until the owner's Developer ID exists (#57). Real signing and
  notarization have not run.
- **Not measured:** Defender, the SmartScreen window, Apple silicon, Gatekeeper's dialog and "Open
  Anyway", and a Mac or PC whose policy forbids unsigned code.
- **Production hook latency on Windows since #39.**
- **Services:** loading the LaunchAgent at login, and `systemd --user` running for real.
- **`iris doctor --live`.**
- **Translations** of the newer console strings, and layout polish.
- **Legal review** of the clean-room record.

## SonarCloud

The quality gate on `main` is red on `new_security_rating` alone. Its findings were reviewed and kept on
purpose:

- `PATH` lookups for `git`, `xdg-open` and the clients' binaries are how those commands are found.
- The session cookie has no `Secure` flag on loopback, where the console is served over HTTP and
  Safari would refuse a `Secure` cookie. Behind the public origin it is `Secure`.
- HTTP and WebSocket URLs on loopback, for the local hub and the console.
