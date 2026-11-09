# Reviewed account import and access review (ADR 014 and ADR 019)

The importer transfers account records into Iris. It does not run a source
backend, import sessions, infer repository access, activate named console login,
or perform the cutover. Use synthetic SQLite files for rehearsal.
A real apply is a separate owner operation after reviewing its manifest and
backup. The examples below describe that operation; they are not evidence of a
production import. The operator access editor is described
[below](#access-review-and-isolated-rehearsal); explicit activation, restore and
rollback use the separate [console runbook](console-activation.md).

Before commands for a first named-console cutover, the owner explicitly confirms
readiness in their own terminal and remains present. Take the backup before
deployment: the new binary immediately closes browser access until the owner
activates a verified operator and performs the second restart; agents continue
working. After an approved import apply, repeat the original dry-run before
owner activation. Agents may prepare or rehearse, but do not activate real
accounts on the owner's behalf or receive passwords in chat.

## Source and manifest

Keep the source SQLite file and manifests private and outside the repository.
Do not inspect or publish password records, salts, internal fingerprints, request
bodies or database dumps. Protect transport with HTTPS, or literal-loopback HTTP
on a trusted machine. Configure `IRIS_TOKEN_FILE` to an owner-token file; never
put its contents in a command. Host tokens and console cookies cannot import.

The source must be an ordinary table with exactly these columns:
`id`, `username`, `password_hash`, `role`, `disabled`, `display_name`,
`preferred_ui_locale`, `preferred_correspondence_locale`, `created_ts`,
`last_login_ts`. IDs are positive SQLite integers; booleans are integer 0 or 1.
Last login may be SQL NULL. Other values must use the required SQLite text/integer
storage types, independently of column affinity.

Supply a **closed, stable, checkpointed export in DELETE journal mode**, not a
live application database. Before SQLite opens anything, the adapter checks the
main-file journal-mode header and refuses WAL mode or any `-wal`, `-shm` or
`-journal` sidecar, including empty or stale files. It resolves symlinks first and
rejects nonregular inputs. A checkpointed WAL-mode file is still refused even
when its sidecars are absent: checkpointing alone does not change journal mode.

Accepted exports are opened with `mode=ro&immutable=1` and SQL query-only mode.
The immutable flag disables SQLite locking and shared-memory update paths;
`mode=ro` and query-only alone would not prevent WAL machinery from creating or
updating `-shm`. See SQLite's [read-only WAL rules](https://sqlite.org/wal.html#read_only_databases)
and [immutable connection contract](https://sqlite.org/uri.html#uriimmutable).
The owner must keep the export unchanged throughout both dry-run and apply.
Iris cannot enforce that prerequisite or provide a live-source snapshot while
immutable locking/change detection is disabled. On a fixed export, one read
transaction covers schema and rows. The importer never creates a missing input,
opens its source backend, or repairs, checkpoints or removes source sidecars.

### Preparing a refused source

Have the source owner produce a consistent backup through its approved export
workflow, such as SQLite's [online backup mechanism](https://sqlite.org/backup.html).
This preparation is separate from the importer and must not target the preserved
original dump or a production file. A raw copy of only a live main database can
omit committed WAL rows. If working from a frozen filesystem snapshot, keep the
main database and its matching WAL together when making a disposable working
copy; never mix files from different moments.

On that **disposable backup copy only**, with all other connections closed, the
owner may use the SQLite CLI to checkpoint and convert journal mode:

```sh
sqlite3 /private/disposable-export.db 'PRAGMA wal_checkpoint(TRUNCATE); PRAGMA journal_mode=DELETE;'
```

Require the final journal-mode result to be `delete`, then close SQLite before
passing this file to Iris. If conversion is busy or sidecars remain, resolve the
export with its owner and repeat preparation; never delete or rename sidecars to
force acceptance. SQLite may update the disposable copy while preparing it. Iris
performs no such preparation and never drops uncheckpointed rows silently.
Review expected safe row IDs/counts through dry-run against the prepared export
before applying it. The supplied table, source identifier and manifest mappings
still require the same explicit review.

### Manifest fields and mappings

Obtain the table name and timestamp contract from the owner of the export.
Declare them explicitly; do not guess from values. Supported timestamps are
integer `unix_seconds` or `unix_milliseconds` in `UTC`; other formats require a
reviewed adapter extension. Dates must be between Unix epoch (1970-01-01) and
the end of year 9999, with last login at or after creation when present. Source
dates are history, never authorization or session expiry inputs.

A version 1 account manifest contains:

```json
{
  "version": 1,
  "source_id": "reviewed-export-a",
  "table": "accounts",
  "timestamp_encoding": "unix_seconds",
  "timestamp_zone": "UTC",
  "renames": {"2": "ReviewedDistinctName"},
  "locales": {"unsupported-source-locale": ""}
}
```

Choose one stable source identifier and retain it across retries. `renames` maps
source row IDs to explicitly reviewed usernames; omit it or use an empty object
when no names need changing. `locales` maps exact source locale values to a
supported Iris locale or explicitly to unset (`""`). Unused/invalid mappings are
refused. The same normalization used for Iris account creation detects Unicode
and case collisions. The original display username remains intact unless its
row has an explicit rename. Locale preferences remain data, not instructions.

The source password contract is ADR 014's exact bounded scrypt profile; the tool
validates its encoding and cost without deriving a password. It preserves an
accepted record byte-for-byte. `admin` becomes `operator`; `member` stays member;
any other role becomes disabled `none`, reported as quarantined. Disabled remains
disabled. Every imported member, including a disabled one, starts with zero
repository grants. The account import contains no project assignments.

Manifests are bounded to 32 KiB, and account/assignment batches to 32 rows.
The HTTP request is also bounded by the hub's body limit (64 KiB by default);
reduce the batch size for unusually large escaped text. A manifest must describe
the entire selected source table. For a larger export, obtain separately reviewed
bounded source extracts with stable row IDs and the same source identifier.
Never modify a captured private dump just to make an import succeed.

## Backup, review and apply

1. Before a real apply, take a fresh backup on the hub's machine with
   `iris admin backup /data/iris.db /private/new-before-import.db`. The destination
   must not exist. This includes the WAL consistently; copying a live `.db`
   alone does not. Protect the backup as credential-bearing data and secure the
   owner token and independent console key separately.
2. Rehearse restore on a copy with
   `iris admin restore-check /private/backup-copy.db /private/empty-scratch-dir`.
   Keep the pre-import backup unchanged. Never use the live volume for rehearsal.
3. Run the default dry-run from the trusted importing machine:

   ```sh
   iris admin console import --source /private/source.db --manifest /private/accounts.json
   ```

   Review row IDs, planned roles, quarantine and mapping indicators, counts and
   any refusals. Neither this command nor its dedicated hub transaction writes
   accounts, ledger, sessions, login timestamps or expired coordination records.
   A dry-run cannot reserve a username or guarantee a later apply: concurrent
   changes can introduce a collision.
4. After owner review, repeat with explicit apply:

   ```sh
   iris admin console import --source /private/source.db --manifest /private/accounts.json --apply
   ```

   Apply revalidates every row and collision inside one `BEGIN IMMEDIATE`
   transaction. Accounts, ledger and owner action records commit together.
   Any invalid row or persistence failure rolls back the entire batch. The CLI
   never opens the hub file for writing and never bypasses the serving hub.
5. Repeat the original dry-run after apply: the exact source/manifest should
   report skips. Retain the reviewed manifest and safe report privately. Never
   retain request-body traces. Import does not activate sign-in. Continue with
   [explicit password verification, activation and restart](console-activation.md#owner-setup-and-activation)
   only when that separate owner operation is approved and the owner is present
   to perform activation, the second restart and sign-in checks in their own terminal.

Exact re-runs use an internal versioned SHA-256 fingerprint independent of the
console key. A later Iris password/profile/role change is preserved, including
disabled status; a re-run cannot restore imported state. Changed source input or
row mappings, duplicate source IDs, a different source using an existing login,
or a target collision refuses instead of upserting. No fingerprint appears in a
report. A failed or lost response does not establish whether apply committed:
repeat dry-run with the unchanged source and manifest before retrying.

## Separate reviewed viewer assignments

Account import never transfers project permissions. After reviewing explicit
source-project to existing immutable Iris Repository ID mappings, use a separate
version 1 assignment manifest:

```json
{
  "version": 1,
  "source_id": "reviewed-export-a",
  "rows": [
    {"id": "1", "account_row_id": "2", "project_id": "project-a", "role": "viewer"}
  ],
  "projects": {"project-a": "rp_existing_iris_repository"}
}
```

The account row must already be in that source's account ledger. Map each source
project once to one existing repository, without ambiguous aliases. Only member
`viewer` assignments become `ro`. Unknown roles, unmapped/ambiguous projects,
assignments restricting a global operator, source project `operator` assignments,
and existing target grants are refused. Iris `rw` does not make source project
`operator` equivalent: the source role includes unsupported capabilities. Revise
and explicitly review an unsupported assignment manifest before apply; no role
is silently downgraded. Granting Iris `rw` later is an independent Iris access edit.

```sh
iris admin console import-grants --manifest /private/viewers.json
iris admin console import-grants --manifest /private/viewers.json --apply
```

Dry-run is again read-only. Grants and their separate fingerprint ledger commit
atomically. A new grant changes the target authentication version/edit revision
and revokes its sessions. Exact replays skip without restoring subsequently
edited grants; changed rows/mappings refuse. This transfer cannot overwrite an
existing role, profile, password or grant.

## Recovery and limits

A refused apply has no partial account/grant/ledger changes. Resolve its safe
reason through explicit owner review and repeat dry-run. Do not patch an import
ledger, silently rename an existing Iris account, or delete an account to force
a changed source through. Sanitized failures intentionally omit raw SQL and
source payloads; do not enable request-body tracing to diagnose them.

A committed import is not undone by blind deletion: the accounts may already
have authored messages or been edited. Preserve a new backup of the current
state and stop clients and the hub before choosing a full pre-import restore.
Follow [the restore procedure](deploy.md#retention-and-restore-adr-006); restoring
the complete backup also loses mail and coordination changes made afterward.
Review those losses and restored password/disabled/grant state before reopening.
Normal restarts preserve the independent key and named sessions' absolute expiry.
A restore instead uses an isolated copy with the old console key withheld,
fresh key material, verified operator activation and cleared sessions. Review
restored grants through a private literal-loopback named UI before reopening;
follow [the restore runbook](console-activation.md#restore-a-database-with-browser-access-isolated).
Owner `iris admin console create`/`reset` commands work even while browser
authentication is closed. For create, reset and activate, the CLI removes exactly
one final LF or CRLF from protected stdin as line framing. All other bytes,
including spaces, embedded line endings, a bare CR and additional final newlines,
are preserved; it applies no general trimming or normalization. The 1,024-byte
and UTF-8 checks apply after framing, and the server verifies submitted password
bytes exactly. Passwords never enter argv, chat or reports. Imported-password
verification accepts the supported bounded profile without imposing the
fifteen-code-point minimum used for new create/reset passwords.
The restored session rows are named `ConsoleSession` records. Legacy link/cookie
grants were only process maps, so a SQLite backup does not restore them. Restart
discards those maps; an older binary can still mint fresh legacy grants and
therefore needs the rollback proxy boundary.

The owner token and hub OS administrator remain trusted. The importer cannot
contain a compromised source file, browser, hub or trusted host process. It
validates a bounded account transfer, not the completeness of an export or its
historical permissions. No real account data or production import is required
to verify this implementation.

## Access review and isolated rehearsal

After [explicit activation and restart](console-activation.md#owner-setup-and-activation),
an enabled operator opens Access, selects an account
by its immutable Iris identity, and reviews its role, disabled state and each
repository's access. Members start with no grants. Choose no grant, `ro` or `rw`
for each existing repository, then save using the displayed account revision.
`ro` permits scoped reading. `rw` also permits sending/replying to agents, with
`rw` required on the sending repository and every destination repository. Neither
mode permits shared-mail acknowledgement, claim changes or administration.
Operators have global console authority without per-repository grants; `none`
is always disabled. A disabled account cannot sign in even when grants exist.

Every accepted access edit revokes all target sessions, so the editor offers Save
only once something has changed. Editing your own access
therefore requires signing in again. A stale revision returns `conflict`: refresh
the account list, compare the current saved permissions, and deliberately reapply
the intended edit. Do not keep retrying an old revision or overwrite someone
else's update. Removing or disabling the last enabled operator is refused; have
the owner provision and verify another named operator before removing that role.
An absent account row is not a reason to grant access by a similar display name.

Use temporary synthetic databases, protected scratch keys and passwords for
rehearsal, with two real browser contexts for live revocation and prepared stale
requests. Also check shared-cookie account replacement across tabs, narrow/wide
layouts, keyboard operation, restart expiry, fresh-key restoration and the proxy
rollback boundary. The [activation runbook](console-activation.md) defines this
sequence and [provisional local evidence](console-activation.md#provisional-rehearsal-evidence).
Real-binary/browser/restore checks passed 44/44 and expanded local Apache checks 73/73;
the modified build does not establish readiness of a final clean candidate or
the production proxy. No real source export or account import was used.

If login or key loading fails, keep the console closed while agent coordination
continues. Owner `reset` recovers the named operator and revokes its sessions;
it does not recover a lost owner credential or bypass password login. A binary
rollback must retain supported named authentication or first close public
browser/login/UI API routes at Apache while preserving agents and authenticated
doctor PUT. Follow [the exact rollback placement and locally rehearsed rules](console-activation.md#rollback-while-preserving-the-browser-boundary)
before starting an older backend. Restoring old links alongside named accounts
is not a supported recovery path.
