# 032: Learn repository aliases only from the Execution's checkout, and isolate ambiguous allocations

Status: accepted on 2026-11-20 by the coordinator (holzera), after the owner asked for the cause
of duplicate repositories to be fixed. Amends [ADR 012](012-repository-identity-repair.md) and
D11 in the architecture.

## Problem

On 2026-11-20 the public hub held 194 repositories: 51 named iris, 70 proprietary and 49 snapper.
A private copy of its database showed two causes.

- **Contamination.** A hook reports the checkout of the event it saw. That can be another
  repository: a submodule, or another project the agent ran a command in. A repeated registration
  of an active Execution taught the Execution's repository every alias of that checkout. The
  canonical snapper repository held the remotes of proprietary, snapper-frontend, snapper-mcp,
  snapper-delegate, polygon, snapper-ios, snapper-android and iris, and about 96 aliases of
  submodule paths. The canonical iris repository held snapper's aliases.
- **Snowball.** Root discovery reads at most 128 loose commits (D11), so almost every request has
  an unknown root, and resolution rests on aliases. Once two repositories held the same shared
  git directory or remote alias, a request from a new worktree path found them ambiguous. ADR 012
  then allocated one isolated repository for that checkout, which is correct, but the new
  repository learned the same shared aliases. It became one more candidate, and the next new
  path was ambiguous again. Each `iris-wt-*` worktree got a repository of its own.
- **The guard allocated.** The commit guard used the registration resolver, so a commit in a
  checkout the hub could not place allocated a repository. 124 of the duplicates had no
  Execution. Such a repository holds no claims, so the guard's answer read as a check that found
  no conflicts.

## Decision

1. **Learning comes only from the Execution's own worktree.** A repeated registration of an
   active Execution checks the request's root against its repository, and learns aliases, only
   when the request's canonical worktree is the Execution's. A request from another checkout keeps
   the Execution and its lease and teaches its repository nothing. Canonical worktrees fold
   alternate paths through the worktree id as before (D6); a path that cannot be folded counts as
   another checkout, which loses learning, never identity.
2. **An ambiguous unknown-root allocation is isolated.** When registration allocates because the
   candidates were ambiguous, whether through the local, shared git directory or remote aliases,
   and the request's root is unknown, the new repository is marked isolated. An isolated
   repository learns no alias that another repository holds, at allocation or at any later
   registration. It still learns aliases no other repository holds, such as its own path, and its
   checkout continuity (ADR 012) is recorded as before. Learning a known root ends the isolation,
   and the repository then publishes its aliases as any repository does. A known-root allocation
   among ambiguous candidates is not isolated: it publishes as before, so duplicate roots stay
   visibly ambiguous.
3. **The isolation mark is an alias.** It is stored as `isolated:<repository id>` in the existing
   alias storage, as ADR 012 stores checkout continuity, so no migration is needed. It names its
   own repository. A merge that carries it into another repository leaves a mapping that marks
   nothing, and no lookup asks for it.
4. **The guard never allocates.** It resolves with the same rules as registration and reuses a
   recorded checkout outcome. When it cannot place the checkout it refuses with `ambiguous` or
   `not_found`, and the CLI warns and allows the commit, as it does for any failed check. It no
   longer establishes checkout continuity for an ambiguous checkout; a registration there does. A
   known-root guard still clears continuity that points elsewhere.
5. **The owner can remove exact aliases.** `iris admin unalias --repository <id> [--dry-run]`,
   with the aliases as arguments or one a line on standard input after a lone `-`, removes those
   mappings from that repository only. Another repository's mapping of the same alias stays. Only
   the owner token may call it, in one transaction. A dry run reports the same answer and changes
   nothing, not even expired leases.

## Repair

Existing contamination and duplicates stay until the owner repairs them, as ADR 012 requires.
After a backup and a rehearsal on a copy, the owner merges each duplicate into its canonical
repository with `iris admin merge` and removes foreign aliases from the canonicals with
`iris admin unalias`. Merges preserve every alias spelling, including checkout outcomes, so the
source-to-destination map is reviewed before it is applied.

## Limits

- A genuinely ambiguous checkout still gets one isolated repository per new path until the owner
  repairs the split. Isolation stops the growth; it does not join anything.
- An isolated repository does not appear under the shared aliases, so a root-less request elsewhere
  never finds it through them. That is the purpose: it was allocated because those aliases could
  not tell.
- Rule 1 does not change which Execution a hook's paths are attributed to. A command whose
  relative paths belong to another checkout was still reconciled against the Execution's
  repository; ADR 033 decision 5 ends that.
- A non-git directory, or one re-initialised as a new repository each time, has a new root each
  time, and different roots are different identities (ADR 012).
