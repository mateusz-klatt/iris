# 006: Retention and restore

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a proposal by `claude-home-iris` (gap review D3). Implementation is planned, not
done.

## Problem

The hub runs continuously and keeps everything. Sessions and automatic claims append events, so
the `Event` table grows with every edit. Released and expired claims, ended Executions and
idempotency records (`Operation`) are never deleted. `PruneEvents` exists but only tests call it.
Nothing says how long anything is kept, what happens when the disk fills, or what a restore from
backup may and may not bring back.

## Decision

**1. Events.** An hourly sweep in the hub deletes every event older than 7 days, and every event
that is at least an hour old and that every saved monitor cursor has already passed. A monitor
whose cursor falls behind the pruned prefix already gets a reset frame and a snapshot on its next
connection, so pruning loses no wake. The console's watch keeps no cursor and resets the same way.

**2. Idempotency records.** `Operation` gains `created_ms`, set to the migration time for
existing rows. The sweep deletes records older than 24 hours. A client derives or keeps a retry
key for at most ten minutes, so a retry inside its window always finds its record, and one after
24 hours is a new operation, as it would be anyway.

**3. Claims.** A released or expired claim is deleted once it has been over for 30 days. A live
claim is never touched.

**4. Executions.** `Message.sender_execution_id`, `Delivery.execution_id` and
`Ack.execution_id` reference `Execution`, and so does history the console shows. An ended or lost
Execution that any message, delivery or acknowledgement still references is **kept**: deleting it
would break message history, and detaching it would lose who sent or handled what. Any other
Execution that ended more than 30 days ago is deleted, with its cursors, waiters and claims.

**5. Messages are not pruned.** This ADR deletes no message, delivery or acknowledgement. The
database therefore grows with mail. A message body is at most 16 KiB. At 500 messages a day of 2
KiB on average, which is well above today's traffic, that is about 1 MB a day, or 0.4 GB a year,
plus the Executions those messages keep. `iris doctor` and `GET /v1/status` report the database
size, so growth is visible. Archiving old mail needs its own decision, about what the console and
`receipts` may then still show, and gets an ADR when the size calls for it.

**6. Disk pressure.** A write that fails because the disk is full fails like any storage error: the
transaction rolls back, the hook fails open (D3) and records why, and doctor shows it. The hub does
not delete anything early to make room.

**7. Restore.** Restoring a backup is a hub start on an older database, and every start already
increments the boot epoch. So every Execution in the restored database is recovering, not live:
it must reattach with its remembered generation inside the recovery window, or it is lost. A
session whose host has moved on since the backup, to a newer generation, cannot present the old
one and registers afresh. Restored authority is never used as it was. A monitor whose saved cursor
is past the restored high-water mark gets a reset, as the events code already does for a cursor
ahead of the hub.

The restore procedure in `docs/deploy.md` is tested for real against a copy of production's backup,
which the archive in the volume holds, and never against production itself. The test opens the
copy in a scratch hub, checks `GET /v1/status`, registers a session with an old generation and sees
it refused or rebuilt, and confirms that a monitor resets.

## Consequences

- Event history is bounded at 7 days. Idempotency records last a day, and finished claims and
  unreferenced Executions 30 days.
- One migration (`Operation.created_ms`), one sweep in the hub, the size in status and doctor,
  and a tested restore procedure in the deploy docs.
- Mail and its history grow without bound until a later ADR decides on archiving.
