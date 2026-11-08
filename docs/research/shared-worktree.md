# Claude and Codex in one worktree

Measured on macOS on 2026-11-08 with Iris `993e545`, Claude Code `2.1.319`
(`claude-opus-5-5`, medium) and Codex CLI `0.167.0` (`gpt-6.1-sol`, medium).
This is one bounded headless run, not a stress test or a measurement on Windows or Linux.

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

## What this proves and what it cannot prove

Concurrent editing of separate files works in this setup. Pre-edit check-and-claim also
protects a contested path between unrelated Claude and Codex sessions in the same worktree.
This protection differs from the shell and pre-commit checks: it is not disabled merely
because the worktree is shared.

Shell attribution is deliberately suppressed when multiple active Executions share a
host and worktree, as D12 describes. The guard also excludes claims from that same host
and worktree. Neither mechanism prevents one agent from staging another agent's changes.
The experiment did not test an actual mixed commit, concurrent Git locks, branch switches,
stale edit retries, or recovery after a crash.

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
