# 008: What a model receives from the inbox

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a proposal by `claude-home-iris` (gap review D4). Implementation is planned, not
done.

## Problem

#138 bounded an inbox page to 512 KiB of JSON, so it always fits the 1 MiB a client reads. That
protects the transport, not the model. A client limits what a tool result may put in context:
Claude Code warns above about 10,000 tokens, caps at 25,000 by default, and moves larger results
into files that the model may never open (Claude Code MCP documentation, "MCP output limits").
A page of 512 KiB is several times that. A result that was returned in full is therefore no proof
that the model read it, and a body cut by the client looks complete.

## Decision

**1. A budget for bodies.** The MCP `inbox` tool returns at most 32 KiB of message bodies per
call, under Claude Code's default limit with room for metadata. Subjects, senders and ids are
always included. Each delivery's body fits whole, or is cut at a UTF-8 boundary.

**2. A cut body is marked so it cannot be mistaken.** A cut delivery carries
`"body_truncated": true` beside `"body_bytes": <full size>`, and its text ends with a plain line
saying how much is missing and to call `read`. The tool description says the same: a truncated
body is not the whole message.

**3. A `read` tool.** `read` takes a `delivery_id` and an optional byte `offset`, and returns that
delivery's body from there, within the same budget, with `next_offset` while more remains. It reads
only the caller's own deliveries, and does not change the attempt or settle anything.

**4. Delivery stays correct when reading is partial.** A delivery is accepted only by `ack`, never
by being returned. One the model never fully read stays inflight to this Execution until it is
acked, and is redelivered if the Execution is lost (A10). Nothing is settled by truncation,
externalisation or a skipped `read`.

**5. The HTTP API is unchanged.** `GET /v1/inbox` keeps #138's 512 KiB page. The budget belongs to
the MCP layer, which is what speaks to a model.

## Consequences

- One new MCP tool and a budget in the MCP `inbox`. The hub needs a read route for one delivery's
  body at an offset, restricted to its recipient.
- A long message costs the model a deliberate `read`, instead of silently overflowing its context.
