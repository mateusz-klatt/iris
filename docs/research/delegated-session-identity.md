# Codex exec split between MCP and hooks

Measured on Linux on 2026-11-09 with Codex CLI `0.167.0`, `gpt-6-luna`,
and Iris baseline `fee6166576e8f46de56fe8319b7b8fa646d6c6ae`.

## Setup

A controller launched a real `codex exec --json -m gpt-6-luna` child in the
existing linked worktree, on `fix/delegated-session-identity`. A separate
loopback Iris hub, database and token isolated the experiment from the configured
hub. `IRIS_HUB_URL` and `IRIS_TOKEN_FILE` pointed at that hub; inherited Execution
and client session identifiers were removed.

The child used a temporary `CODEX_HOME` with the same enabled Iris hook events,
their command paths changed to the scratch Iris binary, and an Iris MCP server
using that binary. Its authentication file was a symlink to the existing Codex
authentication file. `--dangerously-bypass-hook-trust` permitted those inspected
temporary hook commands for this invocation. Installed configuration was unchanged;
hooks and the guard were not disabled. The child made no commits.

The controller was a Python subprocess launcher, not the original MCP bridge.
This preserves the relevant real `codex exec` parent of both Iris surfaces, but
does not independently measure every bridge's launch behaviour.

## Baseline sequence

1. `SessionStart` registered the hook's payload session id,
   `01a1c0fa-8b7d-78a3-9201-bb4b10c70a71`. The hub assigned
   `codex-home-iris-wt-identity2` and Execution
   `ex_5dfa3fbee9de21e81029f98a85195351`.
2. The first MCP `whoami`, without an explicit Execution argument, answered that
   binding was still in progress. The child waited two seconds and retried.
   MCP then reported ancestry rung 3, agent `codex-home-iris-wt-identity2-2`,
   Execution `ex_6fa5c85f24a52787c6a135b90b02b5d1`.
3. Read-only scratch database snapshots showed two active Executions with the same
   Codex process evidence and canonical worktree:

   | Registration | Session id | PID | Start time, Unix ms | `single_session` |
   |---|---|---:|---:|---:|
   | Hook | `01a1c0fa-8b7d-78a3-9201-bb4b10c70a71` | 2166220 | 1794239006820 | 0 |
   | MCP fallback | `mcp-2166220-1794239006820` | 2166220 | 1794239006820 | 1 |

4. MCP granted an explicit exact claim on
   `delegated-identity-probe-before.txt`, fence 1, to the fallback Execution.
   An earlier request with invalid kind `file` was rejected and granted nothing.
5. The child attempted one native `apply_patch` Add File. Its `PreToolUse` hook
   denied the edit, naming `codex-home-iris-wt-identity2-2` as the holder of that
   explicit claim. Codex's stderr recorded the blocked patch. The file did not
   exist afterwards. The child respected the denial and did not release the claim
   or write through another tool.
6. After a further 23-second wait, `whoami` still named the fallback and `peers`
   still listed both Executions as active. The split survived a heartbeat interval.
   The child exited successfully; the scratch hub was stopped.

The retained local evidence is `before/snapshots.json`, `before/codex.jsonl`,
`before/codex.stderr` and `before/hub.log` under the experiment's temporary
directory. These diagnostic artifacts are not checked-in fixtures; the directory
also contains private credentials and must not be published wholesale.

## Cause and correction

The process ancestry rung had matching evidence. The mismatch was in registration:
`internal/hook/runner.go` marked every Codex hook registration as
`single_session=false`, whereas `internal/binding` used `proc.SingleSession` to
recognise the Linux `codex exec` process and register its fallback as
`single_session=true` after the configured 15-second grace period.

The store's process binding query excludes Codex Executions without the
single-session marker. It therefore could not return the already registered hook
Execution. The busy first mailbox caused fallback registration to allocate slot 2.
Heartbeat adoption repeated the binding query while excluding the fallback, so
the same marker mismatch also prevented adoption.

The correction uses the existing process predicate when hooks register a session,
alongside the existing per-client classification. This gives Linux `codex exec`
the same positive classification on both surfaces and implements D6 without
weakening its ambiguity rule.

## Corrected run

The same controller, linked worktree, client version, model and hook events ran
against a fresh scratch hub with the corrected Iris binary. Its first MCP
`whoami` bound at rung 3 to the hook's Execution
`ex_70a2850df9a72c7fa8a3ba8566eb1df9`, agent
`codex-home-iris-wt-identity2`. The only Execution row had payload session id
`01a1c0fd-c6da-7902-9045-679aaf05c5f3`, PID 2172771, start time
1794239218640 and `single_session=1`; no fallback row appeared.

MCP granted explicit exact claim fence 1 on
`delegated-identity-probe-after.txt`. An initial invalid kind `path` was rejected
without a grant. The native Add File succeeded, and the live database still
showed the same explicit claim and Execution. SessionEnd ended that Execution
and revoked the claim. The child exited 0; the controller stopped the scratch
hub and the probe file was removed. The child omitted the requested follow-up
wait, so this run proves initial convergence and self-edit, not prolonged
heartbeat behaviour. Evidence is in the corresponding `after/` artifacts.

Before changing production code, the loopback regression failed on distinct
MCP/hook identities, the self-denial, a third fallback on ambiguous process
registrations and failure to adopt a late hook. The corrected regression also
uses a real temporary Git repository to check shell attribution and asserts no
hook failure was logged. It checks the original explicit fence is retained,
app-server requires an explicit handle, ambiguous sessions remain unbound, and
a late hook replaces the fallback on the next maintenance tick. Existing tests
cover handover retries and lost generations. No captured fixture was changed.

## Shared-worktree implications and limits

The split also satisfies reconciliation's count of more than one active Execution
on the same repository, host and worktree. That suppresses shell attribution even
though there is only one Codex session. This follows from
`internal/store/reconcile.go`; this run did not perform a shell write to measure it.

The guard's same-host, same-worktree exception is independent of the count of
Executions. Fixing the split does not remove that shared-worktree limitation. See
[the shared-worktree measurements](shared-worktree.md).

`proc.SingleSession` recognises Codex through Linux `/proc` and an exact `exec`
command position at `argv[1]`. This correction does not establish automatic binding
for macOS, Windows, another command-line arrangement, or a multiplexing app-server.
Ambiguous process matches still require an explicit Execution handle. Existing
active registrations retain their recorded process classification, so the change
does not repair an already split live session's metadata.
