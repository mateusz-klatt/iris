# 012: Repository identity after a history rewrite

Status: accepted.

## Problem

A rewritten history gives one remote several roots. Registration currently takes
one arbitrary local alias and rejects the remote if any historical root contradicts
the request. Repeated registrations can consequently create more repositories for
the same current root. An already active session skips repository discovery. Root
discovery also stops after 128 loose commits and cannot read packed commits.
Claims and peers are scoped to the resulting repository id, so a split prevents
coordination across hosts. Forgetting records is not a repair: it destroys mail
and history.

## Decision

**Registration.** A known root qualifies an alias. Prefer a unique exact-root
candidate over candidates whose root is unknown; historical roots are separate
identities. Count distinct repository ids, never alias rows. More than one
candidate at the same strength is ambiguous and is never chosen by row order.
Host-local aliases establish continuity before a global remote is considered.
An unknown root cannot distinguish histories: use only unambiguous continuity,
and never guess between contradictory or duplicate repositories. A root alone
never joins forks. Repeated registration checks identity without moving an
existing Execution, its mailbox or its claims to a different repository.
An active session whose known root changes is refused with an operator repair
instruction. If its root was unknown, registration learns the root and aliases
without replacing the Execution.

Keep root discovery bounded and process-free in hooks: first-parent traversal
through at most 128 loose commits. Packed, longer, missing or unreadable history
continues to report an unknown root. This change fixes how the hub treats that
absence; it does not add a Git object parser or invoke Git during identity lookup.

**Repair.** The owner explicitly names a source and a destination repository with
`iris admin merge --from <id> --into <id> [--dry-run]`. Only the owner token may
invoke the route. Both planning and applying use one transaction. The plan reports
the rows that move, alias coalescing and any agent slot changes. Applying preserves
agent and Execution ids, names, generations, message bodies, deliveries,
acknowledgements, events, cursors and idempotency records. Repository references
move to the destination; duplicate alias mappings become one mapping, preserving
every alias spelling. The source repository is removed after its references move.

Agent names are already globally unique and remain unchanged. If two durable
mailboxes occupy the same host/client/worktree/slot after the merge, allocate the
source mailbox the first free slot in a deterministic order; do not combine
mailboxes. The destination's repository name and identity are retained. Refuse
incompatible repository policies and conflicting nonempty contacts rather than
silently changing them. Refuse overlapping live claims between unrelated
Executions under the destination's path policy. The operator must resolve those
claims and contacts before retrying. A failed operation leaves all records as
they were. Dry run makes no changes, including lease expiry or retention.

**Operation.** Back up and restore-check first, rehearse the dry run and merge on
a copy served on loopback with a scratch token, then review the current dry run
before applying to the intended hub. Repair is never part of registration or a
migration.

## Limits

Matching remote and root cannot prove that two repositories should be one. D11
still refuses ambiguous automatic joins; the owner chooses which historical or
duplicate identities to repair. A merge cannot recover previously forgotten mail,
undo work performed during a split, or prevent an unmediated edit. A dry run is a
transactional snapshot, not a reservation: the real run checks again. Retaining
separate mailboxes means a resumed session may need the operator to identify the
mailbox it should use. Hooks and the commit guard remain cooperative.
