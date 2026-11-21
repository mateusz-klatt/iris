# ADR 034: Sessions outside git share one messaging context per host

Status: accepted, 2026-11-21. Amends D3 rule 5, D10 and D11, and ADR 032 and 033 where they speak
of a directory outside git. The hub's part, decisions 1 to 5, the hub's half of 6 and 8, and 9,
ships first; the clients' part of 2, 6 and 7, and the console's part of 8, follow.

## Context

A session started in a directory that is not a git checkout can still register. The hook does
nothing there (D3, rule 5), but once its binding grace has passed, `iris mcp` or `iris watch`
registers a fallback Execution for a session it can identify, so the session can send and receive
mail. Codex id mode and the wake helpers of Antigravity and Kimi only look up, and register
nothing. The fallback registration names the working directory and no checkout:
`binding.Resolver.register` leaves the worktree, git directory, remote and root out when
`gitinfo.Find` fails for any reason, and `Find` itself answers "not a git worktree" for a `.git` it
cannot read. The hub then takes the working directory as the worktree. Unless an active Execution
or an existing alias decides otherwise, it finds nothing to resolve, allocates a repository named
after the folder, and learns a `local:` alias for its path and a `checkout:` continuity alias.

So most folders a session ran in became a project. On the public hub they were `tmp`, `memories` (a
Claude memory directory), `copilot-live` and `perf-bench` (a folder of benchmark scripts beside a
Snapper worktree), among others. The owner wants the project list to hold repositories only, and
had these merged or forgotten by hand. Not registering such sessions would cost them mail, which
is why they register at all.

Two further problems follow from the same records:

- **A folder project can take claims.** The MCP `claim` tool places a path in the innermost
  checkout below the session's directory (ADR 033), so a session in a plain folder that holds a
  real repository can claim that repository's files through `claim_files`. A hook event of that
  session that resolves a checkout through its working directory or workspaces can claim there
  too.
- **A folder project is an identity.** It learns `local:` and `checkout:` aliases. A later
  `git init` in that folder, or a merge, can join it to a real repository's history.

## Decision

1. **One context per host.** Every Execution attached to an outside context belongs to its host's
   one record: a `Repository` row marked by the alias `outside:<host>`, where the host is the
   whole rest of the alias. It is named `outside git on <host>`, has no root commit and has no
   other alias. Finding it or making it is part of the registration's transaction. Two rows with
   the same marker are refused as `conflict` until the owner repairs them; registration never
   chooses by row order. No schema changes. Like ADR 032's isolation marker, the marker lives in
   the alias table, and `iris admin unalias` refuses to remove it.

2. **Which registrations go there.**
   - **New clients.** `gitinfo.Find` answers "not a git worktree" only when no `.git` lies above
     the directory. A `.git` it finds but cannot stat, read or parse is a failure of its own, and a
     checkout with an unknown root or no remote is still a checkout. A client registers on `POST
     /v1/sessions/outside` only after "not a git worktree"; any other failure fails the
     registration. The route refuses a request that names a worktree, git directory, worktree id,
     remote or root, and runs the same store operation as the session route, with its bookkeeping.
   - **Older clients** send such a session on `POST /v1/sessions` with no worktree, git directory,
     worktree id, remote or root. The hub classifies that shape as outside git before it takes the
     working directory for the worktree. No shipped producer sends it after a successful checkout
     discovery: the hook and shell registrations always name the worktree and git directory. An
     MCP or watch fallback whose discovery failed for another reason does send it.
   - **An active Execution keeps its record.** A registration that finds the active Execution of
     the same host, client and session keeps it where it is, whichever kind of record and route.
     Generation, recovery and lost-lease checks apply as before. Mail and claims never move
     silently.
   - **Order.** The request is classified first. Only a git request refreshing an Execution in a
     repository checks the root and learns aliases from its own worktree (ADR 032). An outside
     registration never calls repository resolution, allocation or alias adoption, so it learns no
     `local:`, `checkout:`, `gitdir:`, `remote:` or `root:` alias and writes no checkout
     continuity. Git resolution ignores any record that carries the marker. A git request may
     still canonicalise its worktree path by device and inode (D6); that table names no repository.

3. **Identity stays per directory.** The context's Agents and Executions keep the real working
   directory as their worktree, so each directory has its mailbox, its slots and its name,
   `<client>-<host>-<directory name>`, with the existing slot and collision suffixes. A session that
   ran in a directory before gets its mailbox back when it returns, unless another active session
   holds it or the mailbox is still in an unrepaired folder project.

4. **The context never lends git identity.** With no git alias, no git resolution finds it, and a
   `git init` in one of its directories does not change it. Once the directory is a checkout, a new
   session there registers in that checkout's repository; one still active stays in the context
   until it ends. `iris admin merge` refuses a pair that includes a context.

5. **Messaging only.** An Execution attached to an outside context never creates, renews or
   recovers a claim, and never waits for one. The hub refuses `touch`, `touch_files`, `claim`,
   `claim_files` and every shell reconciliation, full or scoped, by Execution or with its session
   registration, as `outside_git`, before any cached replay, checkout resolution, allocation,
   learning, conflict or wait. A shell reconciliation that registers its session refuses as a
   whole. Release and end stay open. Recovery after a restart copies an Execution's claims to new
   fences; a context's Executions have none to copy. The commit guard is unchanged: it checks real
   repositories and takes no claim.

   The refusal is the hub's, for the Execution's record. It cannot stop the session from editing
   files, in a checkout or anywhere else, and the guard cannot tell who commits.

6. **Bindings say so.** Registration, bind and heartbeat answers of an Execution attached to an
   outside context carry `outside_git: true`. The client keeps it in its binding with the worktree
   and capabilities, and clears it when an answer omits it, as an older hub's does. The MCP `claim`
   tool refuses such a binding before it reads the filesystem, and says to start the session in a
   checkout. A hook whose session is attached to a context sends no claim for an edit either,
   even after a `git init` there, and allows it.

7. **Hooks register outside git.** D3 rule 5 becomes: outside a git worktree, the hook registers
   on the outside route at session start, at a sub-agent's start, at a prompt where its client has
   that hook, and at stop where it registers today. It ends the session with the existing end
   route. An edit or a shell command outside git takes no claim and makes no request. It keeps the
   hook's deadline (D3) and generation memory, and the existing order of the Grok and Antigravity
   helpers and of Kimi's arming. Registration makes a session eligible for wake. Wake still arms
   only on all of its existing evidence: a positive root binding, the exact Execution and
   generation, a hook having reached the hub, the user's preference, native idle evidence, fresh
   attention and no outstanding submission (ADR 017, 023).

   `iris doctor`'s canary runs in an empty directory so that it reaches no hub, and outside git it
   still makes no request.

   A hub without the route answers `not_found` with "unknown API route". On that answer, and only
   on it, a hook allows the event and registers nothing, and `iris mcp` or `iris watch` falls back
   to the legacy registration and says once that the hub is older. An authentication failure, a
   timeout or any other error is not taken for an older hub.

8. **Console.** Overview names each record's kind, `repository` or `outside`, and an outside
   record's host. The project list and its counts leave contexts out, active agents or not.
   Contexts stay in overview, compose origins and the grant editor. Their agents stay in the fleet
   and the recipient picker, labelled `outside git · <host>`, and their mail stays in all activity
   and the views a reader is allowed. Hiding a context from the project list is presentation, not
   access control. A member still needs a grant to the context, as to a repository; an operator
   sees it.

9. **Repair of folder projects.** `iris admin merge --outside --from <id> --into <context>` moves a
   folder project into its host's context, on a route of its own, so an older hub refuses it
   instead of running an ordinary merge. The owner names the pair; nothing finds folder projects
   on its own, and startup changes nothing. It applies only when all of these hold, checked again
   on apply in one transaction:
   - the destination is a context and the source is not;
   - the source has no root, and its aliases are only `local:<host>:…`, a `checkout:` alias whose
     host is the context's, whose git directory is empty and whose remote is `literal:`, and its
     own isolation marker; any other alias refuses the repair;
   - every Agent and Execution of the source that is not the console's person has the context's
     host;
   - no Execution of the source is active or recovering, and none holds a live or recovering
     claim, in any repository;
   - the existing policy, contact and member-grant checks of a merge pass.

   It keeps Agents, names, Executions, mail, history, acknowledgements and receipts, as a merge
   does. It deletes the source's Waiters and shell snapshots instead of moving them, and drops its
   aliases. The dry run lists exactly the aliases, Waiters and snapshots it would drop.

## Consequences

- The project list holds git repositories only, once the hub is upgraded and any folder projects
  are repaired. An active folder project stays an ordinary repository, with its claims, until its
  sessions end and the owner repairs it.
- Every session outside git on a host is a peer of every other one there, and sees their directory
  paths in `peers`. That is discovery, not isolation: a host token can already list the whole hub.
- A session outside git takes no claim. A session registered outside git whose hook later sees a
  checkout through its working directory or workspaces could claim there under ADR 033; it no
  longer can.
- A hook outside git now reaches the hub on lifecycle events, so such a session has a hook-backed
  Execution, which its `iris mcp` adopts instead of registering a fallback.
- `human` mail from a context reaches the context's shared person mailbox. A contact the owner sets
  serves `repo:` addressing, as for a repository.

## Mixed versions

| Client | Hub | Result |
|---|---|---|
| Before this change | Before | Folder projects, as before. |
| Before | This change | A legacy outside registration lands in the host's context. Its hook does nothing outside git, as before. An active Execution in a folder project stays there. |
| This change | Before | The outside route is unknown. `iris mcp` and `iris watch` fall back to the legacy registration and say once that the hub is older; the hub makes folder projects, as before. A hook allows and registers nothing. |
| This change | This change | Contexts, hook registration outside git, refused claims. |

Upgrade the hub first. Rolling the hub back on the database this change wrote does not keep these
guarantees: an older hub treats a context as a repository, accepts its claims, and learns git
aliases for it from a repeated registration in its own worktree. Roll back to the backup taken
before the upgrade, or review the contexts first.

## Limits

- **Iris cannot tell a repository on GitHub from any other git checkout.** "Outside git" means
  discovery found no `.git` above the directory, nothing more.
- **An older client counts every discovery failure as outside git.** A checkout whose discovery
  failed on such a client lands in the context, messaging only, until discovery succeeds and that
  Execution ends or is replaced.
- **Host identity is the host id Iris was given.** Two machines given one id share one context; a
  renamed host gets another.
- **Repair is per pair and owner-selected.** It cannot recover mail of a forgotten record, or take
  apart a folder project already merged into a repository.
- **Edits are not coordinated.** Iris refuses claims to a session outside git; it does not stop
  that session's edits.

## Verification

The change is covered by behaviour tests for: classification on both routes; an active Execution
kept across kinds with its generation, recovery and lost-lease checks; no alias, continuity, Claim,
Waiter or fence written by any outside lifecycle path, recovery included; each refused claim route,
before replay and waits; refused shell reconciliation with registration; mail delivery, recovery
and stale acknowledgements in a context; the binding flag set and cleared; the hook's routes and
its older-hub answer; and the repair's every precondition, its dry run and its atomic apply. Every
new statement propagates execution, query and row failures under the fault driver.

## Changes

- ADR 032 says a directory outside git has no root, not a new one each time.
- ADR 033's rule that a path in no checkout takes no claim extends to every path of a session
  outside git.
- D3, D10 and D11 in `docs/architecture.md` say what is above.
