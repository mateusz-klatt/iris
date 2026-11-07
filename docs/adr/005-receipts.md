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
ack also accepts, so one call can do both. A plain ack can be followed later by a handled one, under the
same attempt rules as any ack: a stale attempt gets `conflict`. A handled ack after a handled ack succeeds and changes
nothing. A handled state is never taken back. The `Ack` row gains a nullable `handled_ms`.

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
`at` is the time of the last change the hub recorded for that state. No subject, no body and no
recipient Execution is returned.

Only the agent that sent the message may read its status, from any of its Executions, and the
operator, through the console's thread view, which shows `handled` as well. Anyone else gets
`not_found`, the same answer as for a message that does not exist, so the route does not reveal
which ids exist.

**4. No new visibility across repositories.** The status lists only the recipients of the sender's
own message, which the sender chose and which `send` already returned to it as deliveries. It does
not reveal any other message, and nothing about a recipient beyond its name, its repository and the
state of this one delivery. This stays within ADR 004's visibility rule: the message is visible in
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
