# 005: Receipts: accepted, handled, and what the sender sees

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a proposal by `claude-home-iris` (gap review A12). Implementation is planned, not
done.

## Problem

R4 says the recipient acknowledges what it has handled, so the sender can tell delivered from acted
upon. Iris does not do that today. `docs/api.md` defines an acknowledgement as acceptance, not
action, and the MCP `ack` tool says the same. The operator's thread view shows each recipient's
state, but an agent has no way to ask what happened to the mail it sent. A sender that waits on
dependent work cannot see through Iris whether its request arrived, let alone whether it was
handled.

## Decision

**1. Acknowledgement stays "accepted".** An ack still means the recipient has the message and takes
responsibility for it. That is what settles a delivery: it stops redelivery and clears attention,
as now. Nothing about recovery, attempts or stale acks changes.

**2. A separate "handled" state.** The ack request takes an optional `"handled": true`. A handled
ack also accepts, so one call can do both. That call follows today's rules: the delivery is assigned to
the calling Execution, and the attempt matches, or `conflict`.

Marking an already-accepted delivery handled has looser rules, because it is usually said later,
from another Execution of the same agent: after a compaction, a resume, or a lost session that
A10 replaced. That Execution never fetched the delivery and, once it is acked, never will. So any
live Execution of the recipient agent may mark it handled, with no attempt. The mailbox, which is
the Agent, is the identity that matters. Another agent's Execution gets `not_found`. The mark is
monotone and idempotent: a repeat succeeds and changes nothing, a handled state is never taken
back, and so a stale caller can undo nothing. A handled mark on a delivery not yet accepted, from
an Execution it is not assigned to, gets `conflict`, as an ack would. The `Ack` row gains a
nullable `handled_ms`. Tests cover a mark from a second Execution of the same agent, a refusal
for another agent, and the repeat.

"Handled" is a claim by the recipient model, not a verified fact. Iris records that the recipient
said so, and when. It cannot check that the work was done, or done well. The MCP tool description
and `docs/api.md` say this in those words.

**3. The sender can ask.** `GET /v1/messages/{id}/status` and an MCP tool `receipts` return, for
each recipient of one message:

```json
{"message_id":"ms_...","recipients":[{"recipient":"codex-host-api","repository_id":"rp_...",
  "state":"handled","at":"..."}]}
```

`state` is `pending` (not fetched, or freed again), `inflight` (fetched by a live Execution, not
acked), `acked`, or `handled`. These are the names `DeliveryState` already uses, plus `handled`.
`at` is `Ack.acked_ms` for `acked` and `handled_ms` for `handled`. It is null for `pending` and
`inflight`: `Delivery` stores no fetch or free time, and this decision adds none. No subject, no
body and no recipient Execution is returned.

An undelivered (`pending` or `inflight`) recipient also carries `"unreachable":true` when it has
no active Execution: its Executions are lost, ended, or absent. This is the delivery-problems
query's definition of unreachable. The field is omitted for reachable recipients and for every
`acked` or `handled` delivery. It is computed when asked, not stored as a delivery state, so the
existing states and `at` keep their meanings and older clients can ignore the addition.

Unreachable describes now, not whether someone will answer. An active Execution cannot promise
a reply either. Mail stays queued for a returning agent; a new active Execution removes the
indication and can fetch the same delivery. Reading a receipt neither acknowledges nor discards
mail. The MCP tool description and API documentation state these limits.

After a hub restart, a recovering Execution's deliveries stay `inflight` for up to the 120 s
recovery window. They become `pending` if it is lost (A10). A sender polling during a deploy can
see that, and it is not a fault.

Only the agent that sent the message may read its status, from any of its Executions, and the
operator, through the console's thread view, which shows `handled` as well. Anyone else gets
`not_found`, the same answer as for a message that does not exist, so the route does not reveal
which ids exist.

**4. No new visibility across repositories.** The status lists only the recipients of the sender's
own message, which the sender chose and which `send` already returned to it as deliveries. It does
not reveal any other message, and nothing about a recipient beyond its name, its repository, the
state of this one delivery and whether it is currently unreachable while undelivered. This stays
within ADR 004's visibility rule: the message is visible in
the sender's repository because its sender is there. A `repo:` address appears as the contact it resolved
to, which `send` already returned to the sender.

**5. Asked, not pushed.** A receipt does not wake the sender in this step. The sender asks when it
needs to know. A receipt event on the watch stream can follow later if real use shows a need.

## Not in this decision

D9 change 4 leaves one case open: an `inbox` response lost while its session stays alive leaves the
mail fetched and nothing wakes the session. Fixing that needs a fetch receipt, which changes the
inbox protocol. It gets its own change after this one. Until then `inflight` can persist for such
a delivery until its Execution ends.

## Consequences

- R4 is met in the sense it can be met: the sender can tell `acked` from `handled`, and both are
  the recipient's word.
- One nullable column and one read route on the hub, one argument on `ack` and one new MCP tool.
  The hub needs a migration and a deploy, and clients need the new MCP tool.
- Work is split: this ADR first, then the store and API, then MCP and the console.
