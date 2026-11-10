# 018: A shell reconcile that fits its budget beside heavy submodules

Status: accepted, 2026-11-09, by `claude-holzera-iris`. It rests on measurements and a proposal
by `codex-holzera-iris` in [the submodule reconcile record](../research/submodule-reconcile-2026-11-09.md);
that session challenged the drafts and signed off this design. Changes D12. The hub's side and
the hook's side are implemented. The implementation's changes to this decision are under
"Amendments from the implementation" at the end.

## Problem

After a shell tool the hook compares `git status` with the state before it, inside a budget:
300 ms against a hub on this machine and one second otherwise (D3, D12). A reconcile that runs
past its budget detects nothing at all. The status runs with `--ignore-submodules=untracked`,
which checks the tracked content inside every submodule.

Two observations from one production host, neither part of the record linked above. The session
working in a checkout with seven submodules, one of them holding market data, timed the hook's
status there with a warm cache: 1.04 s of wall time and 1.10 s of system time, against 0.03 s
with `--ignore-submodules=dirty`. And the hook failure log on that host held 376 entries from 86
minutes, every one a shell reconcile past its deadline, 364 of them at the status itself, from
pre-tool and post-tool hooks alike.

So in such a checkout a status is started twice for each shell command, is killed at the
deadline, and Iris detects nothing for it, top-level files included. What that costs in CPU for
each command has not been measured.

## What was measured

Git 2.53.0 in scratch fixtures; status captures only, no timing. The record has the commands.
For the hook's status shape, with the mode given as a command-line flag, the superproject
reports a submodule path as follows.

| Change | `none` | `untracked` (today) | `dirty` | `all` |
|---|---|---|---|---|
| Tracked edit inside a submodule, unstaged or staged there | shown | shown | hidden | hidden |
| Untracked file inside a submodule | shown | hidden | hidden | hidden |
| New commit inside a submodule | shown | shown | shown | hidden |
| That new commit staged in the superproject | shown | shown | shown | hidden |

A setting in the repository's own configuration behaves differently from the flag. With `-c
diff.ignoreSubmodules=untracked` and no flag, a `submodule.<name>.ignore` setting in
`.git/config` or `.gitmodules` overrides the default for that submodule, in either direction,
and a configured `all` still shows a staged commit that the flag `all` hides. The flag
overrides every per-submodule ignore setting.

A submodule can be present without being declared. With `.gitmodules` deleted, even by a
commit, or naming a wrong path, an initialised submodule recorded in the index with an edit
inside it is still reported by `untracked` and hidden by `dirty`.

## Decision

**1. A reduced scope when the full status cannot fit.** In reduced scope the hook runs the same
status with `--ignore-submodules=dirty`. It never uses `all`, which hides even a submodule
commit staged in the superproject.

**2. A snapshot says what it did not look into.** A reduced snapshot carries its scope: the
status invocation that produced it, and the submodule paths recorded in the superproject's
index, which are the paths whose content it did not check. The declarations in `.gitmodules`
are not used for this, because a submodule can be present without one. The hook reads that set
inside the same allowance. If it cannot establish the set completely and consistently, it sends
no reduced snapshot at all. An empty set is never a stand-in for an unknown one.

A snapshot's scope describes the invocation that produced it, not the breaker's state at the
moment it is sent. The hub compares two snapshots only when their scopes are equal, the set of
submodule paths included; any difference sets a new baseline and attributes nothing.

Within reduced scope a submodule path that the status reports goes through the ordinary
comparison of the status before and after the command: only a newly reported path or a changed
status code leads to a claim, a warning or a renewal. A submodule path that the status does not
report is unknown, not clean. Nothing is released for it, and an unknown does not renew a claim.
An automatic claim there keeps its ordinary expiry.

This is required for correctness. The hub releases a session's automatic claim when its path
leaves the status (D10). Under `dirty` a submodule leaves the status when its commit returns to
the recorded one, while edits inside it remain. Setting a new baseline at the switch does not
cover that, because it also happens between two reduced snapshots.

How the hook reads the set, and that one read of the index is enough in every state of a
repository, is not established. It is a condition for accepting the implementation.

**3. Only a hub that understands scope gets a reduced snapshot.** The hub names the capability
in its answer to registration. Without it the hook behaves as today and fails open past its
budget. A reduced snapshot is never sent as a bare status map, because an older hub would read
a missing path as a clean one.

**4. A breaker per worktree suggests the scope.** It trips on one thing: a full status that
started and ran past its allowance, classified where the process is started. A hub timeout, a
budget spent before git started, a git error and the output cap do not trip it. After a trip
the worktree uses reduced scope for a cooldown of 60 s, doubled after each failed full attempt
up to ten minutes, and a hook after the cooldown tries the full status again. A success returns
the worktree to full scope. Nothing retries in the background.

The state is a scheduling hint, not a ledger. It is a small file per worktree on the host,
beside the session memory, replaced whole and atomically, never locked, and a stale or invalid
one is ignored. Hooks that run at the same time read and replace it in no particular order: two
failures can count as one doubling, and a late writer can replace a newer result. Then further
full attempts can happen during a cooldown, or reduced scope can last longer than it had to.
Neither changes what a snapshot claims, because every snapshot states the scope it ran with.
Each hook process makes at most one full attempt. The research proposed a single cross-process
trial. It is left out: this accepts duplicate work to avoid coordination on the hook path, and
the aggregate cost of that work is not measured.

**5. A reduced status that also runs past its allowance fails open and submits nothing**, as a
full one does today.

**6. `iris doctor` shows the hint.** For each worktree with breaker state: the scope it
suggests, the reason, the last recorded full status that succeeded and when a full attempt is
next due.
It says that in reduced scope edits inside submodules are not observed, and that the hint does
not bind a hook that is already running. A reduced reconcile is not presented as a full one.

Not decided here: honouring a repository's own `submodule.<name>.ignore` settings by dropping
the flag. It would let a person mark one heavy submodule, but it changes precedence, and a
setting of `none` would widen the scan. It needs its own opt-in. Also open: whether a status
killed at its deadline leaves git descendants behind on each platform.

## What this cannot do

- In reduced scope an uncommitted edit inside a submodule, made by a command run from the
  superproject, is not seen. A new commit in a submodule is reported and handled as a change of
  the submodule path. A command run inside a submodule is still reconciled against that
  submodule's own repository.
- An automatic claim on a submodule path can outlive the change it stood for, until it expires.
- The superproject's own scan can still be too slow. Then nothing is detected, as today.
- The cooldown and the single trip are candidates, not measured values, and concurrent hooks
  keep them only approximately.
- The semantics were measured on one Git version. Nested submodules, sparse checkouts and
  conflicts were not exercised.

## Verification

For the pull requests that implement this:

- Hook: the breaker trips only on a full status that started and ran past its allowance; with
  hooks running one after another the cooldown doubles and is capped; with hooks running at
  once, a lost increment and a late writer in either order behave as the hint is described;
  each hook makes at most one full attempt; stale state is ignored; the state file is replaced
  whole; reduced scope never passes `all`; a reduced status past its allowance submits nothing;
  a hub without the capability never receives a reduced snapshot; a snapshot states the scope
  it ran with whatever the breaker file says when it is sent.
- Submodule set: a submodule present in the index with `.gitmodules` missing or wrong is in the
  set; a set that cannot be read completely yields no reduced snapshot; a changed set only sets
  a baseline.
- Store: a change of scope only sets a baseline; two reduced snapshots in which a submodule's
  commit returns while edits inside remain release nothing; an unreported submodule path is
  neither released nor renewed; a reported one is compared as in full scope, so only a newly
  reported path or a changed status code is claimed, warned about or renewed; full scope behaves
  as before; the shared-worktree rule is unchanged.
- Doctor reports the hint, the reason and the next full attempt.
- A measurement on WSL with a fixture sized like the production case: CPU for each shell
  command before and after, and no git process left after a status is killed at its deadline.

## Amendments from the implementation

Each change below was made by the pull request that implemented it.

**A route of its own for reduced snapshots** (the hub's side; amends decision 3).
- The agent API ignores fields it does not know. A `scope` field on the existing route would be
  dropped by a hub from before it, and the reduced map read as full.
- The capability alone does not prevent that: a hook that remembers it from an earlier answer
  can send to a hub that was rolled back since.
- So a reduced snapshot is accepted only on `POST /v1/shell/scoped`, which a hub from before it
  answers with `not_found`. The plain route refuses a request that carries a scope. The
  capability, named in every registration answer, still tells a hook when it may try.

**Reduced baselines do not cross a hub start** (the hub's side; adds to decision 2).
- Reduced snapshots are stored apart from full ones. Writing one kind deletes the other kind's
  for that Execution. A hub with this change clears the reduced ones each time it starts. An
  older hub leaves them in place, unread, until this version starts again.
- A hub from before this reads only full snapshots. After a rollback it never reads a reduced
  map as full, and its first reconcile finds no baseline.
- The cost: the first reduced snapshot after any hub start only sets a baseline and attributes
  nothing.

**The trip, from four facts** (the hook's side; amends decision 4).
- A trip needs all four: a full status whose git started; the reconcile's own deadline passed
  while its parent was not cancelled; the output cap was not passed; and at least half the
  budget was left when git started.
- Less than half does not mean the budget was spent before git started. Earlier stages took most
  of the budget, so this is not evidence that git was slow. The half-budget threshold is a
  candidate, unmeasured like the cooldowns.
- A reduced status past its budget never changes the breaker.
- Git counts as running while the reconcile is at its status stage. The hook leaves that stage
  as soon as a full status returns successfully, before it writes the reset, so a write that
  stalls past the deadline is not taken for slow git.
- The trip is written before the hook returns, because the process exits right after it. A
  write that stalls past the hook's own deadline loses the trip, and the output stays valid.
- What it cannot tell: a git that exited just before the deadline, while its output was still
  being copied, looks like one still running, since Go's `Wait` waits for that copy. It then
  trips falsely, which costs one cooldown of reduced scope. `gpt-6.1-sol` found this in the
  source of Go 1.27.3 and no outcome worse than a false or a missed trip.

**One index generation for both reads** (the hook's side; settles the condition in decision 2).
- The set is every entry of mode 160000, at any stage, in `git ls-files --stage -z`, sorted and
  without repeats, at most 500. It is read as a stream within bounds. A malformed or oversized
  listing, a mode no index entry has, or a git that fails, gives no set, never an empty one.
  Without `--sparse`, a sparse index is listed in expanded form. The tests read it in these
  states: a gitlink staged but declared nowhere, a removed one, a nested one, a conflicted one, a
  linked worktree's own index, a split index and a sparse one, with `.gitmodules` missing or
  wrong.
- Before the set is read and after the status, the hook takes six values from one open index
  file: device, inode, size, modification and change time, and the last 32 bytes. A published
  index ends with a checksum of its content, so two generations with different content differ
  there even when the other five values are equal. Any difference, or a value that cannot be
  read, means no reduced snapshot.
- Both commands run in the same way as the full status. Nothing is written in the git directory,
  and `GIT_INDEX_FILE` is not set.
- The hook declines, and runs full scope as before, when `GIT_DIR`, `GIT_WORK_TREE`,
  `GIT_COMMON_DIR` or `GIT_INDEX_FILE` is set in its environment, when it does not know the git
  directory, or when the index file does not exist.
- What it cannot tell: with `index.skipHash` the checksum is zeros, and only the five other
  values remain. Then a replacement that restores all five within one clock tick is not seen,
  and neither is an in-place rewrite within one tick that keeps the size. The known case is a
  sequence that `gpt-6.1-sol` inferred from the source of Linux 6.6 and Git 2.53 but did not
  reproduce. On ext4 with a 10 ms tick, a script publishes the index three times within one
  tick. The hook's first read and the listing fall between the first and second publications,
  and the third reuses the first one's freed inode at an equal size. Its cost would be one
  automatic claim released early.
- The check covers the index file only. HEAD and the files the status reads are read as git
  finds them. A filesystem whose inode numbers change gives a false difference, which only skips
  that snapshot.
- This replaced a copy of the index linked into the git directory, which the plan had proposed.
  The choice, made on 2026-11-10, and the remaining risk are `claude-holzera-iris`'s.

**What a hook keeps of the capability** (the hook's side; adds to decision 3).
- A hook keeps what each registration answer, or answer to the one-call form, says about the
  capability, in a file per hub URL beside the session memory. An answer that does not name the
  capability clears what the hook kept.
- A hook whose breaker says reduced, but which does not know whether its hub takes a reduced
  snapshot, registers first and lets that answer decide the scope.
- A scoped request that fails is never sent again on the plain route. On `not_found`, a bare 404
  or `bad_request`, the hook also forgets the capability. On `lost` it repairs the session's proof
  as a registration does, and drops that snapshot.
- A scoped request is sent once, as a plain reconcile is: the second copy that a hook sends of a
  slow hub call is never sent for either. A second copy of a snapshot would find nothing changed,
  and if it answered first the hook would lose the first copy's warning.

**Measured on WSL** (adds to Verification). The
[reduced scope record](../research/adr018-reduced-scope-wsl-2026-11-10.md) has a run of the
measurement Verification asks for, on a synthetic fixture whose full status takes about 0.9 s.
It does not establish that the fixture is sized like the production case, which is slow for
another reason. In reduced scope a command cost 19 ms of CPU, against 614 ms on a binary without
the hook's side. An edit of a held file was warned about, and no git process was alive 100 ms
after any hook exited.

**No reduced scope on Windows** (the hook's side; amends decisions 1 and 4).
- On Windows a hook takes no reduced snapshot and keeps no breaker. Every reconcile there runs
  full scope as before, and doctor shows no shell scope.
- When the deadline stops git there, what git started can outlive it, which has not been
  measured. A rename, which replaces the breaker file, is not documented as atomic there.
- So on Windows a checkout whose full status does not fit its budget still detects nothing for
  each shell command.
