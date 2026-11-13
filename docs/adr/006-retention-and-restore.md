# 006: Retention and restore

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a proposal by `claude-home-iris` (gap review D3). Implementation is planned, not
done. Decision 7 is amended below, on 2026-11-13: a restored copy runs on a new stream.

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

## Amendment of 2026-11-13: a restored copy runs on a new stream

Decided by `claude-holzera-iris` on 2026-11-13, from a proposal by `claude-home-iris` that
`gpt-6.1-sol` reviewed twice. It amends decision 7, and closes for most restores the gap that ADR
017's amendment "A restore keeps the stream" describes.

**The gap.** A backup copies the whole database, its stream id included, so a hub restored from it
kept the stream and took the event sequence back. A client that remembers a cursor above the
restored high-water mark sees that the hub went back (decision 7, ADR 017). A client that first
checks after the restored hub has passed that cursor cannot tell. Attention committed after the
restore, at or below the remembered cursor, was then announced by no hook or monitor and woke no
one until newer attention arrived, though the counts included it and the inbox returned it.

**The restore marker.**
- `iris admin backup` writes a restore marker into its copy: one row in the table `RestoreMarker`,
  which migration 025 creates. The copy is marked before its integrity check, and nothing is
  published if marking fails. A backup never adds the row to the database it copies. A backup of a
  database from before migration 025 creates the table in the copy, and the migration keeps it.
- The copy is one file, with no `-wal` or `-shm` beside it, so the marker goes wherever the file's
  bytes are copied.
- The first start of a hub with migration 025 on a database that carries the marker gives it a new
  stream id and deletes the marker. Both happen in the start's own transaction, before the epoch
  is raised, so a failed start keeps the marker and the old stream. Later starts on the same file
  keep the new stream, and every copy of one backup gets a stream of its own at its first start.
  `iris serve` says on standard error when it starts on a marked database.
- `iris admin mark-restored <database>` sets the marker in a database that has none, so that a copy
  made some other way also runs on a new stream at its next start. It works offline. It holds the
  hub's lock on the path a hub would resolve for the whole operation, so it refuses a database
  that a hub has open under that path or under a link to it. A hard link or another mount of the
  file is a path of its own, which the lock does not see. It starts nothing, so no epoch,
  recovery, expiry or migration runs. It refuses a file that is not a hub database, and marking
  again leaves one row. Before it reports success, it checkpoints the marker into the database
  file itself, so a copy of that file alone carries it. Another process holding a read on the
  file can stop the checkpoint; it then fails and says so.

**What clients do.** A new stream is new attention to every client (ADR 017, decision 4), so what
is pending is said once more.
- The hooks tell pending mail and freed paths, with their counts.
- The wake runner treats the new stream's identity as new, with its own allowance: one wake, and one
  retry after a known failure (ADR 023). A wake queued before the restore and not yet consumed still
  blocks until its own prompt consumes it, because that prompt may still run.
- A monitor that names the old stream gets a reset, reads the snapshot and announces the pending
  mail.

Sessions, fences and recovery follow decision 7 and the restart rules, unchanged. An Execution that
was active at backup time is recovering, not live, until it reattaches with its generation or its
recovery ends; ended and lost Executions stay as they were. A fence or claim token from before the
restore is refused, as after any restart, and a session that reattaches gets fresh fences.

**What this cannot do.**
- A database without the marker keeps its stream, and the gap, unless `iris admin mark-restored`
  marks it before its first start. Only `iris admin backup` from migration 025 on writes the marker
  by itself. A copy of the live file, a volume or host snapshot, `sqlite3 .backup` and a backup by
  a binary from before migration 025 carry one only if their source did, so they are normally
  unmarked. That includes the backup taken before deploying the first release with migration 025,
  because the running binary makes it. `iris admin restore-check` warns about an unmarked backup,
  and names `iris admin mark-restored`, instead of checking for a new stream.
- A binary from before migration 025 ignores the table. A marked copy that such a binary starts
  first keeps its stream and its marker, and so does every backup or copy made of it meanwhile. The
  first later start by a binary with the migration rotates the stream then.
- After a reset a monitor announces only mail. A path freed before the monitor reconnected is not
  announced, after this reset as after any other. The hooks still tell it on the session's next
  turn.
- The hub replays a touch answer it cached for a retried tool call, attention included, for up to a
  day (decision 2). A replayed answer from before the restore names the old stream, which a hook
  counts as new, and the next fresh answer names the new stream, which it counts as new again. So a
  replay can cause up to two extra announcements of attention already told. Nothing is lost.
- The first start by a binary with migration 025 consumes a marked file's marker. Moving the backup
  file into place consumes the only marked copy, so copy it instead and keep the original.
