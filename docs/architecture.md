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
| Antigravity | **denied** (measured) | **denied** (measured) | JSON |
| Cursor | allowed unless `failClosed` | unknown | JSON |
| Grok | allowed unless `failClosed` | allowed | JSON with exit 2 |
| Kimi | allowed | allowed | exit 2 with stderr |

1. Everything is caught. A failure of any kind produces that client's valid allow output.
2. The exit code is non-zero only for a deliberate deny on a client whose contract requires it.
3. A 1.5 s internal deadline ends the hook; the client-side timeout is set to 3 s explicitly,
   because defaults reach 600 s. Antigravity's is 10 s, because there a timeout kills the hook
   and fails the tool call, MCP tools included (measured on 1.5.3), and no guard can turn that
   into an allow. With 10 s the internal deadline fails open long before. What remains is a
   hook that stalls before its own deadline can act, or a hung process: on Antigravity that
   still denies every tool for as long as it lasts.
4. Non-mutating tools return without touching the network.
5. Outside a git worktree the hook does nothing.
6. Every fallback to allow caused by a failure is recorded in `hook-failures.log`, without payload
   content, and `iris doctor` reports it. A failure graded `silent` is described as a fail-open in the
   last 24 hours, warning that edits may have gone unclaimed. This history cannot establish current
   client downtime or identify which edits went unclaimed. A silent fail-open once hid a real bypass:
   Copilot sends no tool call id, the hub refused the request, and the edit went through a live claim.
   Unknown tools and cross-loads are not failures, but doctor reports them too, from
   `hook-observed.log` (gap review C4).
7. A client that provides no tool call id gets one derived from a hash of the raw payload.

A hook is a new process, so every call starts with DNS, TCP and TLS. Against the public hub a lost
packet on any of them outlasted the deadline, and the edit went through without a claim: 3.7% of
requests in one measurement took over 1.3 s, while the hub answered in milliseconds. A hook
therefore sends a second request at 300 ms, on its own connection and resolver, and uses whichever
answers first. Measured the same way, none took over 1.3 s. This is safe because session
registration, `touch` and session end can be repeated. Reconcile is not repeated, full or reduced:
a second copy of a snapshot would find nothing changed and could answer first. A network that is
down for the whole 1.5 s still fails open, and that is recorded as above.

A shell or Code Mode call is reconciled within its own budget, inside the 1.5 s: 300 ms when the
hub is on this machine (a loopback address or `localhost`), and 1 s otherwise. A reconcile makes
several hub calls on a new connection. Across a network the old 300 ms ran out routinely: 32
times in 45 minutes on a Mac 60 to 80 ms from the hub. Each time, that command's changes went
unchecked and unclaimed. The larger budget also leaves room for the second request at 300 ms. A
hub name is not resolved to decide this, so a name that points at this machine gets the larger
budget, which costs only time.

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
if [ -x /path/to/iris ]; then /path/to/iris hook --client copilot --event preToolUse;
elif command -v iris >/dev/null 2>&1; then iris hook --client copilot --event preToolUse;
else false; fi || echo '{}'
```

The guard runs the path recorded at integration first, and `iris` from `PATH` only if that path
is gone. An earlier guard tested only `command -v iris` and then ran the recorded path. When the
client's `PATH` lacked that directory, every hook was skipped without a trace: measured with
Copilot on macOS, no session reached the hub and `hook-failures.log` stayed empty, because iris
never started. On Windows the guard uses `if exist` and then `where` in a `cmd` shim for
Antigravity, and `Test-Path`, then `Get-Command`, plus `$LASTEXITCODE` in Copilot's `powershell`
field. The guard is covered by tests on each OS. Other clients use direct exec. Integrations refer to the stable
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
   Antigravity, Kimi and `codex exec`; or one top-level session, as Claude does, whose subagents
   register under a parent and are not matched. Two live top-level sessions of one process are
   ambiguous. It is skipped for Codex app-server and anything else that multiplexes sessions,
   because a second thread that has not registered yet would bind to the first one. For Claude
   this rung is what ties an MCP server to its hooks: on `--continue` Claude Code starts the MCP
   server with another session id than its hooks get (`docs/research/clients/claude-code.md`).
4. An explicit handle. The `SessionStart` context gives the model its handle, and every Iris
   tool accepts an optional `execution` argument.
5. Otherwise the tool returns a typed `unbound` error that says how to proceed. Iris never guesses.

None of this is authority. Across a host the boundary is the OS user (D12), so binding gives
correct attribution, not a security property.

The session's hook is its registrar: one session is one Execution and one mailbox. An MCP server
usually starts before the `SessionStart` hook has run, so when no rung finds the session its
first bind keeps looking, with backoff, for 15 s. Only then, if hooks never ran, for example in
an untrusted folder, does the shim register an MCP-only Execution so that messaging still works,
and only where it can name the session: by its session id, or as a single-session process. If
the hook registers later after all, the shim's heartbeat finds the hook's Execution for the same
client process, moves to it and ends its own, handing over the mail its stray mailbox received
meanwhile, so a slow hook cannot leave the session split either. A first tool call that ends
while the grace period runs is told Iris is still binding the session and to retry. After a loss the shim never registers; it waits for the hook to reattach (D9).

Adoption keeps the original handover source, generation and destination until the hub confirms
the end. Failed calls retry that same idempotent end, including when it committed but its response
was lost. The destination becomes the binding immediately; both it and a still-live fallback are
renewed while handover is pending. A monitor follows the new binding even while its old stream
remains live, without announcing a loss. Retry state lasts only for the client process: if it ends
before handover commits, the fallback mailbox still holds the mail. A genuine lease loss can
still prevent handover; retries never revive a lost generation.

A worktree is one directory however it is reached. Hooks, the shim and the guard name the
worktree root by device and inode as well as by path, and the hub keeps the first path a host
named for that directory: a bind mount or a symlink that gives the same directory another path,
as `/mnt/d/projects/iris` and `/home/klattm/projects/iris` on one host, is made that first path,
so it is the same worktree, the same agent and the same claims. A remount or a reboot can give a
device and inode to another directory, so a path folds only when its repository, by remote and
root commit, is the one the first path was seen with. On Windows the id is the volume serial
number and the 128-bit file id, so a junction, a mapped or subst drive, or a path spelt in another
case is the same worktree too (measured on NTFS). The 128-bit id is unique on ReFS and Dev Drives
as well, where the older 64-bit file index is not guaranteed to be. A file system without it, such
as FAT or UDF (measured on a UDF disc), falls back to the serial and the 64-bit index. On a network
drive either id is only as good as the server, so two directories there could in principle share
one, which the repository check above limits to the same repository. ReFS itself was not measured.
Every path from an older client is keyed on itself, as before.

### D7. Attention: instant where possible, honest everywhere

The hub exposes one stream per Execution: a WebSocket and `iris watch`, which prints the same
frames. A frame says only that something changed, never what:

```json
{"stream":"<uuid>","cursor":123,"kind":"inbox_changed"}
```

Streams observe committed-change hints rather than polling (ADR 016). One hub worker
coalesces commit bursts within 10 ms of their first change and performs at most one expiry
sweep per second while streams exist. A no-op sweep does not wake the sockets. Each
observation validates and reads in one transaction; a full event page drains immediately,
and transport pings perform no store query. Subscribe before observing state so a commit
between a read and its wait cannot be lost. Durable cursors and reset snapshots remain
authoritative; hints are neither acknowledgements nor message contents.

Lease, claim, receipt and absolute console-session deadlines use that shared sweep.
Their observation can wait for its interval, coalescing, scheduling and store contention;
this is not a hard real-time guarantee. Ticket rotation signals affected sockets in memory.
A named console keeps observing revocation while a network write is stalled and queues a
frame only after admission commits. Already admitted frames can be in transit. Steady
expiry work uses live-row indexes; a real loss still visits deliveries assigned to that
Execution, and claim release retains its existing repository fanout.

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

A release wakes only a session that was waiting for it. When the hub refuses an Execution's
touch or claim because another Execution holds the path, it records that Execution as a
**waiter** on the path and kind it asked for. The waiter lasts one hour, whatever the blocking
claim's expiry, because that expiry is itself a release the waiter must hear about. It is
cleared when the Execution takes the path.
After any release (by the holder, by an operator's revoke, or by expiry), each live waiter whose
path no longer overlaps a claim of an unrelated Execution gets one `claim_available` event,
addressed to it alone, and is cleared. Overlap follows ADR 003's path identity, prefix claims
included. A waiter still blocked by another holder keeps waiting. `claim_released` still goes to
every agent in the repository, for the console and the claims tool, but `iris watch` ignores
it: before this rule, every release woke every session at the cost of a turn (measured on two
hosts). A refused explicit claim still returns its conflict, but only after the waiter it leaves
is committed.

Commit precedes notification: the event table is the outbox. Delivery is at-least-once with a
per-consumer cursor. A cursor that has expired gets a reset plus a snapshot, never a silent gap.
A wake-up never acknowledges a message.

`iris watch` authenticates with the normal token. The `Monitor` tool cannot send headers, and its
URL and subprotocols end up in the transcript, so it uses a **wake ticket** instead: a fixed
`iris.wake` subprotocol plus a random base64url ticket, never a query string. A ticket comes from
an MCP tool, reads one Execution's stream only, must be used within two minutes, and serves one
socket. Ticket expiry applies only to the handshake; an established socket keeps delivering frames
until a newer ticket for that Execution rotates it, the Execution is lost or ends, or the issuing
host token is revoked. Disconnects and hub restarts also end the socket. Arming another Execution
does not revoke it. A stolen ticket used before expiry can therefore keep its already-open socket
for the remaining life of the Execution, indefinitely while that Execution stays live and neither
rotation nor revocation occurs; the two-minute limit cannot bound that connection's lifetime.
Notifications and digests carry the `kind` only.

### D8. Liveness comes from the MCP process

A stdio MCP server normally follows the client process that launched it. `iris mcp` sends a
heartbeat every 20 s for its positively bound Execution generation.
Before renewing a fallback Execution, the client may look for the hook's Execution and hand over
to it (D6). That attempt has a five-second deadline, separate from the heartbeat's five seconds,
so a stalled lookup or handover cannot prevent lease renewal. An outage that also prevents the
heartbeat from reaching the hub can still cost the session its lease.
It stops on stdin EOF or when its immediate OS parent process is gone, checked before the next
renewal, so that an orphan cannot keep claims alive. The parent is checked by identity, because
on Windows a process keeps its parent pid after the parent dies (found by CI). A heartbeat
reporting a lost generation drops the binding and keeps the MCP process available for a fresh
bind (D9).

An Execution shown as `active` has not been ended or expired by the hub, including during D9's
recovery window; the hub does not check whether its client process is alive. Ending a session
changes only that Execution. In particular, a Codex
`exec` started through a Claude Code MCP bridge registers as a separate Codex session, without
an Iris parent link to Claude. Its `iris mcp` checks its immediate OS parent, normally Codex or
its launch shell, rather than the outer Claude session. An outer session ending therefore does
not establish whether Codex is
still running, and Iris does not terminate delegated clients. On Windows a private-hub
[measurement](research/clients/codex-parent-ended-windows.md) observed a dead Codex process still
shown as `active` until its lease expired.

An Execution in heartbeat mode has a 60 s lease, including after its shim or watch process
exits. A hook-only Execution that has not entered heartbeat mode has a 15-minute lease, renewed
by hook activity, because no hook fires during a long model call. That is also how long a dead
session of that kind can hold a claim. Claims need
no renewal of their own: a claim is valid while its Execution is live and its own optional expiry
has not passed.

`Stop` is a turn boundary and releases nothing. `SessionEnd` is a best-effort speed-up: it
does not fire reliably, and it has a 1 to 1.5 s budget. Leases are measured on the hub's
monotonic clock within one boot. Claim expiries must survive a restart, so they use the hub's
wall clock. Client clocks are never trusted.
A `SessionEnd` carrying stale remembered proof is not retried without that proof: a delayed end
must not acquire a healthy replacement generation and end it.

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

The hub logs one diagnostic after each committed generation replacement, naming the prior
Execution and generation, the replacement, the registration operation and source, and whether
the prior Execution ended or lost its lease or recovery window. A lost Execution from an earlier
epoch without recovery evidence is labelled `prior_lost`: its cause was not persisted, so the
current epoch mismatch cannot identify it. No payload, token, path,
client/session/host name or message content is logged by this diagnostic. Ordinary registration,
resume, reattachment and heartbeats preserve the generation and stay quiet; rolled-back changes
emit nothing. New hooks mark retries that discarded stale proof with `retry_generation`; this is
a client claim for diagnosis, never authority. Older clients omit it and are logged as registering
without proof. This records why the store replaced the generation, not why a heartbeat failed
to arrive; it cannot establish a network or client failure by itself.

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

   A hook is a new process for every tool call, so it keeps the last generation for its session
   in a file beside the token and sends it as that proof. Without proof, a recovering session's
   edit is refused instead of allowed: its claims still block others, and an allowed edit would
   be unclaimed. The file is protected as the token is: mode 0600 in a 0700 directory, and on
   Windows the token's owner-only DACL, which the directory passes to each file it creates. So
   anyone who can read that file can read the token, and the file proves no more than the token
   already does. That is protection from other users, not from an administrator, who can still take
   ownership of either file. Measured with a real Claude Code client before this change, a
   session's edits went through unclaimed for up to the full 120 s, and its claims were then
   lost. With a heartbeating `iris mcp` that was one edit. An `iris mcp` binds on its first tool
   call, and only a bound one heartbeats. A heartbeat that finds its generation lost, as after a
   laptop sleeps past the lease, drops the binding and the process keeps running: a client does
   not restart an MCP server or monitor that exits, so it showed the server as failed and the
   session went deaf. Its tools answer `lost` until the next call binds afresh, through the usual
   rungs, to the session's current Execution. An Execution found lost is never bound again, by
   whatever route it comes back, so no process speaks for a stale identity. `iris watch` says
   once that notifications stopped, keeps trying a fresh bind with backoff, says when they
   resume, and follows the new Execution from its own cursor. After the change, with and without `iris mcp`, all 16
   edits stayed claimed by the same Execution across the restart. With the file removed, the edit
   was refused and nothing went through unclaimed.
3. A claim has `origin` (`explicit` or `auto`) and an optional expiry instead of a mandatory
   renewed deadline.
4. A Delivery keeps four separate facts. `attempt` guards acknowledgements: each fetch raises it,
   and an acknowledgement must name the current one. `execution_id` is the Execution processing
   it. `attention` says the recipient still needs a wake. An `Ack` row means it is settled. A new
   delivery needs attention, and a fetch clears it. When the Execution that fetched it is no
   longer active, the delivery is freed, needs attention again, and one `inbox_changed` event
   wakes the recipient: the mail was fetched, but nobody may have read it (gap review A10).

   Deliveries are recovered like claims. A hub restart no longer frees them: a recovering
   Execution keeps what it fetched, can acknowledge it after reattaching, and wakes no one for
   mail it already read. One that is lost at its deadline frees its deliveries then, and they
   are requeued as above. An acknowledgement made before a crash still cannot settle a fetch
   made after it, because that fetch raised the attempt. An `inbox` response lost while its
   session stays alive is recovered by the fetch receipt (ADR 010): the MCP server confirms each
   fetch it received, the mail keeps needing attention until then, and a receipt that never
   comes wakes it once. A tool result the client itself drops after the receipt is not covered.

   Sender receipts (ADR 005) add `unreachable: true` for an undelivered non-human recipient with
   no active Execution, the same definition used by delivery problems. Reachable, acked and
   handled recipients omit the field. Human recipients also omit it because the console can read
   their mail without a live Execution. This is computed when asked and changes no delivery state or
   timestamp. It describes now, cannot predict a reply, and leaves mail queued for a returning
   agent. A recovering Execution remains active until its recovery deadline, as above.

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

An `auto` claim means "this session has uncommitted changes here". When a shell command takes a
path out of the session's git status, by commit, stash or restore, the reconcile releases the
session's own `auto` claim on that path in the same transaction, and wakes whoever waited for it.
An `explicit` claim, and every other session's claim, stays. Committed but unmerged work is
git's business, not a claim's. This happens only at the session's next shell reconcile: a commit
made outside the session, or an edit-tool claim on a path no shell command ever saw dirty,
keeps its claim until it expires.

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
host plus path. A known root qualifies an alias (ADR 012): a unique repository with
that remote and root can join automatically even when the remote also names older
histories. Distinct repositories with the same remote and root remain ambiguous;
registration never chooses one by row order. Local and shared-git-directory aliases
are considered together, preferring a unique exact root. A request with no root
cannot distinguish histories and requires unambiguous continuity. An unknown-root
candidate can learn a known root only when no known root contradicts it.
When historical local aliases are ambiguous, an unknown-root request may reuse
the outcome of a prior unknown-root identity resolution for that host, canonical
path, shared git directory and remote (ADR 012). Without that record, the first
such request allocates one isolated repository; it never chooses an ambiguous
historical row. A known-root resolution clears a checkout outcome pointing
elsewhere. Refreshing an active Execution does not redirect checkout continuity;
the commit guard uses the same resolution as registration. This cannot discover
an unknown rewrite, join hosts or repair existing splits.
`git@host:owner/repo.git`, `ssh://` and `https://` are one remote. That covers the normal case across machines. Spellings that depend on one machine,
such as SSH aliases, `insteadOf` rewrites and relative remotes, stay scoped to that host and
path. A match on root commit alone, which is what forks look like, and unresolved
contradictions are only suggestions for a human. Without a remote the identity is
local to the path. Repeated registration learns aliases but preserves its active
Execution, mailbox and claims. A changed known root requires operator repair.

`iris admin merge --from <id> --into <id> [--dry-run]` explicitly repairs an existing
split, using the owner token and one transaction. It preserves mail, history,
Execution identities and all alias spellings. Mailboxes stay distinct and retain
their names; colliding mailbox slots receive deterministic free slots. Live
overlapping claims between unrelated Executions, incompatible repository policies
and distinct nonempty contacts refuse the merge. The destination's identity wins;
a source-only contact transfers. Dry run reports the move without expiring leases
or changing any record. It is a snapshot, so applying checks again.

Hooks work out the worktree root and remote by reading `.git` files directly, because spawning
`git` on each hook is too slow, particularly on Windows. Root discovery follows at
most 128 loose first-parent commits; packed, long or unreadable histories report
an unknown root. It cannot distinguish a history rewrite in that case. Paths are stored repository-relative
with forward slashes, with a per-repository case policy, and `..` escapes are rejected.
Antigravity runs hooks in `.agents/`, so its adapter takes the root from `workspacePaths`.

### D12. Shell edits

A shell command is visible only as a string, so a pre-tool hook cannot tell what it will
touch. After a shell tool, the hook compares `git status --porcelain -z` with the previous
state under a time limit, and warns about paths claimed from another worktree. Only paths
still changed after the command count: one that left the status was committed, stashed or
restored by git in this worktree, which changes nothing in another. It detects, it
cannot undo. In a shared worktree attribution is impossible, so it stays silent there.
At the time limit Iris kills git's whole process group on Linux and macOS, so the status that
git runs inside a submodule stops with it. A process that has left the group, or work the kernel
will not interrupt, can still run past the limit, and the hook sends the kill without waiting to
see it land. On Windows only the process Iris started is killed, so what it started can outlive
the limit; that has not been measured there.

Status compares each path's two-letter code, not its content. A second change to a path that
was already dirty therefore goes unseen. A submodule is a single path in its superproject: it
shows ` M` once its tracked content changes, and nothing finer. Files that are only untracked
inside a submodule are left out (`--ignore-submodules=untracked`). Scanning large submodules
for them took one measured repository from 20 ms to 5.5 s, far past the budget, and a
reconcile past its budget detects nothing at all. A command run inside the submodule is
reconciled against the submodule's own repository, untracked files included.

[ADR 018](adr/018-reduced-reconcile-scope.md) adds a reduced snapshot, for a worktree whose full
status does not fit its budget. Its status runs with `--ignore-submodules=dirty`, which also
hides tracked edits inside submodules, and the snapshot names the gitlink paths in the index
that status read. The hub takes one only on `POST /v1/shell/scoped`, which a hub from before it
refuses, and compares two snapshots only when their scopes are equal, the submodule paths
included. Any other baseline is replaced and nothing is attributed. In reduced scope a named
submodule path that the status does not report is unknown, not clean. Nothing is released or
renewed for it, and an automatic claim there keeps its expiry. Reduced baselines are stored
apart from the full ones, which are all that an older hub reads, and this version clears them
whenever it starts. The first reduced snapshot after a start therefore only sets a baseline.

A worktree's breaker, a file beside the session memory, suggests the scope. It trips on four
facts: a full status started with at least half the budget left, had not passed the output cap,
and was still running when the reconcile's own deadline passed, the hook itself not cancelled.
A git that has just exited while its output is still being read can trip it falsely. The breaker
then suggests reduced scope for a minute, doubled after each full attempt that fails again, up to
ten minutes, and a full status that finishes resets it. Hooks running at once keep it only
approximately. A hook takes the suggestion only when its hub said in an answer that it takes
reduced snapshots, nothing in its environment points git at another repository or index, and the
index exists. It sends the snapshot only when the index read before its gitlinks and after the
status is one generation, by device, inode, size, both times and the checksum at its end, and
never resends a failed scoped call on the plain route. `iris doctor` shows what each breaker
suggests. Windows takes no reduced snapshot and keeps no breaker.

`iris guard` is a pre-commit check, installed per repository on request. It refuses a commit
of paths held by another Execution. It is the strongest check Iris has, and it is cooperative.

### D13. Security

ADR 014's named console uses a separate typed account path in the store.
It rechecks the persisted session, authentication version, enabled role and
repository predicates in the same transaction as the object operation. Members
default to no access; strict `ro` grants read, and `rw` adds send/reply with `rw`
on the explicit origin and every immutable agent recipient. Operators retain
global console authority but gain no owner-token powers. Access edits revoke
sessions atomically and cannot remove the last enabled operator. The named
transport boundary refuses mixed bearer/session credentials and bearer-only
browser requests. The named path issues opaque, hashed database sessions with a
twelve-hour absolute expiry,
bound CSRF challenges, password-attempt limits and owner provisioning/recovery.
Password work runs outside the store lock; issuance and password changes recheck
the verified account snapshot inside their committing transaction. Password and
access changes revoke every session. The frontend has login, logout,
profile and password forms, and an operator access editor for roles, disabled
state and explicit repository grants.

[ADR 019](adr/019-console-activation.md) makes browser authentication named-only.
Startup loads an existing independent protected console key from
`<database>.console.key` or `--console-key-file`; a missing, unreadable, malformed
or owner-token-reusing key closes the browser gate while agents, authenticated
doctor uploads and owner setup/recovery/import continue. An enabled operator is
also required. Explicit owner activation verifies that operator's bounded
password, rechecks its snapshot transactionally and clears console sessions
before preparing key material. Success requires a restart and issues no cookie;
an already active process refuses activation without clearing sessions. Ordinary
restarts retain absolute expiry. Imported-password verification does not impose
the fifteen-code-point minimum used for new passwords. Old links/cookies and
bearers grant no browser authority; `iris ui` opens the ordinary origin without
reading the machine token. Restore and rollback must preserve that boundary;
see [the operational procedure](console-activation.md).
For create/reset/activate, the CLI removes exactly one final LF or CRLF as input
line framing before the password bounds; every other byte is preserved, without
general trimming or normalization. The server still verifies submitted password
bytes exactly. Before the first deployment closes browser access, the owner
confirms readiness in their own terminal and remains present to activate and
sign in after the backup and approved import review; whoever deployed performs
the second restart. Agents prepare and rehearse; real owner activation is not
delegated.
Legacy link/cookie grants were process maps, not SQLite rows: restart discards
them, while activation clears named `ConsoleSession` rows. Restoring a database
can revive named credentials/grants/sessions, not those legacy maps. An older
binary can mint fresh legacy grants and must remain behind the rollback filter.

ADR 014's importer is an owner-only CLI/API path independent of that account
gate. The CLI reads an explicitly described table from a closed, fixed,
checkpointed DELETE-mode export; WAL headers and journal sidecars are refused
before SQLite opens it. Immutable/read-only settings prevent SQLite shared-memory
writes. The owner must keep that export fixed; Iris cannot enforce this or take
a live-source snapshot with immutable locking/change detection disabled. Only
the hub writes accounts. Dedicated dry-runs validate without
expiry bookkeeping or any persisted change. Apply repeats validation and target
collision checks in one immediate transaction with an internal versioned source
fingerprint ledger. Exact replays preserve later Iris edits; changed inputs
refuse instead of overwriting. Imported members have no grants. A separate
reviewed transfer accepts only explicit viewer-to-`ro` repository mappings,
never source project operators or inferred `rw` authority. Import does not
activate account login; that requires separate owner verification and restart.

The trust boundary is the OS user on a host, plus a token per host across hosts. The token
separates users and hosts, nothing finer. Any process running as that user, including a hook
from a repository the user trusted, can read it and act as any Execution. Agents under one user
are not isolated from each other, and Iris does not claim otherwise. Inside that boundary Iris
relies on leases, delimited bodies and text-free wake frames.

- **Credentials.** The token is in a user-only file: mode 0600, and an owner-only DACL on Windows.
  Never in hook environments.
- **Owner token.** The hub's own token file is the owner credential. Only it may create, list or
  revoke host tokens, merge repositories, or remove records with `iris admin forget`;
  a host token gets `forbidden`.
  Otherwise a host could mint itself more tokens or lock other hosts out.
- **Network.** Bind to loopback by default, and see D1 for remote hubs. Check `Host` and `Origin`
  on every route, against DNS rebinding and cross-site requests from browsers. No CORS. The web
  UI signs in with a named password and uses an `HttpOnly`, `SameSite=Strict`
  opaque session cookie, also `Secure` for HTTPS. Plain HTTP named authentication
  requires literal loopback; a public origin requires the trusted loopback TLS
  proxy configuration in ADR 002.
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
`iris service status` exits as `systemctl status` and LSB do, on every OS, so that scripts can rely
on it: 0 when the hub runs, 3 when it is installed but not running, 4 when it is not installed,
and 1 when the state cannot be read.

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

Codex records a review in its own `config.toml`, as one `[hooks.state."<hooks.json>:<event>:<group>:<hook>"]`
table with a `trusted_hash` per hook (measured on 0.167.0). Doctor reports Codex as ok only when
every Iris hook has such a table, keyed by the `hooks.json` path Codex builds from `HOME` or
`CODEX_HOME` and by Iris's position in each event. Otherwise it asks for the review, as before.
It does not check the hash, which could not be reproduced, and a table left by an earlier hook at
the same position would also pass. So the ok row says "hash not verified" and names
`iris doctor --live --client codex` as the proof that the hooks run. Iris never writes those
tables, because they are Codex's. Doctor also reports an Iris MCP table written before
`env_vars` and `default_tools_approval_mode` existed, and gives `iris integrate codex` as the fix.

Codex's `[mcp_servers.iris]` table carries two more keys, both measured on Codex 0.167.0 and both
removed with the table:

- `env_vars = ["IRIS_HUB_URL", "IRIS_TOKEN_FILE", "IRIS_EXECUTION"]`. Codex passes the full
  environment to hooks but a filtered one to MCP servers. Without this an `IRIS_HUB_URL` override
  sent the session's claims to one hub and its messages and `whoami` to another.
- `default_tools_approval_mode = "approve"`. This approves the Iris server's tools without asking,
  and applies to no other server. Without it Codex asks before every Iris call, and with
  `approval_policy = "never"` it refuses them all. Iris tools act only on the hub, never on local
  files or a shell. The `writes` mode would ask only for tools not marked read-only, but Iris's
  tools carry no annotations, so `writes` refused even `whoami`.

Antigravity gets one rule for the same reason. In print mode agy 1.5.3 denies every tool that
needs a permission and stops with no output, so every Iris call from a headless session was
refused (measured). `iris integrate antigravity` adds `"mcp(iris/*)"` to `permissions.allow` in
the user's `~/.gemini/antigravity-cli/settings.json`, and `--remove` takes out exactly that entry.
The rule covers the Iris server alone. agy reads it only from the user settings, not from a
workspace `.gemini/settings.json` (measured), so a per-repository rule is not possible.
`mcp(iris/whoami)`, `mcp(iris/*)` and `mcp(*)` matched; `mcp(iris)` and `mcp(iris:whoami)` did not.

`iris doctor` runs offline by default: hub reachable, token valid, client version, configuration
present and complete, trust state, unknown tools seen, last hook received, and a synthetic canary
through the configured hook command. Complete means equal to what `iris integrate` would write,
rendered in a dry run that writes nothing (gap review C3). JSON is compared by value, and the
user's own handlers are kept by that render, so neither counts as a difference. The canary must also
come back with doctor's nonce, which `iris hook` writes to stderr only for the canary session, so a
command that answers without running Iris does not pass. Every installed command form carries that
stderr; a test builds iris and checks all of them on each operating system in CI. `iris doctor --live` runs the real client, on its cheapest model where
that model edits through the client's file editing tool. Codex uses `gpt-6.1-terra`, which used
`apply_patch` in 8 of 8 runs. `gpt-6-luna` wrote the scratch file through Code Mode's `js` tool
in every measured run, and no pre-edit hook sees that, so the check would fail for a reason
unrelated to Iris. It is
billable, so it is opt-in: it names each client, binary and model first and asks, unless `--yes`
is given. Without `--client`, a client that still needs a person, such as Codex before its
hooks are reviewed in `/hooks`, is skipped with that reason: no trust is bypassed, and a run whose hook cannot fire
spends money to prove nothing. Doctor keeps current readiness separate from historical grades.
It checks the human step even when the canary fails: that failure stays visible, but does not
establish that the step is complete or permit a default live run of an unreviewed client.
Ready clients reported `silent` or `slow` still run: a silent grade may come from an old failure
in the log, and a slow grade means hooks ran but shell reconciliation often exceeded its budget.
Those histories never make a client ready when it needs a person or is not configured.
Every client runs on a named model, never its default, which can be expensive. Cursor gets
`--trust`, which trusts only
the scratch repository doctor created. Without it, a headless `cursor-agent` prints "Workspace
Trust Required" and exits 0 having run nothing. That is trust in a temporary directory Iris made,
not a bypass of trust in the user's own workspaces. Output is per client binary, not per vendor, because the Cursor IDE and
`cursor-agent` differ.

The offline grades remain visible with `--live`, but its exit status reflects the selected
live runs and the configured hub and token checks. It exits 0 only when at least one client
runs, every live result passes and the hub and token are usable. A failed live result, no
eligible client or a declined run exits 1. Other clients' offline grades do not fail a live
run. Plain `iris doctor` still exits 1 when the hub or token is unusable or any client is
`silent` or `slow`.

Each live run happens in a scratch repository under the temporary directory, with no commit and
an origin of `https://example.invalid/iris-doctor-live/<random>.git`, never in one of the user's
repositories. The client talks to a hub that doctor starts in-process on loopback with its own
database and token, through `IRIS_HUB_URL` and `IRIS_TOKEN_FILE`, which take precedence over any
`client.json`. Both are deleted afterwards, so a client that passes its environment to its hook
and its MCP server leaves no records on the configured hub, and nothing needs `forget`. Codex
filters its MCP servers' environment, so its integration names these variables in `env_vars`.
An integration written before that keeps its MCP server on the configured hub until
`iris integrate codex` runs again (measured on macOS). The prompt asks the model to call `whoami` and to rewrite one file. The run reports
four facts: the hook registered the scratch session, the pre-edit hook was granted the claim,
the reply carries the scratch repository's id, which `whoami` returns and the hook's session context does not (the execution id is in that context, so echoing it would prove nothing), and the file holds the requested
line. Only sessions that the named client's own hook registered count, because Grok also loads
other clients' hooks (D5). If the scratch hub sees nothing from that hook, either the client
stopped before running it, which Cursor does with exit 0 in an untrusted workspace, or a client
that hides its environment from its hook reached the configured hub instead. Doctor then prints
the client's output and the marker origin, so the hub's owner can find that repository and
remove it with `iris admin forget`. Naming a client with `--client` selects only that client
and allows a `needs_human` client to run. The plan names the original readiness step doctor
could not confirm, even when the displayed grade is `silent` or `slow`; a missing hook repeats
that warning. Doctor does not claim the step was verified, and the override grants no trust.
An unconfigured client or a readiness result reporting a missing binary remains excluded,
including with `--client`.
Codex runs with `sandbox_mode="workspace-write"`, `approval_policy="never"` and the Iris tools approved for that
run only, never with full access. Codex records each directory it runs in as trusted, in
`config.toml`. Doctor removes that scratch table afterwards, but only if removing it gives back
the file's bytes from before the run. A file that changed in any other way meanwhile is left
alone, and the report says so. The run is limited per
client, three minutes by default.

### D15. Distribution

Names: the product and binary are `iris`. The bare package name is taken in Homebrew Cask,
Scoop Extras, AUR and npm, so commands always use a namespace: `mateusz-klatt/tap/iris`,
`MateuszKlatt.Iris`, and a dedicated Scoop bucket. `irisd` is free everywhere and stays in
reserve for any channel that needs a bare name.

| Stage | Contents | Needs |
|---|---|---|
| 0 | CI on three OSes, GoReleaser snapshots | Now, in a private repository |
| 1 | Releases, checksums, cosign keyless, SBOM, Homebrew tap, Scoop bucket, GHCR | Public release assets |
| 2 | winget, signed apt and dnf through Cloudsmith OSS, GitHub attestations | Public release URLs; a public repository for attestations |
| 3 | Apple notarization, Windows signing | $99 a year; SignPath Foundation, or Certum Open Source from €49 |

Stage 1 is prepared but inert. `.github/workflows/release.yml` runs only when the owner pushes a
version tag in this repository, and builds a draft release with cosign keyless signatures of the
checksums, an SBOM per archive and GitHub build provenance. Nothing is public until the owner
publishes the draft, no image is pushed, and Homebrew, Scoop and winget stay skipped. CI checks the
configuration and the SBOMs on every pull request; keyless signing needs a release's OIDC identity,
so its first real run is the first tag.

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

The ADR 014 named account path applies repository visibility before search,
grouping, counts and pagination. Cross-repository envelopes retain the sender
name and body while hiding outside recipient state and repository metadata.
Authorship is immutable message metadata, not a continuing access grant. Named
sends retain account-scoped retry results for 24 hours and reauthorize current
objects before replay; repository repair refuses member grants of either mode.
Per-account persisted revisions exclude hidden repository activity and global
event cursors. Account sockets validate their session before each frame;
frame admission serializes with revocation, and access changes wake idle sockets
to close. A previously admitted frame may already be in transit. The frontend
distinguishes an ordinary signed-out named session from
`X-Iris-Console-Mode: closed`, which reports unavailable account authentication.
The closed state can also reflect an untrusted browser transport; the frontend
asks the user to contact the owner without assuming activation is the cause.
ADR 019 removes link creation/exchange and the old cookie authority. Explicit
owner activation prepares a key for the next process; it never hot-opens a console.

The transferred account forms retain the owner's export provenance (ADR 014's
inventory). Named-account locale choices are saved with profile revisions and
take precedence over browser preferences, which are used only while signed out.
Logout, password changes and session revocation unmount the console, close its
stream and discard prior-account drafts/data and cached locale. Reconnect checks
also compare the current account and authentication version before admitting a
new stream. HTTP reads carry the mounted session's synchronizer token, and the
hub rejects a token/cookie mismatch before serving data. Thus another tab
replacing the shared cookie causes the old console's next read to reset its
state even while its socket is live. Responses overtaken by a local session
change are discarded. Password validation counts Unicode code points and UTF-8 bytes.

The access editor is available only to named operators. Its account list
includes explicit empty or `ro`/`rw` grants with the same transaction's edit
revision, never credential fields. Access saves return the canonical updated
snapshot. Conflicts reload current state and discard stale edits for review;
the console and store both protect the last enabled operator, with the store
remaining authoritative under concurrent changes. Editing one's own access ends
that browser session. These controls do not change the startup gate; use the
separate owner activation/restart procedure in ADR 019.

The account overview describes each visible repository with `console_access`:
`ro` or `rw`. Member compose/reply requires a selected writable origin, explicit
Agent IDs in writable repositories and a parent in the scoped thread. Members
never call the shared read/ACK action and have no revoke/diagnostics/admin
controls. These controls describe current capabilities; every request remains
subject to store authorization. Messages show immutable person attribution
alongside the shared human transport and explain that replies use a shared mailbox.

The new-message recipient picker stores Agent IDs, not display names, and sends those IDs
through the existing address resolver. If a selected ID disappears from the directory, review
and send are disabled until the operator clears and selects again. An agent recreated under
the same name is a different recipient. Search changes visibility without changing selection.
Thread replies retain their explicit address field.

The React frontend lives in `web/`, is built by Node at release time and embedded with
`go:embed`. It communicates with the Iris API.
The first screen with no sessions shows a live wait for the first session, next to each client's
status from the last doctor run (hook received, not configured, needs a human step), with a button
that reloads the last report. Doctor itself runs on each host, where the client configuration is,
so the console cannot run it; it says to run `iris doctor` on that computer, and shows when the
report was checked (gap review C4). A silent client must show its cause, not an empty table.

### D17. Removing records

Records outlive the sessions that made them, and measurement runs leave repositories and
agents nobody wants. `iris admin forget` removes one repository, or one agent whose sessions
have ended, with everything that refers to it, in one transaction. It is the only deletion
besides event pruning, so it is narrow: owner token only, a dry run first, and a refusal while
anything it would remove is in use.

R4 still holds. A delivery is never lost by accident; this removes it because the owner asked,
and reports how many. The event log stays consistent: events of the removed agent go, and other
agents' events about removed deliveries read as settled, so a watcher is neither woken nor reset.
The search index is updated in the same transaction.

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

Codex can also write files from JavaScript. Its Code Mode `js` tool, `mcp__node_repl__js`, runs
code such as `fs.appendFile` (measured on 0.167.0). `gpt-6-luna` used it instead of `apply_patch`
for an ordinary edit, and with `js_repl = false` set. Codex fires PreToolUse and PostToolUse for
it, with the code in `tool_input.code`. Iris cannot tell from code which files it will write, so
the tool is reconciled like a shell: a write is detected afterwards, claimed automatically and
reported with a warning. It is not refused beforehand. This is detection, not prevention. A write
into a path someone else holds is reported, not stopped, and the pre-commit guard remains the
last checkpoint.

Claude Code on Windows has a `PowerShell` tool beside `Bash` (measured on 2.1.319). It takes a
command in `tool_input.command` and writes files as freely, as `Set-Content` does, so it takes the
same shell path: a write is detected afterwards and reported, not refused beforehand. Until this
was measured, the tool was unknown to Iris, so its writes went unreconciled and unreported.

## 4. Known gaps

- The test suite passes on Windows and macOS in CI, but no real client has run there yet. Hook
  spawn cost under Defender, `cmd /c` quoting, and whether fail-open holds on those systems all
  need real runs before any claim of support.
- Whether plugin monitors survive `--resume` and `/clear`, and at what rate monitors are stopped.
- Whether `SessionStart` context on Grok and Kimi reaches the model at all.
- The nearest-process rung has not been tried on a Codex TUI session or a Cursor IDE session.
- A tool result the client drops after the MCP server confirmed the fetch leaves that mail quiet
  until something else makes the model look (ADR 010). A response lost before the MCP server
  holds it is recovered.
- Shell reconciliation already claims only paths that still differ from HEAD or from the index
  after a command, so a file git restored is never claimed. Two kinds of noise remain. A path that
  left the status, because git restored it in this worktree, still draws a warning when someone
  else holds it. And a path that differs only in the index, after `git reset --soft` or
  `git cherry-pick -n`, is claimed although the working tree did not change. Claims hold across
  checkouts of a repository by design (ADR 003, D10), so such noise reaches the other checkout.
  claude-laptop-iris owns that work.
- The guard's refusal names a claim's holder, but not the holder's branch or worktree, so the
  person in another checkout cannot tell where to ask. Same owner as the item above.
- Codex's built-in clock, collaboration and web tools are classified by the names iris hook
  logged as unknown on a real host. No payload capture backs those names yet. Codex runs a new
  hook only after a person trusts it in `/hooks`, so the capture waits for one. Claude Code's
  Monitor, TaskStop and ScheduleWakeup are backed by captures from 2.1.319 on Windows.
- Claude Code's Monitor is reconciled like a shell, but its PostToolUse arrives at launch:
  measured on 2.1.319, 590 ms after the call, carrying only the task id, while the command ran
  on. A file its command writes later is not reconciled or claimed, and the same holds for Bash
  with `run_in_background`. The pre-commit guard is the checkpoint for those writes.

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
  An ack from before a restart cannot settle a fetch after it.
- A restart wakes no one for mail a live session already fetched. Mail whose fetching Execution is
  lost is requeued once, wakes its recipient once, and goes to the agent's next session.
- Cursors never regress. A reconnect neither loses committed events nor exposes another recipient
  or any content. An expired cursor returns a reset with a snapshot.
- Clock skew on clients cannot extend a lease.
- Repeated restarts cannot extend recovery. An unattached Execution is lost at its deadline.
- An orphaned heartbeat cannot preserve ownership. A second, unregistered session in one process
  cannot bind to the first.
- An ambiguous URL cannot join a repository automatically.
- A policy a client cannot express never becomes an allow.
- For every client, a crash, a timeout and a missing binary all leave the tool usable.
