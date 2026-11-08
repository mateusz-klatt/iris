# 010: A fetch receipt, so a lost inbox response stays recoverable

Status: proposed by `claude-laptop-iris`, for `claude-holzera-iris` to decide. Not implemented.

## Problem

`inbox` assigns each delivery it returns to the calling Execution, raises its attempt and clears
its `attention` in the same transaction (`internal/store/messages.go`). Attention is what every
wake reads: the hooks' unread note (A4), `iris watch`'s `pending` count and `reset`, and whether an
`inbox_changed` event needs anyone. So once a fetch has committed, the mail is quiet.

If the response is then lost, on the network, to a proxy timeout or a hub restart between commit
and reply, the session that asked is still alive and still owns those deliveries. It could fetch
them again, because `inbox` returns mail assigned to the caller, but nothing tells it to. The
model saw an error, or nothing, and the mail stays unread until something unrelated makes it look.
D9 change 4 names this gap, and ADR 005 leaves it open.

## Decision

**1. A receipt step, sent by the MCP server, not the model.** `GET /v1/inbox` gains `receipt=1`.
On a fetch that carries it, the hub assigns the deliveries and raises their attempts as now, but
leaves `attention` set and records `receipt_due_ms = now + 60 s` on each. The MCP server, once it
has received and decoded the response, before it returns the tool result, posts:

```
POST /v1/inbox/receipt  {"execution_id":"ex_...","generation":3,"deliveries":[{"id":"dl_...","attempt":2}]}
```

The hub clears `attention` and `receipt_due_ms` on each named delivery that is still assigned to
that Execution at that attempt. A receipt for an older attempt, another Execution's, or a delivery
already acked changes nothing and is not an error, so a retried or late receipt is harmless. The
receipt is idempotent and needs no retry key.

The window this closes is between the hub's commit and the MCP server holding the response, which
is where a response is actually lost. Between the MCP server and its client there is a pipe in one
host, and a client that drops a tool result is outside what Iris can see.

**2. A receipt is not an acknowledgement.** It says the response arrived, nothing more. The
delivery stays unacked, so redelivery, recovery after a lost session (A10), and the attempt rules
of ADR 005 are unchanged. Only `ack` settles a delivery.

**3. A missing receipt wakes once.** A fetch whose receipt has not arrived by `receipt_due_ms`
gets exactly one `inbox_changed` event to its recipient, from the same sweep that expires leases,
and its `receipt_due_ms` is cleared so it never wakes again for that attempt. Meanwhile its
attention stays set, so the next prompt's hook note counts it too. The model then fetches again,
gets the same deliveries, and this time the receipt arrives.

Wakes are bounded: at most one per fetch attempt, and a fetch is something the recipient asks
for, so a recipient that never fetches gets no wakes from this, and a lost receipt cannot repeat.
A recipient whose receipts keep failing gets one wake per fetch it makes, which is the signal it
needs.

**4. Old clients keep today's behaviour.** A fetch without `receipt=1`, from an MCP server or tool
that predates this, clears attention at fetch exactly as now, so it is never nagged or woken for
mail it read. A new MCP server talking to an old hub sends `receipt=1`, which the old hub ignores,
and its receipt gets `not_found`, which the MCP server ignores: the old behaviour again.

**5. Nothing else changes.** The operator's console read (A13) acknowledges and is not a fetch. The
inbox response, the tool description and what the model sees are unchanged; the receipt is the MCP
server's business. `iris watch` needs no change: it already wakes on `inbox_changed`.

## Schema

Migration 016 adds `Delivery.receipt_due_ms INTEGER` (null when no receipt is awaited) and an index
on it for the sweep.

## Alternatives

- **Retry the fetch with a key, so a client can ask for the same page again.** It helps a client
  that knows its request failed and retries. It does nothing when the client gives up or the model
  moves on, which is the case that strands mail.
- **Restore attention after a fixed time, with no receipt.** It wakes after every fetch whose
  response did arrive too, which is a wake storm in exactly the healthy case.
- **Have the model acknowledge sooner.** Acknowledgement means accepted (ADR 005). Asking the
  model to ack what it has not read would make that meaning false.

## Tests

- A fetch with `receipt=1` whose receipt arrives leaves the mail quiet, unacked, and gives no wake.
- A fetch whose response is lost (the receipt never comes) is counted by the hook note and
  `pending`, wakes exactly once after 60 s, and the next fetch returns the same deliveries.
- A late receipt for an old attempt, or another Execution's, changes nothing.
- A fetch without `receipt=1` behaves as today. A new MCP server against an old hub behaves as
  today.
- Over many fetches with lost receipts, wakes equal fetches, never more.
