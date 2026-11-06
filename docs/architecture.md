# Iris architecture

Status: baseline, 2026-11-06. It is built from `docs/spec/requirements.md` and from
measurements on the seven target clients, kept in `docs/research/clients/`. Each decision
says what it rests on. Later changes go in `docs/adr/`.

Claims are labelled by strength. **Measured** means observed on an installed client on Linux.
**Documented** means vendor documentation or source only. Windows and macOS behaviour is
documented, not yet measured.

## 1. Shape of the system

```
 agent session (any client)
   ├─ hooks ───────  iris hook   ──┐
   ├─ MCP ─────────  iris mcp    ──┤  HTTP + bearer token      ┌────────────┐
   └─ monitor ─────  iris watch  ──┤  (WebSocket for watch)    │ iris serve │── SQLite
 git commit ───────  iris guard  ──┘                           │  + web UI  │
 browser ──────────────────────────────────────────────────────┘────────────┘
```

One Go binary plays every role. One `iris serve` per deployment owns the database. Everything
else is a short-lived or per-session client of it. There is no per-host daemon in the first
versions: every client process talks to the hub directly.

## 2. Decisions

### D1. One hub, reached over HTTP

`iris serve` is the only writer of SQLite. Clients reach it at a configured `hub_url`, by
default `http://127.0.0.1:7878`, with a bearer token from a user-only file. The same transport
works on every platform; Unix sockets and named pipes would fork the code and stop at the
WSL and container boundaries.

Loopback is a default, not a topology. WSL in NAT mode, containers and other machines need a
configured URL. The token must never travel in the clear, so clients refuse an `http` URL unless
the host is literally `127.0.0.1` or `::1`, and `iris serve` refuses a non-loopback listen
address without a certificate. A remote hub therefore means TLS or an SSH tunnel. The database
stays on a local disk of the hub, never a network or `drvfs` mount, because WAL does not work
across those.

`iris agent`, the per-host process in early drafts, is deferred. Its jobs are done by the
per-session `iris mcp` and `iris watch` processes.

### D2. Four surfaces, each with an honest guarantee

| Surface | Purpose | Guarantee |
|---|---|---|
| MCP tools | Explicit actions by the model | Universal, but only when the model calls them |
| Client hooks | Automatic binding, context, pre-edit check | Blocks a visible edit tool; fails open by design |
| `iris watch` | Attention | Best available per client, see D7 |
| Git guard | Last checkpoint before history | Cooperative: `--no-verify` bypasses it |

Claims are advisory outside mediated actions. Iris never says that a file is protected, only
that a conflict was prevented or reported.

### D3. One hook path with per-client adapters

Every client runs `iris hook --client <c> --event <e>`. The event is passed in argv because
Copilot and Antigravity do not name it on stdin. An adapter decodes client JSON into a
canonical `Event` and encodes a canonical `Decision` back. Native HTTP or MCP-tool handlers are
not used; they would be a second code path on the most fragile surface.

An adapter owns event names, the tool taxonomy, path extraction, decision encoding and
tolerance of unknown fields. It does not own claim logic, repository identity or messaging.
Payloads have no schema version, and measured payloads have already diverged from vendor
docs, so the captured fixtures in `internal/hook/testdata/` are the contract. Adapters require
only a session id and tolerate everything else. Unknown tools pass through, and the hook
reports their names so that `iris doctor` can show drift after a client update.

Hard rules for `iris hook`, because failure semantics differ between clients:

| Client | Hook exits 1 | Timeout | Measured deny |
|---|---|---|---|
| Claude Code | allowed | allowed | JSON |
| Codex | allowed (measured) | allowed (measured) | JSON |
| Copilot | **denied** (measured) | allowed | JSON |
| Antigravity | **denied** (measured) | unknown | JSON |
| Cursor | allowed unless `failClosed` | unknown | JSON |
| Grok | allowed unless `failClosed` | allowed | JSON with exit 2 |
| Kimi | allowed | allowed | exit 2 with stderr |

1. Everything is caught. A failure of any kind produces that client's valid allow output.
2. The exit code is non-zero only for a deliberate deny on a client whose contract requires it.
3. A 1.5 s internal deadline ends the hook; the client-side timeout is set to 3 s explicitly,
   because defaults reach 600 s.
4. Non-mutating tools return without touching the network.
5. Outside a git worktree the hook does nothing.
6. Every fallback to allow caused by a failure is recorded in `hook-failures.log`, without payload
   content, and `iris doctor` reports it. A silent fail-open once hid a real bypass: Copilot sends
   no tool call id, the hub refused the request, and the edit went through a live claim.
7. A client that provides no tool call id gets one derived from a hash of the raw payload.

A hook is a new process, so every call starts with DNS, TCP and TLS. Against the public hub a lost
packet on any of them outlasted the deadline, and the edit went through without a claim: 3.7% of
requests in one measurement took over 1.3 s, while the hub answered in milliseconds. A hook
therefore sends a second request at 300 ms, on its own connection and resolver, and uses whichever
answers first. Measured the same way, none took over 1.3 s. This is safe because session
registration, `touch` and session end can be repeated. Reconcile is not repeated. A network that is
down for the whole 1.5 s still fails open, and that is recorded as above.

Target latency is p95 under 50 ms on Linux and macOS and under 150 ms on Windows, measured in
CI with the real binary.

Fail-closed is an opt-in per repository, and only on clients that support it. Copilot always
fails open on a timeout, so Iris cannot promise fail-closed there.

### D4. A missing binary must not brick a client

On Copilot and Antigravity any non-zero hook result denies the tool, so removing the binary
without removing the hooks, or a crash of the Go runtime, would stop those agents from doing
anything. For these two clients `iris integrate` writes a guarded command. A deliberate deny on
these clients is JSON with exit 0, so a non-zero status can only mean a crash or a missing
binary, and either one must become the client's recorded allow output:

```sh
command -v iris >/dev/null 2>&1 && iris hook --client copilot --event preToolUse || echo '{}'
```

`command -v` does not exist on Windows. There the guard uses `where` in a `cmd` shim for
Antigravity, and `Get-Command` plus `$LASTEXITCODE` in Copilot's `powershell` field. The guard
is covered by tests on each OS. Other clients use direct exec. Integrations refer to the stable
command path, never a versioned package directory.

### D5. Clients load each other's configuration

Grok loads Claude and Cursor hooks, plugins and MCP servers by default (measured), and Cursor
exports `CLAUDE_PROJECT_DIR`. Environment variables also leak across nesting: a Copilot session
started from a Claude session still carries `CLAUDE_CODE_SESSION_ID` (measured).

So `--client` selects a parser, and the hook then confirms who is really running it. Environment
markers alone are not proof: a profile can export them, and `GROK_HOOK_EVENT` is also set on
Claude hooks that Grok runs. A client counts as the runner only when its session variable equals
the session id on stdin, for example `GROK_SESSION_ID` equal to `sessionId`.

When a hook meant for one client runs under another, it prints the runner's allow output, exits
0 and does not call the hub, whether or not the runner has its own integration. `iris doctor`
reports the cross-load. The hub is idempotent per client, session, event and tool call, so a
double fire is harmless. Configuration of a client is written only for that client.

### D6. Binding a tool call to an Execution

Hooks always receive a session id. The problem is the stdio MCP process, which often does not.

| Client | Id in stdio MCP env | Hook and MCP share parent |
|---|---|---|
| Claude Code | `CLAUDE_CODE_SESSION_ID` | yes |
| Copilot | `COPILOT_AGENT_SESSION_ID` | yes |
| Grok | `GROK_SESSION_ID` | yes |
| Cursor | no | yes, through a shell |
| Antigravity | no | yes, through a shell |
| Kimi | no | yes, through a shell |
| Codex | no | yes for `exec`; **no** for app-server, where threads share one pid |

`iris mcp` resolves its Execution at the first tool call, by this chain:

1. `IRIS_EXECUTION`, when a launcher set it.
2. The client's own session id variable. The variable for the client named on the command line
   wins over any inherited one.
3. The nearest client process. A hook records `(host, pid, start time)` of its nearest
   non-shell ancestor at `SessionStart`, and the shim looks up its own parent. The walk stops
   at the first client process; walking further bound the wrong session in a measurement. This
   rung applies only where a process is positively known to host a single session: Cursor Agent,
   Antigravity, Kimi and `codex exec`. It is skipped for Codex app-server and anything else that
   multiplexes sessions, because a second thread that has not registered yet would bind to the
   first one.
4. An explicit handle. The `SessionStart` context gives the model its handle, and every Iris
   tool accepts an optional `execution` argument.
5. Otherwise the tool returns a typed `unbound` error that says how to proceed. Iris never guesses.

None of this is authority. Across a host the boundary is the OS user (D12), so binding gives
correct attribution, not a security property. If hooks never ran, for example in an untrusted
folder, the shim registers an MCP-only Execution so that messaging still works.

### D7. Attention: instant where possible, honest everywhere

The hub exposes one stream per Execution: a WebSocket and `iris watch`, which prints the same
frames. A frame says only that something changed, never what:

```json
{"stream":"<uuid>","cursor":123,"kind":"inbox_changed"}
```

Each client gets the strongest tier it has:

| Tier | Mechanism | Clients |
|---|---|---|
| Instant, idle session | Plugin monitor running `iris watch` | Claude Code (measured: 0.3 s from line to model turn) |
| Instant, armed by the model | `Monitor` tool with a `ws` source | Claude Code |
| Instant, Iris-owned session | App-server or ACP control of a session Iris started | Codex (measured), Copilot, Cursor, Antigravity, Grok, Kimi (later) |
| Next turn | Hook context at prompt or post-tool | All, with client-specific limits |
| Human | Web UI and desktop notification | All |

On Claude Code the plugin monitor is the default. It needs no re-arming and fires while the
session is idle, and it gets `CLAUDE_CODE_SESSION_ID`. The monitor event passes through
`UserPromptSubmit`, so the same hook can add a bounded inbox digest at that moment. It does not
run under `claude -p`.

Every wake costs a model turn, so `iris watch` coalesces events and reports only what needs
attention: new mail, and a claim someone is waiting for. The session context tells the model
what an Iris notification means, because without it the model correctly declines to act on an
unexplained background line (measured).

Commit precedes notification: the event table is the outbox. Delivery is at-least-once with a
per-consumer cursor. A cursor that has expired gets a reset plus a snapshot, never a silent gap.
A wake-up never acknowledges a message.

`iris watch` authenticates with the normal token. The `Monitor` tool cannot send headers, and its
URL and subprotocols end up in the transcript, so it uses a **wake ticket** instead: a fixed
`iris.wake` subprotocol plus a random base64url ticket, never a query string. A ticket comes from
an MCP tool, reads one Execution's stream only, lives for minutes, serves one socket, rotates on
every arming and dies with the Execution. Notifications and digests carry the `kind` only.

### D8. Liveness comes from the MCP process

A stdio MCP server normally lives exactly as long as its client session, on every client.
`iris mcp` therefore sends a heartbeat every 20 s for its positively bound Execution generation.
It stops on stdin EOF, when its parent process is gone and on loss of binding, so that an orphan
cannot keep claims alive. The parent is checked by identity, because on Windows a process keeps
its parent pid after the parent dies (found by CI). An Execution with a recent heartbeat has a 60 s lease. With no shim or
watch process it has a 15-minute lease, renewed by hook activity, because no hook fires during a
long model call. That is also how long a dead session of that kind can hold a claim. Claims need
no renewal of their own: a claim is valid while its Execution is live and its own optional expiry
has not passed.

`Stop` is a turn boundary and releases nothing. `SessionEnd` is a best-effort speed-up: it
does not fire reliably, and it has a 1 to 1.5 s budget. Leases are measured on the hub's
monotonic clock within one boot. Claim expiries must survive a restart, so they use the hub's
wall clock. Client clocks are never trusted.

### D9. Domain model and storage

`docs/research/reference-schema.sql` is the starting point, and it passed 23 reference checks
in Python. The Go implementation must repeat them on `modernc.org/sqlite`. The main entities:

- **Agent**: a durable mailbox, identified by repository, host, client and worktree, with a
  slot number for concurrent sessions. The default name is `<client>-<host>-<worktree>`.
- **Execution**: one session. A resumed session keeps its Execution while the lease holds, and
  otherwise gets a new generation. Subagents link to a parent and do not conflict with it.
- **Claim**: held by an Execution, with a monotonic fence token.
- **Message** and **Delivery**: one Delivery per recipient, so groups need no second mechanism.
- **Event**: the ordered log and outbox. **Cursor** per consumer. **Operation** for idempotent
  retries, keyed by principal and client-chosen key, with a request hash so a reused key with a
  different request returns 409.

Every state change happens in one `BEGIN IMMEDIATE` transaction. An OS file lock stops a second
hub on the same database.

Changes from the reference schema:

1. Delivery has a recipient, and a message can have several.
2. Claims survive a hub restart as recovery reservations, because an upgrade must not drop
   every claim in the system. At startup the boot epoch increases, and each active Execution
   without a `recovery_deadline` gets one, persisted, 120 s ahead. A recovering Execution's
   claims still block others, but it may do nothing until it reattaches with the same session
   and generation, which clears the deadline and grants a lease in the new epoch. A heartbeat
   from `iris mcp` or `iris watch` carries that proof, so idle sessions reattach too. At the
   deadline it is lost. Repeated restarts cannot extend the window, because an existing deadline
   is never moved, and an expired generation is never revived.
3. A claim has `origin` (`explicit` or `auto`) and an optional expiry instead of a mandatory
   renewed deadline.

### D10. Claim semantics

Claims apply to a repository across all worktrees and branches. Branch and worktree are
context, not an exemption: two worktrees editing one file will meet at merge time, which is
exactly the collision Iris exists to prevent. A narrower policy can be added per repository.

A claim is `exact` or `prefix`, on repository-relative POSIX paths, with overlap at component
boundaries, so that `src` does not cover `src2`. Globs are not supported, and paths that do not
exist yet still conflict. An empty prefix, meaning the whole repository, needs an explicit flag.

**A pre-edit hook checks and claims in one step**, so protection exists without the model doing
anything, and `explicit` claims are for planned work. The step is one idempotent transaction
over every path in the tool call, including both ends of a rename. It grants all missing claims
or none. Paths the caller already holds are reused, and an `explicit` claim is never downgraded.
An `auto` claim lasts 10 minutes from the last touch. A retried call, identified by its tool call
id, does not extend it.

`claim.ignore` lists paths that never take an auto claim and only warn: by default lockfiles such
as `go.sum`, `package-lock.json`, `pnpm-lock.yaml` and `Cargo.lock`, changelogs, and generated
output. An `explicit` claim still applies to them.

The conflict action per repository is `deny` by default, or `ask` or `warn`. Refusing before the
edit is what Iris is for, so `deny` stays the default, and the first deny must read as
coordination, not as a failure. It names the path, the holder and the expiry, and gives one
concrete next step:

```
Iris: src/api.go is held by codex-host-api until 14:32 (auto claim).
Message them with the send tool, or work on something else. Do not bypass this with a shell command.
```

Policies are checked against client capabilities. Codex does not support `ask` on a pre-tool
hook, so there `ask` becomes `deny` with the instruction to ask the human. A policy is never
silently weakened to an allow.

### D11. Repository identity and paths

The hub assigns a repository id. Aliases point to it: normalised remote URL, root commit, and
host plus path. A remote joins an existing repository automatically only when it means the same
thing everywhere: a fully qualified host with no contradiction in the root commit, whatever the
spelling. `git@host:owner/repo.git`, `ssh://` and `https://` are one remote. That covers the normal case across machines. Spellings that depend on one machine,
such as SSH aliases, `insteadOf` rewrites and relative remotes, stay scoped to that host and
path. A match on root commit alone, which is what forks look like, and any contradiction are
only suggestions for a human. Without a remote the identity is local to the path.

Hooks work out the worktree root and remote by reading `.git` files directly, because spawning
`git` on each hook is too slow, particularly on Windows. Paths are stored repository-relative
with forward slashes, with a per-repository case policy, and `..` escapes are rejected.
Antigravity runs hooks in `.agents/`, so its adapter takes the root from `workspacePaths`.

### D12. Shell edits

A shell command is visible only as a string, so a pre-tool hook cannot tell what it will
touch. After a shell tool, the hook compares `git status --porcelain -z` with the previous
state under a time limit, and warns about paths claimed from another worktree. It detects, it
cannot undo. In a shared worktree attribution is impossible, so it stays silent there.

`iris guard` is a pre-commit check, installed per repository on request. It refuses a commit
of paths held by another Execution. It is the strongest check Iris has, and it is cooperative.

### D13. Security

The trust boundary is the OS user on a host, plus a token per host across hosts. The token
separates users and hosts, nothing finer. Any process running as that user, including a hook
from a repository the user trusted, can read it and act as any Execution. Agents under one user
are not isolated from each other, and Iris does not claim otherwise. Inside that boundary Iris
relies on leases, delimited bodies and text-free wake frames.

- **Credentials.** The token is in a user-only file: mode 0600, and an owner-only DACL on Windows.
  Never in hook environments.
- **Network.** Bind to loopback by default, and see D1 for remote hubs. Check `Host` and `Origin`
  on every route, against DNS rebinding and cross-site requests from browsers. No CORS. The web
  UI signs in by one-time link and uses an `HttpOnly`, `SameSite=Strict` cookie, on loopback only.
- **Repository-supplied configuration.** Project-level hooks and MCP servers are code. `iris
  integrate` writes user-level files only and never grants trust on a user's behalf. `iris doctor`
  reports trust state instead.
- **Prompt injection.** Bodies of up to 16 KiB are data, marked by sender and delimited wherever
  they reach a model. Sender names are restricted to a safe charset. Wake frames carry no text.
- **Denial of service.** Limits on claims per Execution, leases on everything, and a web UI that
  can revoke any claim.

### D14. Install experience

```
brew install mateusz-klatt/tap/iris     # or winget, scoop, apt, dnf
iris setup
```

`iris setup` detects clients and shows one screen: the hub URL, with a non-local hub entered here,
and the exact files it will write. It asks once, then installs the user service (launchd,
`systemd --user` or HKCU Run), integrates the clients and runs `iris doctor`. It ends with
the steps only a human can do, such as Codex's `/hooks` review, and reports `Iris inactive on
<client>` for each one left undone, so that a green finish is never false. `iris integrate
<client>` and `iris integrate --remove` do the parts separately.
On Windows the `Iris` value under
`HKCU\Software\Microsoft\Windows\CurrentVersion\Run` runs the stable binary with
`service start`. The first real non-elevated Windows 11 setup measured access denied for
Task Scheduler's ONLOGON registration: it requires elevation. Installation starts the hub
immediately. `service start` detaches `iris serve` without a console, logging beside the
database; `service stop` checks the pid file's process creation time before termination.
Start is a no-op if the hub responds, and the database OS lock still guards against two hubs.
The Run value starts at logon; it does not supervise or restart a crashed hub.

Writes are idempotent, backed up and validated before saving, which matters because Kimi drops
its entire hooks section over one bad key (measured).

`iris integrate --remove` edits each file as it is now and takes out only what Iris wrote, so a
change made after install survives. A file Iris created goes if nothing else is in it. The backup
is used only to return the original bytes when nothing but Iris differs, and never if it holds Iris
entries of its own, because that would reinstall an older integration. Removal cannot keep the
formatting of a JSON file changed after install: keys come back sorted and indented by Iris's
writer, as they do on install. A file that no longer parses is refused and stays in the manifest
for a retry. A plugin skill somebody edited no longer matches its template, so it stays.

Clients that cannot be fully automated are reported truthfully. Codex needs its hooks reviewed
once in `/hooks`; project hooks need trust on Cursor, Copilot and Grok. Kimi has no project
hooks at all. Monitors require the Claude plugin, so `iris integrate claude` installs one,
generated from files embedded in the binary.

`iris doctor` runs offline by default: hub reachable, token valid, client version, configuration
present, trust state, unknown tools seen, last hook received, and a synthetic canary through the
configured hook command. `iris doctor --live` runs the real client on its cheapest model. It is
billable, so it is opt-in. Output is per client binary, not per vendor, because the Cursor IDE and
`cursor-agent` differ.

### D15. Distribution

Names: the product and binary are `iris`. The bare package name is taken in Homebrew Cask,
Scoop Extras, AUR and npm, so commands always use a namespace: `mateusz-klatt/tap/iris`,
`MateuszKlatt.Iris`, and a dedicated Scoop bucket. `irisd` is free everywhere and stays in
reserve for any channel that needs a bare name.

| Stage | Contents | Needs |
|---|---|---|
| 0 | CI on three OSes, GoReleaser snapshots | Now, in a private repository |
| 1 | Releases, checksums, cosign keyless, SBOM, Homebrew tap, Scoop bucket, GHCR | Public release assets, after IP review |
| 2 | winget, signed apt and dnf through Cloudsmith OSS, GitHub attestations | Public release URLs; a public repository for attestations |
| 3 | Apple notarization, Windows signing | $99 a year; SignPath Foundation, or Certum Open Source from €49 |

Private release assets cannot serve package managers. The source repository need not be public
for stage 1 if the assets are public somewhere, but GitHub attestations on this plan need a public
attesting repository that truthfully built the artifact. Azure Artifact Signing is closed to
individuals in the EU, and Iris has no company behind it.

Windows signing tries SignPath Foundation first, after the first public release, because it is
free. The certificate then names SignPath Foundation as publisher, and approval is a review that
Iris may not pass while it has no users. The fallback is Certum Open Source Code Signing in the
cloud, issued to the maintainer as "Open Source Developer" and limited to non-commercial
distribution. CI would then hold a long-lived TOTP seed, so it stays in a GitHub environment with
manual approval. Neither removes SmartScreen warnings at once, because reputation builds over
time. A private key can no longer be delivered as a file, so a `.p12` in Azure Key Vault is not an
option.

Until macOS binaries are notarized, Homebrew ships a prebuilt formula through GoReleaser's `brews`
key, which v2.20 supports but deprecates, so it must be replaced before upgrading to v3. A cask is
quarantined, and Gatekeeper would refuse an unsigned one. A formula avoids that mechanism but is
not a substitute for notarization, and this is to be checked on a clean Mac. Package managers own
upgrades, and the binary never replaces itself.

### D16. Web UI

The owner's React frontend moves to `web/`, built by Node at release time and embedded with
`go:embed`. The API layer is rewritten for Iris. See `docs/PROVENANCE.md` for what is excluded.
The first screen with no sessions shows a live wait for the first session, next to each client's
status from the last doctor run (hook received, not configured, needs a human step), with a button
to run the doctor. A silent client must show its cause, not an empty table.

## 3. Client capability matrix

| Capability | Claude | Codex | Copilot | Cursor | Antigravity | Grok | Kimi |
|---|---|---|---|---|---|---|---|
| Pre-edit deny, measured | yes | yes | yes | yes | yes | yes | yes |
| Session-start context | yes | yes | yes | yes | yes | partial | plugin only |
| Prompt context | yes | yes | partial | IDE only | yes | no | yes |
| Stop can continue | yes | yes | partial | partial | yes | partial | yes |
| Session end event | yes | yes | yes | yes | Stop only | yes | unreliable |
| Edit path form | field | patch text | field or patch | field | field | field | field |
| Hooks in headless mode | yes | yes | yes | yes | yes | yes | yes |
| Project hooks | trust | trust and review | trust | trust | no trust step | trust | none |
| User hooks need a step | no | review once | no | no | no | no | no |
| Wake idle session | yes, plugin monitor | owned sessions | owned sessions | owned sessions | owned sessions | owned sessions | owned sessions |

## 4. Known gaps

- The test suite passes on Windows and macOS in CI, but no real client has run there yet. Hook
  spawn cost under Defender, `cmd /c` quoting, and whether fail-open holds on those systems all
  need real runs before any claim of support.
- Whether plugin monitors survive `--resume` and `/clear`, and at what rate monitors are stopped.
- Whether `SessionStart` context on Grok and Kimi reaches the model at all.
- The nearest-process rung has not been tried on a Codex TUI session or a Cursor IDE session.
- The clean-room record has had no legal review.

## 5. Invariants the tests must prove

On `modernc.org/sqlite`, with real concurrency:

- Of concurrent overlapping grants, exactly one wins. A multi-path grant is all or nothing.
- Prefix, case-folding and rename boundaries hold across worktrees.
- A self-touch reuses a claim and never downgrades an `explicit` one. A replayed tool call does
  not extend an expiry.
- Expired, released, wrong-owner and old-epoch fence tokens fail. A lost lease cannot renew.
- Retries cannot double-send or resurrect a claim. A key reused with a different request gets 409.
- A failed transaction exposes nothing.
- Deliveries survive a crash and deduplicate by id. A stale ack fails, and a repeated ack is harmless.
- Cursors never regress. A reconnect neither loses committed events nor exposes another recipient
  or any content. An expired cursor returns a reset with a snapshot.
- Clock skew on clients cannot extend a lease.
- Repeated restarts cannot extend recovery. An unattached Execution is lost at its deadline.
- An orphaned heartbeat cannot preserve ownership. A second, unregistered session in one process
  cannot bind to the first.
- An ambiguous URL cannot join a repository automatically.
- A policy a client cannot express never becomes an allow.
- For every client, a crash, a timeout and a missing binary all leave the tool usable.
