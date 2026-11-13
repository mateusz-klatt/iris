# Status

Release status updated 2026-11-12. [v0.1.0](https://github.com/mateusz-klatt/iris/releases/tag/v0.1.0)
was published at 06:07 UTC from `c4983205cb320a1b4313d114c1b2ab644023e59c`. Its signed release
workflow [43117855817](https://github.com/mateusz-klatt/iris/actions/runs/43117855817) succeeded.
Package-manager uploads remain disabled. The next intended `v0.1.1` is a final release on the
owner's decision; this status does not create or authorize a tag.

The Codex wake implementation merged after v0.1.0. Its
[acceptance record](research/clients/codex-iris-idle-wake-2026-11-12.md) preserves Linux
default/custom-home daemon evidence and owner-reported macOS TUI evidence for the original
opt-in runner. [ADR 023](adr/023-default-codex-wake-after-idle-check.md) now changes the default,
requires a positive native idle check and removes the automatic second native wake. Those
earlier runs do not validate the new gate or defaults on every platform. Further native and
cost claims must name their own evidence; implementing a policy is not a new measurement.

The earlier hub latency, shared-worktree, recovery, service and release-rehearsal measurements below were
last summarized on 2026-11-08, from `main` at `efe0af6` before the repository history rewrite.
Their commit hashes and PR numbers, including the Windows diagnostic at `993e545`, are retained as
historical evidence labels; they refer to the history before the rewrite, not revisions of the
current `main`. The CI and translation notes were updated on 2026-11-09 on `main` at `655526a`.
The hub's idle CPU note was updated on 2026-11-10 on `main` at `ef67498`. The deployment paragraph
and console record were updated on 2026-11-11 on `main` at `427703f`.

The hub runs at https://iris.klatt.ie behind Apache and Cloudflare (`docs/deploy.md`). It was last
deployed from `main` at `427703f` (#266) on 2026-11-11 at 11:10:51 UTC, after a backup; it was ready
1.4 s later, with the console open and a restart count of 0. That deploy brought the hub's side of
the reduced shell scope (ADR 018, migration 022), the attention identity (migration 023) and Go
1.27.4 for GO-2026-6859. Before it, `d4cae61` (#250) was deployed on 2026-11-10 at 16:49:18 UTC for
the login cutover. Earlier deployments by the procedure that deploys an image CI built (#243,
#248) were for `cfcd86a` and `6d555c7` on 2026-11-09 at 19:32 and 19:52 UTC, and for `ef67498` on
2026-11-10 at 01:55 UTC.
Each of those runs took a backup before it replaced the container. After the `ef67498` deployment,
the container and the host CLI reported that commit with a clean source, the container had not
restarted, and `/v1/status` answered locally and through the public origin.
The host itself was restarted on 2026-11-09 at 22:14 UTC; the container stopped with exit
status 0 and Docker started it again when the host came back. Agents on macOS, Linux under WSL, and
Windows 11 use the hub every day. Evidence and its limits are identified below. Anything not
measured is listed under "Not done or not measured".

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
  182 ms and p95 222 to 366 ms. Six of 400 failed open.
- **Windows 11, after #39 (2026-11-07, `main-63024fb`, hub `bf0e899`):** one session of 100 pre-edit
  hooks, each a new `iris.exe hook` process, on a home connection with ESET active: p50 263 ms, p95
  416 ms, max 498 ms, min 171 ms. None reached the deadline, all 100 answered allow, and no failure
  was logged, so every edit got a real claim rather than the fail-open answer. A single run: the p50
  rise against the earlier runs is not attributed to #39, which only adds a second request when the
  first is slow.

## Measured behaviour

- **Claude and Codex in one worktree.** Actual concurrent clients on macOS and WSL preserved edits
  to separate files and refused native Edit/apply_patch attempts against the other session's claim
  (#208, [measurement record](research/shared-worktree.md)). WSL also demonstrated silent shell
  overwrites and commits containing both sessions' changes. In a linked-worktree control, the shell
  warned and the commit guard refused the conflicting commit. A shared worktree therefore needs one
  owner for Git operations and agreed file ownership; that is coordination, not Iris enforcement.
  Separate worktrees isolate working files, HEAD and index. These bounded runs do not establish
  freedom from Git races. The Windows observations lack captured refusal text and include a
  confounded attempt, so they are not native-contention proof in that record.
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
  - On 2026-11-11, a local snapshot at `c28c1a4` on an Intel Mac with macOS 26.7.3 signed and
    notarized both darwin binaries in 1m17s. `spctl` accepted both as `Notarized Developer ID`.
    Only amd64 was run ([record](release.md#signing-and-notarization)).
- **Services.** launchd install, start, stop, crash loop and log were run for real on macOS under a
  scratch home (#25, #34). The Windows Run value started the hub with a minimal PATH (#48).
  `systemd --user` ran for real on WSL with systemd enabled and linger on, using a scratch port,
  token and database. Install wrote and enabled the unit and the hub answered. Status, stop and
  start behaved as expected. A SIGKILL was restarted within 2 s; a SIGTERM was a clean stop and was
  not restarted. The journal shows each start. Uninstall removed the unit and the saved settings.
  `iris service status` exited 0 while running, 3 after stop and 4 after uninstall (#85).
  After `wsl --shutdown`, the unit was active about a second after the VM booted, and the hub
  answered. That run had a confounder: the Codex desktop app restarts the VM within a second of
  every shutdown, through a login shell, so a login session existed, and the run cannot show
  whether linger alone starts it.
- **CI.** Linux, macOS and Windows pass the test suite and six targets cross-build. The Go coverage
  gate counts statements (#246): a rounded percentage had let 10306 of 10309 pass as 100.0%. Each
  system must cover every statement it builds, and so must the merged profile. On #246's head `1f183a1` that was
  10343 of 10343 on Linux, 10306 of 10306 on macOS, 10414 of 10414 on Windows and 10657 of 10657
  merged. The console's gate requires 100% of statements, branches, functions and lines.
- **Hub CPU while idle, before and after the stream change.** On the public hub at `4840765`, by the
  Holzera operator's measurement: `iris serve` at 35 to 52 percent of one core with about 15
  connections, and tables of 148 Agents, 468 Executions, 3227 Claims, 2583 Deliveries and 1900
  Messages. Over 60-second windows the process then used 25 to 34 CPU seconds a minute before the
  change that removes per-socket polling (#245) and about 0.3 after it, with 14 to 15 and 11 to 12
  connections. In the 3.29 hours after the host's restart that night it used 18.5 CPU seconds, which
  is 0.09 a minute. This is not a controlled comparison: the WSL sessions ended seconds after the
  deploy ([record](research/hub-idle-cpu.md#public-hub-before-and-after-the-deploy)).
- **Public hub login cutover.** Password login is active on the public hub. It is made of the
  account schema (#223), authorization (#225), sessions (#226), the importer (#228), the login and
  profile forms (#229), the operator access editor (#242) and activation (#250,
  [ADR 019](adr/019-console-activation.md)). The approved import created 2 accounts, one operator
  and one member, on 2026-11-10 at about 16:48 UTC. The dry run before it listed 2 to create; the dry run after it
  listed 2 skipped, none quarantined. The cutover deploy of `d4cae61` (#250) at 16:49:18 UTC that day
  closed the console as [the runbook](console-activation.md#first-cutover-on-the-public-hub) expects;
  agent coordination kept working.

  On 2026-11-11 the owner activated the named console in his own terminal. Activation said a restart
  was required. The coordinator restarted the hub at 10:02:30 UTC, and the hub's log said
  "Iris console open". The owner signed in with both accounts and changed both passwords. Five
  minutes after the restart the container's restart count was 0.

  The console was closed for 17 hours 13 minutes. That was the wait for the owner, not the time the
  procedure itself takes. Login links are gone on the public hub; the web console takes a username
  and a password.

## Not done or not measured

- **Other clients on real hosts.** Codex has run with Claude in the bounded macOS and WSL
  shared-worktree experiments above; that does not validate every integration path. Cursor,
  Antigravity, Grok and Kimi do not have equivalent end-to-end evidence in this status record.
  Copilot has run live on Linux only.
- **Remote execution targets are unprotected.** A client whose tools run on a remote host, in a
  container or in a cloud sandbox changes files Iris neither claims nor checks: the hook sees only the
  machine it runs on (ADR 009). Detection waits for a captured remote payload.
- **Distribution after v0.1.0.** Release archives are published and the signed release workflow
  succeeded. Package-manager uploads remain disabled. The newer GHCR release-image path was
  added after that tag's source commit; the earlier release does not prove that later path.
  Each release must retain its own signatures and verification evidence. A published archive
  is not evidence for a clean browser-download or package-manager installation on every host.
- **Not measured:** Defender, the SmartScreen window, Apple silicon, a quarantined browser download
  of a notarized binary starting under Gatekeeper, Gatekeeper's dialog and "Open Anyway", and a Mac
  or PC whose policy forbids unsigned code.
- **An internal/integrate test that fails under load.** One run of four failed on Windows with every
  CPU busy. It has not reproduced since, and its name was not captured. On 2026-11-08 about 470 runs
  of `go test -v ./internal/integrate/` under load on all 16 cores of WSL on the same PC passed, with
  48,902 test passes and none failing, so it did not reproduce on Linux. A -v loop under load on
  Windows itself is still pending. On native Windows at `993e545`, three uncached repetitions of
  `go test -p 1 -count=3 -timeout=3m -v ./internal/integrate/...` passed in 22.746 s with
  `CGO_ENABLED=0` and `GOMAXPROCS=2`. The local diagnostic log is
  `iris-windows-integrate-993e545.log`. This bounded validation used no artificial CPU stress and
  does not reproduce or resolve the original full-load failure.
- **ADR 018 scope tests that fail now and then in CI.** On 2026-11-13 three of them failed once each,
  every time in a pull request that did not touch the hook:
  - `TestAColdCacheRegistersBeforeChoosingTheScope` failed on `test (macos-latest)` in run
    43392693883 (#290 at `b97cd78`). Its case of a full status past its budget on an older hub saw
    no hub call at all, and the test's temporary directory could not be removed because something
    was still writing into it. The job passed when it was rerun.
  - `TestAFailedScopedRequestIsNeverResentAsFull/one_call/past_the_budget` failed on
    `test (macos-latest)` in run 43392561151 (#296 at `0347e5b`). The run ended at stage
    "shell session" without the deadline error.
  - `TestEveryClientsOutputIsValidInReducedScope/copilot` failed on `test (ubuntu-latest)` in run
    43396567786 (#299 at `3136b78`). After a status that should have tripped the breaker, the breaker
    still had the state the test had set, with no new cooldown.

  Each failing case gives the whole reconcile a real 20 ms deadline (the trip budget in
  `internal/hook/scope_unix_test.go`). On a loaded runner that deadline can pass before the step
  under test has begun or has recorded its result. That is the suspected cause, not yet confirmed.
  A required check that fails one run in a few costs every pull request a rerun.
- **Services:** loading the LaunchAgent at login, the `systemd --user` unit starting at boot from
  linger alone, without a login session (it needs the Codex desktop app closed first), and the
  service exit codes on Windows.
- **`iris doctor --live` on clients other than Claude Code.** It ran for real for Claude Code
  2.1.319 on WSL and on macOS against its scratch hub. Grok's allow flags were measured on macOS.
  Codex's invocation follows claude-laptop-iris's measurements of its flags on macOS. The whole
  run has not yet been measured end to end. The other clients' invocations are not measured yet.
- **Native review of the operator console strings, and layout polish.** The 38 `operator` strings
  are translated into all 44 locales. Claude made the translations, and Codex reviewed them twice,
  with gpt-6.1-sol and then gpt-6-astra. They are machine-made. Each locale file says so above its `operator` block, until a
  native speaker has reviewed them.
- **The access editor's strings.** gpt-6-astra reviewed the 15 `access` strings in all 44 catalogs at
  `313dab9`, and its 24 corrections of meaning or terminology in 10 locales are applied. The Armenian
  `access.saved` does not say which account must sign in again, and no verified wording was available,
  so it is unchanged. The reviewer could not judge full grammar or idiom for `ga`, `is`, `hy` and
  `my-MM`, nor subtle idiom for `lt`, `lv` and `sq`. The Icelandic and Bengali corrections are medium
  confidence. No locale has had a native speaker's review.

## SonarCloud

SonarCloud's quality gate reported every condition passing on 2026-11-09, `new_security_rating`
included, which was the one that had been red. These findings were reviewed and kept on purpose:

- `PATH` lookups for `git`, `xdg-open` and the clients' binaries are how those commands are found.
  The `git` call in `internal/gitinfo/command.go` carries a `NOSONAR` line comment for `go:S4036`,
  because SonarCloud raised it as a new finding when three accepted calls became that one. The
  comment hides every SonarCloud finding on that line, so a change to that call needs review.
- The session cookie has no `Secure` flag on loopback, where the console is served over HTTP and
  Safari would refuse a `Secure` cookie. Behind the public origin it is `Secure`.
- HTTP and WebSocket URLs on loopback, for the local hub and the console.
