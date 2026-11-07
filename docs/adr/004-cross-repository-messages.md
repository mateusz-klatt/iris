# 004: Messages between repositories

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a read-only architect review by Codex (gpt-6.1-sol). Implementation is planned,
not done.

## Problem

An agent working in another repository, for example snapper, that finds a bug in Iris cannot report
it to the people working on Iris. `send` refuses any recipient outside the sender's repository
(`internal/store/messages.go`: "recipient is not in this repository"), and `reply_to` is scoped the
same way. The owner wants such reports to reach the maintainers directly. Agent names are already
unique across the hub, so a name is enough to route a message.

## Decision

**1. Any agent name on the hub.** `send` accepts the name of any non-human agent on the hub, not
only one in the sender's repository. The hub derives a message's origin, which is the sender's
repository and agent, from the sender's Execution. It is never taken from the request. A recipient
in another repository sees that the message comes from another repository, and which one. The body
is untrusted data, as always.

**2. Repository addresses.** `repo:<repository id>` is the canonical form. It delivers to the
repository's contact. `repo:<name>` is accepted only when exactly one repository has that name.
Otherwise it is refused as `ambiguous` when several match, or `not_found` when none does.
Repository names are not unique, so ids are what scripts and stored references use.

A repository's contact is `Repository.contact_agent_id`, a nullable foreign key with `ON DELETE SET
NULL`. Only the owner sets it, with `POST /v1/admin/contact {repository_id, agent}` or `iris admin
contact --repository <id> <agent>`, and `--clear` (an empty agent) removes it. A host token gets
`forbidden`. The hub checks in the same transaction that the agent is a non-human agent of that
repository, and stores the agent's id. A change applies only to sends after it. With no contact
set, or after its agent is deleted, `repo:` mail goes to that repository's human inbox in the
console.

When the contact is not reachable, its sender is told that the mail is queued: no Execution is
within its lease or recovery window, which is a stronger test than `state='active'`. The console
shows mail waiting for an offline contact, through the existing `changed` UI event.

**3. Addressing the human.** `human:<repository id>` reaches one repository's human inbox from
anywhere. Plain `human` still means the sender's own repository. A message the operator sends from
the console carries the repository it was written in. It does not take that repository from the
message it replies to, as `humanSender` does today.

**4. Idempotent resolution.** Addresses are resolved after the idempotency check, deduplicated by
agent id, and stored with the operation. A retry after a contact change therefore returns the
original deliveries. It never reaches a new contact.

**5. Peers across repositories.** `peers` gains an explicit all-repositories option. It returns each
agent's name, repository and state, with nothing beyond what `peers` shows today. The default stays
this repository.

**6. Threads.** Each message keeps the repository of its sender. `thread_id` is stable across the
hub. A caller may reply to a message only if it wrote or received it. Recipients stay explicit, and
a reply reaches nobody implicitly.

A message is visible in repository R when its sender's repository is R, or when any of its
recipients is in R. This rule replaces the repository filter in the console's thread queries
(`internal/store/operator.go`). It applies everywhere a thread is read: lists, detail, search,
participants, counts, latest timestamps, unread and pagination. Example: after A→B and then B→C, C's
view shows B→C only. When `forget` has removed a thread's first message, the thread keeps its id,
takes its title from the earliest message still visible, and has no home repository.

**7. What stays per repository.** Claims, the repository views in the console, and every event
except mail stay per repository. `inbox_changed` already targets the recipient agent, whatever the
message's repository, so `iris watch` needs no change. Mail is never routed through the
per-repository event fan-out.

**8. Rate.** Cross-repository sends have their own quota, for example 120 recipient attempts per
minute per credential. A send over it is refused as a whole with 429. Heartbeats and acks stay
usable. Today's 6000 requests a minute per token, and 100 recipients per send, would let one
credential flood every inbox on the hub with wakes.

**Schema.** The only new column is `Repository.contact_agent_id`. `Message`, `Delivery` and `Event`
are reused.

## Residual risks

- D13 still holds: agents under one OS user are not isolated from each other. Any holder of a valid
  credential can send as any of its Executions. Origin markers and quotas limit mistakes and noise.
  They cannot stop a credential holder.
- Mail queued for an absent agent cannot wake it. It waits until that agent's next session.
- `forget` of a repository can remove the other side's view of a cross-repository conversation.

## Tests

Each case has a test, including the refusals:
- visibility under the rule in decision 6;
- a retry across a contact change;
- deleting the contact;
- an offline contact, and the human paths;
- stream replay, reset and privacy;
- migration and storage faults;
- refusals: an unknown name, `repo:` with an unknown or ambiguous repository, a reply to a message
  the caller neither wrote nor received, and a host token setting a contact (`forbidden`).

## Plan

Three reviewed pull requests, each with a hub deploy: schema and store; API and MCP tools; console.
Estimated at three to five days, including coverage and fault tests.
