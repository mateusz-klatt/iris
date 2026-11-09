# Reviewed account import (ADR 014, step 4)

The importer transfers account records into Iris. It does not run a source
backend, import sessions, infer repository access, activate named console login,
or perform the later UI/cutover steps. Use synthetic SQLite files for rehearsal.
A real apply is a separate owner operation after reviewing its manifest and
backup. The examples below describe that operation; they are not evidence of a
production import.

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
storage types, independently of column affinity. The adapter opens the supplied
file with SQLite `mode=ro` and reads one
transactional snapshot. It neither creates a missing file nor writes the source.

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
   owner token separately. In a future account-mode deployment, also protect its
   independent console key separately.
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
   retain request-body traces. Account sign-in verification and UI activation
   belong to the later cutover, not this importer release.

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
Normal restart preserves named console sessions in account mode; deployed
legacy browser cookies remain in memory. An account-mode restore must instead rotate
the independent console key and clear restored console sessions before access
is enabled. Account-mode key setup/recovery and console cutover remain gated in
this release; do not reopen legacy anonymous links alongside named access as a
recovery shortcut. Owner named-operator creation/reset uses the staged
`iris admin console create`/`reset` stdin-password commands when that later gate
is enabled; passwords never belong in argv or reports.

The owner token and hub OS administrator remain trusted. The importer cannot
contain a compromised source file, browser, hub or trusted host process. It
validates a bounded account transfer, not the completeness of an export or its
historical permissions. No real account data or production import is required
to verify this implementation.
