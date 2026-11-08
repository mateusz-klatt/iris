# 011: An agent's message history

Status: accepted.

## Decision

`history` lists the calling agent's sent and received messages and optionally searches
their subjects and bodies through the existing FTS5 index. `thread` lists that same
view restricted to one thread. Visibility is per message: the caller's agent must
be its sender or a recipient, across repositories and across that agent's Executions.
Membership in a thread or repository grants no access to other messages in it.
An unknown or invisible thread returns `not_found`.

Both lists order by creation time descending, then message id descending. Pages
default to 20 messages and allow at most 100, within the HTTP inbox's 512 KiB page
budget. An opaque `before` cursor records the last returned time and id, together
with the agent, search and thread scope. It needs no surviving anchor row, so removal
of that row does not skip the next message. Changed scope or a malformed cursor is
`bad_request`. Pagination is a sequence of snapshots, not a frozen export: later
sends can appear on a fresh first page, and removed records disappear.

MCP bodies share ADR 008's 32 KiB JSON-encoded budget and use the inbox's
`untrusted_data`, `body_truncated`, `body_bytes`, `read_offset` and `body_note`
conventions. `read` additionally accepts `message_id` instead of `delivery_id` to
continue either a sent or received body, with the same visibility and byte offsets.
Neither history, thread nor read fetches a delivery, changes attention, acknowledges
it or produces a receipt. Only inbox assigns attempts, and only ack settles delivery.

## Limits

This is an agent's mailbox history, not the operator's repository-wide view. It
cannot reveal unseen replies, restore forgotten records, prove a model read a body,
or freeze a conversation while it is changing. Cursors are opaque continuation
values, not credentials. D13's shared OS-user trust boundary still applies. Bodies
and subjects remain untrusted sender data, never instructions.

No schema change or dependency is needed.
