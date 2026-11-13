# Client research

Iris depends on behaviour of seven fast-moving CLIs, so the architecture rests on measurement,
not memory. On 2026-11-05 each client was consulted about itself, in an empty throwaway
workspace, under the rules in `consultation-brief.md`, with a live experiment on the installed
binary. Each one also reviewed the design from one assigned angle.

| Client | Version | Angle | Report |
|---|---|---|---|
| Claude Code | 2.1.318 | Run by the orchestrator | [claude-code.md](clients/claude-code.md) |
| Codex CLI | 0.166.0 | Domain correctness | [codex.md](clients/codex.md) |
| Copilot CLI | 1.0.105 | Release engineering | [copilot.md](clients/copilot.md) |
| Cursor Agent | 2026.11.05 | First-run experience | [cursor.md](clients/cursor.md) |
| Antigravity CLI | 1.5.1 | Portability | [antigravity.md](clients/antigravity.md) |
| Grok CLI | 1.0.77 | Security | [grok.md](clients/grok.md) |
| Kimi Code | 2.5.1 | Adapter architecture | [kimi.md](clients/kimi.md) |

Reports are kept as written, apart from anonymised paths and a correction note at the top where
a later measurement overruled a claim. A commit hash in a report, or in an ADR that cites one,
names the version that was measured. Some of those commits are no longer kept in the repository,
such as `bc1eae9` and `df1d873`, so such a hash is a label, not a revision to check out. Captured payloads are in `internal/hook/testdata/`.
`reference-schema.sql` is Codex's proposed model, on which `docs/architecture.md` D9 is based.

The [shared-worktree experiment](shared-worktree.md) measures Claude and Codex running
concurrently in one checkout, including edit contention, shell attribution and the shared index.

The [Codex attention measurement](clients/codex-attention.md) records how Codex
0.167.0 takes context from hooks, continues at Stop, waits in an MCP call, and is woken through
an app-server and through `codex queue`, with the lifetime of a thread's MCP child.
[ADR 017](../adr/017-wake-codex-by-queue.md) rests on it.

The [MCP child queue record](clients/codex-mcp-queue-2026-11-10.md) measures whether a Codex
thread's MCP child can itself queue a turn for its session, with the default home and with
another. It also sets each tool call's session id beside the session id of the hooks of the same
turn, in nine runs that include `codex exec resume`, `codex exec fork` and a sub-agent. ADR 017's
binding rung rests on it.

The [Copilot extension wake measurement](clients/copilot-extension-wake-2026-11-12.md) ran Copilot
CLI 1.0.109's real extension loader with `iris watch` and a model: an idle and a busy session,
`/clear`, resuming with unread mail, shutdown, and how the installer treats a file edited by hand.
[ADR 024](../adr/024-copilot-extension-wake.md) rests on it.

The [gap re-audit of 2026-11-09](gap-reaudit-2026-11-09.md) checks every finding of the
[gap review of 2026-11-07](gap-review-2026-11-07.md) again, against `main` at `4840765`: 28 closed,
7 partly addressed, 1 open.

The [submodule reconcile record](submodule-reconcile-2026-11-09.md) measures what `git status`
reports for a submodule under each `--ignore-submodules` mode and under a repository's own
settings, and that a submodule can be present without a declaration in `.gitmodules`.
[ADR 018](../adr/018-reduced-reconcile-scope.md) rests on it.

The [Git process and index follow-up](adr018-git-process-index-2026-11-09.md)
measures descendant lifetime after cancellation and index inventory across small conflict,
sparse, split, linked-worktree and nested-submodule fixtures. It distinguishes the Linux
measurements from unverified concurrency, portability and production-cost assumptions.

The [reduced scope run on WSL](adr018-reduced-scope-wsl-2026-11-10.md) measures real shell hooks
in a superproject whose full status takes about 0.9 s: the cold trip, 30 commands in reduced scope,
the full attempt after the cooldown, and 30 commands on a binary without the hook's side. It
records wall time, CPU including killed git processes, attribution and what was left running.

The [console activation rehearsals of 2026-11-09](console-activation-rehearsal-2026-11-09.md)
record what a build of the activation change showed in two browsers, after a restore and behind
an Apache that closes the console for an older backend, and what a candidate is checked for.
They ran on a modified checkout. [ADR 019](../adr/019-console-activation.md) refers to them.

The [candidate run of 2026-11-10](console-activation-candidate-2026-11-10.md) records the image CI
built for the activation change: started closed, activated with the password on standard input,
restarted, signed in to from a browser, and started once on a copy of a production backup. A
second run went back to the version before it on the same database and then started the candidate
again. It does not repeat the access checks.

The [access and import run of 2026-11-10](console-access-and-import-2026-11-10.md) ran the binary
from the final head's image: an import on the version before, the switch, owner setup and
activation, the access checks in two browser contexts and by keyboard in a narrow and a wide
window, and going back with another attempt.

## Review of the architecture

The synthesis then went back to the clients as a plan review. Antigravity was left out of this
round: its quota cost per run was too high for the value of a second opinion. Kimi's review did
not finish, because of the account's limit of 3 requests per minute, so D3 to D6 were reviewed by
Codex, Grok and Cursor only.

Accepted changes include: refusing plain HTTP off loopback; recovery reservations in place of
reviving leases at restart; stopping heartbeats on orphaning; all-or-nothing multi-path claims;
restricting the nearest-process rung to single-session processes; rejecting URL spellings that
depend on one machine; confirming the runner by comparing session ids; rotating wake tickets;
never weakening an unsupported `ask` into an allow; ignoring lockfiles for auto claims;
actionable deny messages; and setup that reports human steps instead of a false green.

Rejected or changed advice:

| Advice | From | Outcome |
|---|---|---|
| Default the conflict action to `warn` until the user opts in | Cursor | Kept `deny`. Refusal before an edit is the point of Iris (R5). The noise it feared is addressed by `claim.ignore`, a 10-minute auto claim and an actionable message |
| Drop process ancestry entirely | Codex, Copilot | Kept as one restricted rung. It is the only automatic option on three clients, and it has no authority |
| Guard that returns the hook status unchanged | Grok | Kept the fallback on any non-zero status. A deliberate deny exits 0 on these clients, and R8 requires a crash to leave the tool usable |
| Warn on all non-Iris project hooks in doctor | Grok | Deferred. It is a general security audit, not coordination |
| Mandatory launcher, `iris run <client>` | Codex, Copilot | Optional later. Users start clients themselves, and the installation must stay seamless |
