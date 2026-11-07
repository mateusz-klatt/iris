# Cooperative checks

`iris guard install` installs a pre-commit checkpoint for the current repository.
Git resolves the hooks directory, including linked worktrees and `core.hooksPath`.
An existing hook is saved as `pre-commit.iris-original` and runs after a successful
Iris check. `iris guard remove` restores it; removal refuses to discard edited hooks.
No hook is installed automatically. The hook runs the `iris` recorded at install, then `iris`
from Git's PATH if that path is gone. With neither it prints `Iris guard: iris not found; commit
not checked` and allows the commit, so an uninstalled iris cannot block commits. A hook written
by an earlier version, which found iris only on PATH and failed with 127 without it, is upgraded
by the next `iris guard install`; hooks Iris did not write are kept as the chained original.

Without the guard, an edit made through a shell past someone else's claim is only warned about
and can reach a commit. So a session that starts in a repository without the guard is told so
once, in the SessionStart context, and asked to suggest `iris guard install` to the user. The
model is never told to install it, because `.git/hooks` belongs to the user. Subagents are not
told again. To hide the note for one repository, run `git config iris.guardHint false`. The check
runs inside the hook's deadline, so it starts no git process. It reads the repository's own
`config` for `core.hooksPath` and that setting, and then the one hook file. A `core.hooksPath`
set only in the global or system Git config is not seen, so the note can appear even though a
guard is installed; the setting above hides it. `iris doctor` reports the guard of the repository
it runs in, using the hooks path that Git itself resolves: installed, not installed, or another
pre-commit hook without Iris. A missing guard does not fail doctor.

`iris guard check` reads staged paths, including both ends of renames and deletions.
It refuses paths claimed by an Execution in another worktree. Claims in the same
host/worktree do not block it. An unreachable hub produces a warning and allows the
commit. This is cooperative: `git commit --no-verify` bypasses the hook. It neither
prevents arbitrary writes nor resolves a conflict.

On clients with shell pre/post hooks, Iris records porcelain status before the shell
and compares the post-tool status with that hub-stored snapshot, within 300 ms.
Post-tool failures also reconcile because a failed shell can have written files.
Warnings use the client's context output. Multiple live Executions in the same
host/worktree suppress attribution and warnings. Non-shell tools do not scan status.

This detects changes and cannot undo them. Without a prior snapshot, the first post
hook establishes a baseline. A file rewritten while retaining the same porcelain
status cannot be distinguished from its previous state. Ignored files do not appear
in Git status. Timeouts, large status output and hub failures leave the tool usable.
