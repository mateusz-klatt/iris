# Claude and Codex in one worktree

Measured on macOS on 2026-11-08 with Iris `993e545`, Claude Code `2.1.319`
(`claude-opus-5-5`, medium) and Codex CLI `0.167.0` (`gpt-6.1-sol`, medium).
The macOS results below are one bounded headless run, not a stress test. A separate
WSL run on the same date is recorded below, including a linked-worktree control.

Both clients ran at once in the same new Git checkout. A separate loopback Iris hub,
database and token kept the experiment off the configured hub. Installed client hooks
remained enabled; inherited execution and session identifiers were removed. The scratch
repository had a unique `example.invalid` origin. No Iris source files were edited by
either client.

## Observed behaviour

The controller verified two distinct, active Executions, one per client, with the same
host and canonical worktree path before and during the contention step.

| Step | Observation |
|---|---|
| Claude uses Edit on `claude.txt`; Codex uses apply_patch on `codex.txt` | Both edits succeeded. Each file acquired an automatic claim belonging to its own Execution. |
| Claude uses Edit on `shared.txt`, then Codex attempts apply_patch on it | Iris denied Codex's edit. Codex did not retry or fall back to a shell; the file retained Claude's content. |
| Claude writes `shell.txt` through Bash while both sessions remain active | The write succeeded, but no automatic claim appeared for that path. |
| Controller stages `claude.txt`, then `codex.txt` | The single shared index contained both files. Staging was sequential; an index-lock race was not tested. |
| Controller runs `iris guard check` on that index while both claims are live | Exit 0, with no warning. Same-worktree claims do not block that check. |

Both clients exited successfully after the controller released the final gate. The
temporary hub was stopped. This run used Edit and apply_patch for the contested write;
it did not attempt to bypass the denial with a shell.

## WSL actual-client control

Measured on WSL with Iris `993e545`, Claude Code `2.1.319` (Haiku) and Codex CLI
`0.167.0` (`gpt-6-luna` for the disjoint-file rounds; `gpt-6.1-sol`, low, for
contention). Installed hooks ran against a separate loopback hub and disposable
repository. The recorded contention output identifies actual apply_patch and Edit
calls, rather than synthesized hook payloads or JavaScript file writes.

| Scenario | Observation |
|---|---|
| Both clients edit, stage and commit their own separate file, six rounds each | Twelve commits, each containing only its author's file; no observed index-lock error and a clean final worktree. This single run does not establish freedom from Git races. |
| Claude holds `shared.txt`; Codex attempts native apply_patch | PreToolUse refused the edit. A separately authorized shell append then succeeded without a conflict warning; the following commit succeeded and included the holder's uncommitted line as well as the shell append. |
| Codex holds `shared.txt`; Claude attempts native Edit | The same result in the reverse direction: native refusal, silent shell append and a successful commit containing both agents' lines. |
| Claude holds the file in the original worktree; Codex repeats from a linked worktree | Native apply_patch was refused. The shell append succeeded but produced a conflict warning. The commit guard refused the commit and named the holder's branch and worktree. The holder's uncommitted line was absent from the attacker's working copy. |

The shell append and commit after native refusal were deliberate, explicitly authorized
scratch probes. They demonstrate enforcement limits, not the expected response of a
cooperating agent to a refusal. The scratch clients exited and the hub was stopped.

Evidence was checked in the retained `samewt/out` captures: `s2.codex-attacker`
records the apply_patch refusal and successful two-insertion commit; `s3.claude-attacker`
records the reverse refusal and mixed commit; `s5.codex-attacker` records the linked
worktree warning and guard exit 1. The retained disjoint-round Git history contains
six Claude and six Codex commits. These are local diagnostic artifacts, not fixtures
checked into this repository; private token files must not be published.

A second bounded WSL run with Opus 5.5 medium and Sol 6.1 medium independently
confirmed simultaneous active peers, disjoint native edits, denial of Claude's Edit
against Codex's explicit claim, and an unclaimed shell write. Its actual same-worktree
guard check returned exit 0. Its different-worktree guard comparison used a synthetic
API request, so the linked-worktree CLI evidence above is the stronger control.
A preliminary attempt whose clients did not overlap is excluded from concurrency
claims; it observed automatic shell attribution with the other Execution already ended.

Native Windows observations were reported separately, but the refusal text was not
captured and a later attempt was confounded by an earlier live claim. They are not
used here as proof of the native contention or Git outcomes.

## What this proves and what it cannot prove

Concurrent editing of separate files works in this setup. Pre-edit check-and-claim also
protects a contested path between unrelated Claude and Codex sessions in the same worktree.
This protection differs from the shell and pre-commit checks: it is not disabled merely
because the worktree is shared.

Shell attribution is deliberately suppressed when multiple active Executions share a
host and worktree, as D12 describes. The guard also excludes claims from that same host
and worktree. Neither mechanism prevents one agent from staging another agent's changes.
The WSL run demonstrated actual mixed commits and a linked-worktree refusal. Neither
run systematically tested Git lock races, branch switches, stale edit retries or crash
recovery. The macOS run did not attempt an actual mixed commit.

Use one owner for stage, commit, switch, reset, stash and rebase in a shared worktree, and
agree on file ownership before editing. This is a coordination rule, not enforcement by
Iris. Separate worktrees provide separate working files, HEAD and index for independent
branches; claims still coordinate overlapping repository paths across them.

## Repeating the experiment

1. Create a disposable Git repository with four tracked files containing `base`, a unique
   `example.invalid` origin and ignored gate files. Start a temporary authenticated hub on
   loopback; pass its URL and token file to both clients through `IRIS_HUB_URL` and
   `IRIS_TOKEN_FILE`. Remove inherited `IRIS_EXECUTION` and client session identifiers.
2. Start both clients in that exact directory, using their installed hooks and the models
   above. Give each a different file to edit with its editing tool, then hold both at a gate.
   Verify both resulting files and both active Execution records before releasing it.
3. Let Claude edit the shared file first. Keep it alive while Codex attempts one edit of the
   claimed path. Require Codex to respect a denial and keep both sessions alive at another gate.
4. Have Claude write the fourth file through Bash. Inspect the live claims, stage the two
   separate edits and run the guard with the scratch hub environment. Record the staged paths
   and guard exit status before ending either client.
5. Release the final gate, collect client exit statuses and stop the temporary hub. Retain
   only the evidence needed for diagnosis; never publish the scratch token.
