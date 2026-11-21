# ADR 033: A claim belongs to the repository of its file

Status: accepted, 2026-11-21. Amends D10, D11 and D12. Decision 5 replaces ADR 032's limit on
shell attribution. Decision 6, for explicit claims, is decided here and ships in a later change.

## Context

The hook places an edit's paths in one checkout: the one it finds from the event's working
directory, falling back to the client's workspace paths (D11). It makes each path relative to
that checkout's worktree. The hub stores the claim under the repository of the session's
Execution. That repository is fixed when the session registers, and later registrations from
other checkouts keep it (ADR 032). The repository that holds the file plays no part. That breaks
in four ways:

- **A submodule file has two identities.** Take `snapper`, whose `frontend` directory is the
  `snapper-frontend` submodule:
  - A session started in `snapper` that edits `frontend/src/x.ts` claims that path in
    `snapper`.
  - A session started in a `snapper-frontend` checkout that edits the same file claims
    `src/x.ts` in `snapper-frontend`.
  - The two claims never meet. On the production hub, Executions were registered both in
    `snapper` and directly in its `proprietary` submodule, so both entry points are used.
- **A moved working directory misnames claims.** When the event's working directory is inside the
  submodule, the hook makes the path `src/x.ts` and the hub claims it in the Execution's
  repository, `snapper`, under a name that belongs to no file there.
- **A file outside the event's checkout takes no claim.** The hook drops a path it cannot make
  relative to that worktree, and allows the edit without a claim.
- **A shell status from another checkout is attributed to the Execution.** The shell hook takes
  the status of the checkout it found from the event's working directory. Iris does not read
  `cd` or `git -C` in the command itself. In a submodule, that status is compared with the
  Execution's single baseline:
  - When that baseline came from the superproject, superproject paths look as if they left the
    status, which releases the session's own automatic claims on them.
  - Submodule-relative paths look changed and are claimed in the superproject under the wrong
    names.

  The comparison needs an `after` status against a baseline of another checkout; a `before`
  status only replaces the baseline. D12's statement that a command run inside a submodule is
  reconciled against the submodule's own repository was not true.

The owner's requirement, 2026-11-21: a claim on a submodule's file is a claim in the submodule's
project, "whether one works in the snapper-frontend checkout or in the submodule".

## Decision

1. **A path's checkout is the innermost one that contains it.** The hook walks up from the path's
   directory, as it finds any checkout (D11). It stops at the event's worktree, which it has
   already read, or at the first directory with a `.git` entry, whose `.git` file it follows. A
   submodule, a nested repository and another project's checkout are each their own. The path is
   made relative to that worktree. A nested `.git` that cannot be read leaves its paths with the
   event's checkout, as before. A path in no checkout takes no claim, as a session outside git
   takes none (D3, rule 5). `frontend/src/x.ts` belongs to the submodule. The gitlink `frontend`
   itself, and a prefix that contains it, belong to the superproject.
2. **One tool call is one grouped touch.** A hook whose hub names `file_claims` sends every edit as
   groups on `POST /v1/claims/touch/files`. Each group names one checkout, the event's own
   included: its canonical worktree, worktree id, shared git directory, remote and root commit,
   with its paths. The hub recognises the Execution's own worktree, through the worktree id as
   for registration (D6) and with Windows spellings folded, and keeps the Execution's repository
   for it, after the same known-root check a registration from there makes. Every other group is
   placed by decision 3. Then, in one transaction:
   - The hub de-duplicates paths by repository and case-folded key, then checks every path of
     every group.
   - With a conflict in any group, nothing is granted or renewed.
   - Without one, every missing claim is taken and the session's own automatic claims are renewed,
     within the Execution's claim limit counted once over all repositories.
   - The action is the strictest policy among the repositories that had a conflict: `deny`, then
     `ask`, then `warn`. A clean repository's policy does not decide it. `ask` becomes `deny`
     where the client cannot ask, as before.

   Grants, ignored paths and conflicts name their repository. A conflict outside the Execution's
   repository names that repository in the message, because its path is relative to a checkout the
   session may know as a submodule directory. The rename ends of a call are plain paths of their
   groups. A replay is allowed only while each grant is still held. It may be held as the same
   claim, which its fence finds in whatever repository an owner's merge moved it to. It may also be
   held as a live claim on the same path in that repository, which a recovery keeps while it
   renumbers fences. A claim of the
   same name in another repository never counts, for a plain touch's replay either. The grouped
   route keeps its own retry namespace, so a plain touch's retry hash is unchanged.
3. **The hub places a group without learning from the session.** It resolves the group's checkout
   with the registration rules (D11, ADR 012), scoped to the Execution's host:
   - A repository it resolves to is used and learns that checkout's aliases. That is the
     checkout's own description, as a session started there would give.
   - A checkout that matches no repository allocates one, as a session started there would, so a
     later session there meets the claim.
   - A checkout that is ambiguous takes no claim and allocates nothing. Its worktree is listed as
     `unclaimed`, and the message says so and how to repair it. The rest of the call goes on.
     Allocating there would add one more isolated repository per checkout path (ADR 032).

   Session attachment never decides another checkout's repository. Host tokens already act across
   repositories within one hub's trust (D1). A group is the host's word about its checkout, as a
   registration is; nothing new is authorised.
4. **The claim keeps its holder.** A claim row names the session's Execution and the file's
   repository:
   - **Holder lifecycle.** Expiry, the claim limit and the end of the Execution work over all of
     its claims, wherever they are.
   - **Release.** A release tells each repository whose claim it revoked, and wakes the waiters
     there.
   - **Waits.** A wait is recorded in the repository of the claim that refused it, and taking the
     path there clears it. The waiter table keeps one wait per Execution, key and kind, as an
     older hub's table requires, so a later wait on the same key in another repository moves the
     earlier one.
   - **Forget.** `iris admin forget` counts the live claims held on a repository's files by
     another repository's sessions, and refuses while there are any.
   - **Console.** A claim is listed under its own repository, with its holder's name only.
5. **A shell status from another checkout is not attributed.** The hook names the checkout each
   status was taken in, on the session route as before and now on the Execution route too. When
   that checkout is not the Execution's worktree, after the worktree id is folded in, the hub
   keeps its baseline as it was. It takes, renews and releases nothing for that status. The
   shared-worktree check runs first, so it still clears the baselines of a worktree another live
   Execution shares. An older hook's Execution-only status names no checkout and is reconciled as
   before. No snapshot table changes, because an older hub writes those tables by position.
   No full baseline crosses a hub start any more, as no reduced one did (ADR 018): one an older
   hub wrote records no checkout, so it may be another checkout's, and that holds after a
   rollback and forward again too. Each session's first shell command after a start sets a new
   baseline and attributes nothing. Claims
   made from another checkout by edit tools stay until release, expiry or the end of the Execution.
6. **Explicit claims will follow the same rule.** The MCP `claim` tool takes a path relative to the
   session's repository. A path below a checked-out submodule or nested checkout, found on disk
   from the Execution's worktree, is to be claimed in that checkout's repository, relative to it.
   That needs the Execution's worktree and the hub's capabilities in the MCP binding, and a grouped
   explicit claim route. Until that change ships, an explicit claim stays in the session's
   repository.
7. **Mixed versions keep working.** The hub names `file_claims` in the capabilities of every
   registration answer. A hook sends a grouped touch only to a hub that named it in the same
   call's registration. Otherwise it sends the plain touch it sends today, and sends no touch for an
   edit with no path in the event's checkout. An older hub refuses the grouped route, so a grouped
   request is never half understood.

## Consequences

- Two sessions on one submodule file conflict, wherever each was started and wherever its working
  directory is.
- A session that edits another project's file claims it in that project, whose sessions see the
  holder.
- A shell command run inside a submodule no longer releases the session's superproject claims or
  takes misnamed ones.
- The commit guard of a commit made in the submodule checks the submodule's repository, and so sees
  claims superproject sessions took there. Its same-worktree exemption compares the holder
  Execution's worktree, so a superproject session's claim on a submodule file refuses that
  session's own commit in the submodule. The refusal stays, with its instruction to release first:
  the guard cannot tell who runs `git commit`.
- The hook reads more `.git` entries:
  - It walks up from each directory of an edit, once per directory per call, and reads each other
    checkout it finds once.
  - Inside the event's worktree the walk only checks for nested `.git` entries up to that worktree.
  - It looks before registering only when the event's checkout holds none of the paths. Otherwise it
    looks only after a hub has named `file_claims`.

  The walk runs inside the hook's 1.5 s deadline and fails open; its cost has not been measured
  on a slow filesystem.

## Limits

- An older hook, or an older hub, keeps the old placement. Its claims can miss conflicts with
  claims placed by this rule. The guarantee holds between sessions whose hooks and hub both have
  it.
- A superproject's prefix claim does not reach into its submodules, and a submodule's claim does
  not reach the gitlink path in the superproject.
- A file reached through a symbolic link is placed by the path the client reports.
- A checkout the hub finds ambiguous takes no claim until the owner repairs the split (ADR 032).
- A submodule that is not checked out has no `.git` entry, so its paths belong to the superproject.
- Claims made before this change stay where they were taken until they expire or are released.
- An availability event records the freed path, not its repository. When a monitor reconnects and
  catches up, the path is compared with live claims in the session's own repository only. A path
  freed in another repository can therefore be woken for once more after it was taken again, or
  not be replayed when the session's own repository has a live claim of the same name. The live
  event, sent when the path is freed, is not affected.
- One Execution keeps one wait per path key and kind, so a wait on the same key in a second
  repository replaces the first.
