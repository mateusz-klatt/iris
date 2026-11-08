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
a later measurement overruled a claim. Captured payloads are in `internal/hook/testdata/`.
`reference-schema.sql` is Codex's proposed model, on which `docs/architecture.md` D9 is based.

The [shared-worktree experiment](shared-worktree.md) measures Claude and Codex running
concurrently in one checkout, including edit contention, shell attribution and the shared index.

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
