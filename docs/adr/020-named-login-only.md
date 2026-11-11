# 020: Named login is the only console login

Status: accepted on 2026-11-11 by the fleet's vote 5, under the owner's direction.
The vote at 10:46 UTC was 1B 2A 3A from all four voters: holzera, macOS, Windows
and WSL. PR 1 implements the command and link cleanup. PR 2 implements import
removal, migration 024 and fresh-hub setup through `create`. PR 3 remains decided
but not yet implemented.

## Problem

Named username/password authentication already serves the browser console. ADR
019 removed login-link creation and exchange and the old cookie authority. An
unused sign-in translation directed people to a printed link, and a coverage
exclusion named a deleted frontend file. `iris ui` only printed the configured
hub URL and asked the operating system to open it. It read no token, called no
hub and authenticated no one.

Before PR 2 the account importer was the only producer of scrypt hashes in the
database. PR 2 removes it; bounded scrypt verification remains. Successful
browser login upgrades those hashes to Argon2id; activation accepts them without upgrading. Both accounts on
the public hub have Argon2id hashes after the owner's password changes on
2026-11-11. Older backups can still hold scrypt hashes.

On 2026-11-11 the owner directed:

> mozemy dropnac support do importu i starego scrypt

English: We can drop support for the import and the old scrypt.

> mozemy tez uczynic logowanie username/password jako natywne i domyslne

English: We can also make username/password login native and the default.

> i dropnac support do iris ui z lini komend calkowicie

English: And drop support for `iris ui` from the command line completely.

Iris has no release yet. There is no compatibility contract to preserve.

## Decision

**1. Remove the command and link leftovers in PR 1.** Remove `iris ui` completely,
including its dispatcher, usage, platform openers and tests. The command becomes
unknown and changes nothing. A person opens the hub's URL in a browser:
`http://127.0.0.1:7878/` by default for a local hub. There is no replacement command.
Remove the unused sign-in text in English and all 44 catalogs and the coverage
exclusion of the deleted `src/legacy.ts`.

Vote 2A keeps the explicit 404 for `/ui/login` and `/v1/ui/links` and its tests.
An old bookmarked link must not reach the console page. No schema or served
response changes in PR 1. Shared fake-executable fixtures remain for other tests.

**2. Remove the import in PR 2; implemented.** This follows the
cutover record in pull request #265. The account and grant import commands,
their two owner routes, source adapter and store import code are removed. The
shared read transaction used by activation readiness remains beside activation,
under a name that does not say import, with cancellation cleanup and no expiry
writes.
Migration 024 drops `ConsoleAssignmentImport` and `ConsoleImport` in the store's
single migration transaction and sets schema version 24. Migrations 017 and 020
remain unchanged; a fresh database replays them, and 017 also creates the account
tables that stay. Existing accounts, grants, sessions and audit rows remain,
including historical import actions. Shared tests no longer assume the ledgers
exist in the latest schema; historical migration fixtures retain their assertions.

The owner creates accounts with new passwords through the native CLI. `create`
makes an enabled operator. For an intended member or disabled account, save its
intended role, state and grants in the access editor and read them back before
handing out its password. Import history and assignments are not recreated.

**3. A fresh hub opens through `create` in PR 2; implemented.**
Vote 1B applies only when the database has no console account at all and the hub
has no console key. In that case `iris admin console create <username>` creates
the operator and prepares the independent console key in the same owner action.
It records `owner_provision` and `activation_prepared` in the owner audit and
answers `account_created: true`, `key_prepared: true`, `restart_required: true`.
The preparation record does not claim separate password verification or key
persistence. Output states exactly what happened: operator made, key prepared,
restart needed. One restart opens named login.

The no-account/no-key check and account creation are one transaction. Key
preparation happens only after commit, as with activation. If it fails, the
operator remains without a key; `activate` followed by a restart can then open
the console. On every other hub `create` only creates an enabled operator and
answers `account_created: true`, `key_prepared: false`. Neither an account
without a key nor a key without an account triggers key preparation through
`create`.

`restart_required` is independent of `key_prepared`: it is true when the console
is closed now and, after creation, a valid key and an enabled operator are
available, so a restart would open named login. On a hub with a valid existing
key but no accounts, `create` therefore returns `key_prepared: false`,
`restart_required: true`; after restart the new operator can sign in. On a hub
with an account but no key, `create` returns both flags as false; `activate` is
still needed. Creating another account on an already open console also returns
both flags as false. The CLI separately reports operator creation, key
preparation and whether a restart is required.

`activate` remains for a restored database without its key and for key loss.
It verifies an enabled operator's password, clears all console sessions,
prepares the key and requires a restart. On a fresh hub, verifying a password
the owner just set proves nothing beyond `create` and clears no sessions. On
restore it proves that the restored password is known and clears sessions that
the backup brought back. The console key and startup gate remain. A closed
console keeps the existing unavailable page and retry action, without public
setup instructions. A binary without the console bundle still answers 503 with
build instructions.

**4. Remove scrypt in PR 3; decided, not yet implemented.** This follows PR 2,
which removes the only producer of scrypt hashes. Accept Argon2id only. Remove
scrypt parsing, derivation and successful-login upgrade code. Do not convert
stored hashes at startup, migration, backup or restore: passwords are unknown.

A stored scrypt hash then follows the unreadable-hash login path. After rate
admission, verify against the Argon2id dummy hash and return the same generic
failure as for an unknown user, disabled account or wrong password. Preserve
status, body, cookies, retry headers, quota and safe logs; return no 500 or
account-existence or hash-format hint. Keep shared KDF admission and owner-only
activation without cookies or changes to stdin framing. Activation refuses an
unreadable stored hash with fixed owner advice: this version does not read the
stored password format; recover the operator with
`iris admin console reset <username>`, then activate. The browser gets no such
format-specific advice.

Activation keeps verification-only bounds: valid UTF-8 and the maximum byte
length. A short Argon2id password retained through an earlier upgrade still
activates a restored hub. Create, reset and password change retain the minimum
for new passwords. Reset remains operator recovery: it replaces the hash without
verifying the old one, makes the account an enabled operator, deletes its grants
and revokes its sessions.

Through PR 1 and PR 2, a wrong password for a scrypt account still costs scrypt
work, while unknown, disabled and unreadable accounts take the Argon2id dummy
path. Answers, quotas and logs match, but timing differs. This affects imported
accounts that have not signed in since import and older restores; the public
hub has none. PR 3 removes that difference. Vote 3A keeps these three pull
requests separate and in this order.

**5. Recover a restored database in isolation after PR 3; decided, not yet
implemented.** A backup from before 2026-11-11 10:06 UTC, including older public
hub backups, or a database with imported accounts can hold scrypt hashes.
Restore with the console key withheld and the public proxy closed to the console,
as the [restore procedure](../console-activation.md) requires. Keep that isolation
until access has been restored and checked.

Every password set during recovery is fresh and unpredictable and known only to
the person performing recovery. Keep it private until recovery finishes. After
restart, loopback accepts sign-ins; another user of the hub's host could reach
it with a password they already know.

1. Choose the recovery operator. If its hash is scrypt, reset it. Then activate
   and restart with fresh key material and cleared sessions.
2. Before resetting any other scrypt account, including a member or disabled
   account, record its role, disabled state and grants outside the console. The
   access editor includes disabled members' grants. Reset makes it an enabled
   operator, deletes its grants and revokes its sessions. In the access editor,
   click Reload, select the account again, apply its recorded role, state and
   grants, save, reload and check that they read back as recorded. A form left
   open before the reset holds stale access, and saving it returns a conflict.
3. Hand out passwords or reopen access to anyone else only after every account's
   access reads back as recorded. If a save fails or a check differs, keep the
   isolation and the record taken before reset unchanged. Repeat only these
   steps: click Reload, apply that record, save and read back. Never record the
   account again after reset: its temporary state is operator with no grants.

No new reset command that preserves role and grants is added. Such a command
would need a separate decision. PR 3 adds this recovery to the restore runbook.

**6. No compatibility path.** No flag, alias, replacement command or grace period
for `iris ui`, import or scrypt. Keep the public hub's existing Argon2id accounts.
Each deployment takes the usual backup first. Rollback across migration 024
restores its matching pre-deploy backup. PR 1 changes no schema, so image-only
rollback remains available under [the deployment procedure](../deploy.md).
Research records remain as measured, including their old links and imports.
Section 5's architecture invariants do not change. Transactions retain their
existing guarantees, including the all-or-nothing migration transaction.

This decision supersedes:

- [ADR 002](002-public-hub.md): bearer-created browser login links.
- [ADR 014](014-operator-accounts.md): the import ledger parts of account identity;
  imported role and grant mappings; scrypt acceptance and upgrade; the ordinary
  origin `iris ui` command; and the transactional importer. Native accounts,
  roles, grants, sessions, Argon2id and browser defenses remain.
- [ADR 019](019-console-activation.md): import independent of the console gate;
  scrypt acceptance during activation; the URL opener; and import steps of the
  cutover. PR 2 also replaces separate activation on a fresh hub under the exact
  no-account/no-key condition above. Activation remains the restore and key
  recovery path, including verification-only password bounds.

The superseded text remains as history with a note at each decision. Only the
PR 3 parts above describe accepted future work.

## What this cannot do

- After PR 3, sign in with a stored scrypt hash or recover its password from the
  hash. Recovery sets a new password; role, state and grants return by hand.
- End sessions issued before PR 3 merely because an account's hash is scrypt.
  Session checks do not read hash formats. Activation clears all sessions and
  restore isolation withholds the key. The public hub's password changes on
  2026-11-11 revoked its old sessions.
- Bring accounts, history or grants from another system. The owner
  creates each account and reviews access before handing out its password.
- Stop a pre-024 binary opening a version-24 database. The store has no maximum
  version check: it starts and fails only when it touches a dropped table. The
  backup rollback procedure is advice to the owner, not enforcement.
- Undo migration 024 without restoring the matching pre-deploy backup.
- Open a browser for anyone. The person opens the URL.
- Keep a binary from before #250 from minting links, or a binary from before
  PR 3 from reading scrypt, against a database it is given.
- Change the research records.

## Verification

PR 1 proves `iris ui` is unknown and has no effects, keeps retired-path 404
assertions, and checks equal translation keys and placeholders across English
and all 44 catalogs. Run the full local Go, race, coverage, static and fixture
checks and the web test, coverage, lint and typecheck scripts.

PR 2 proves that import commands and requests to import routes are refused
without effects. It proves that migration 024 preserves all non-ledger records
and that the database reopens. It also proves that a failure of the second DROP
or the commit rolls back both ledger drops and the schema version, after which a
retry succeeds. Prove fresh-hub `create`, both account/key edge cases and
recovery from key preparation failure. Rehearse an older binary on version 24
and the supported backup rollback on a scratch hub. Doctor uploads still work
while closed.

PR 3 proves generic login failures with KDF admission free and busy, shared
admission, and no session issuance after a racing reset or access change. Test
restored operators, members and disabled accounts through the isolated recovery,
including a stale editor conflict, failed save, reload and read-back from the
record taken before reset. Verify session revocation, short Argon2id activation,
Argon2id-only hash validation and access review before a new member's password
is handed out. Every implementing pull request runs the full local checks and
native Windows and macOS checks, with code and documentation review.
