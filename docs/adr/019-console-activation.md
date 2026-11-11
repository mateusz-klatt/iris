# 019: Activate the named console explicitly, then restart

Status: accepted on 2026-11-10 by claude-holzera-iris, on the candidate evidence
at the end of this record.

## Problem

ADR 014 requires an enabled operator whose password has been verified before
named browser authentication is activated. Setup and recovery must work before
that gate opens. A missing key must close the console while agents keep working.
The existing key helper creates a missing file, so ordinary startup cannot use
it as evidence of an explicit activation.

The fresh-hub verification step is superseded by [ADR 020](020-named-login-only.md); PR 2 implements fresh-hub setup through `create`.

## Decision

1. The browser console is named-account-only. Remove link creation, link exchange,
   in-memory grants, the `iris_session` cookie authority and the anonymous
   operator fallback. Owner and host bearers never mint browser sessions.
   Authenticated machine diagnostic uploads and agent routes keep their existing
   authority. The owner may create, recover or import accounts while the console
   is closed.
   Import independent of the gate is superseded by [ADR 020](020-named-login-only.md); PR 2 implements this removal.
2. Startup only loads the independent protected console key. By default it is
   `<database>.console.key`; `--console-key-file` selects another hub-local file.
   Missing, unreadable, malformed or reused owner-token material leaves the
   browser namespace closed. Opening named authentication also requires an
   enabled operator in the database. The protected key is an explicit durable
   opt-in marker, under the trusted hub OS owner's control.
3. The create, reset and activate CLI commands treat exactly one final LF or
   CRLF as stdin line framing and remove it before password validation. Every
   other byte is preserved, including spaces, embedded line endings, a bare CR
   and an additional final LF. This explicitly amends the former CLI contract
   that counted the final newline as password content; it adds no general
   trimming or normalization. Read at most 1,027 bytes, remove that one optional
   framing sequence, then enforce valid UTF-8 and the 1,024-byte password bound.
   Only create/reset also enforce the new-password minimum. The HTTP server
   continues to verify submitted password bytes exactly, without this CLI framing.
   `iris admin console activate <username>` calls an owner-only endpoint. It
   verifies the bounded supported password profile outside the store mutex,
   then rechecks the verified account snapshot,
   authentication version and enabled operator role inside one transaction.
   That transaction clears all console sessions and records `activation_verified`.
   It does not claim that later key persistence succeeded.
4. Only after successful verification does activation create or load the protected
   independent key. Publication must use a private temporary file in the same
   directory, apply protection before writing secret bytes, then sync/close and
   link atomically at the final path without replacement. An existing final file
   is loaded and validated; malformed/incomplete material is never overwritten
   or silently repaired. Owner review/quarantine is required to choose fresh
   material. An I/O failure leaves the running console closed. Success reports `restart_required`;
   activation does not issue a cookie or change the running process's gate.
   Calling activation on an already open console is refused without clearing its
   sessions. Normal restarts preserve the key, database and existing absolute
   session expiries.
5. Imported credentials retain their original minimum password length. Activation
   accepts the supported bounded scrypt profile and the normal Argon2id profile,
   with the existing UTF-8/1,024-byte verification bounds. It does not apply the
   fifteen-code-point policy for newly created passwords to imported passwords.
   Scrypt acceptance is superseded by [ADR 020](020-named-login-only.md);
   PR 3 implements Argon2id-only activation.
6. `iris ui` resolves and validates the ordinary console origin, prints it and
   opens it best-effort. It does not read a credential file or request a login
   link. The frontend distinguishes a closed console from an ordinary signed-out
   named session. The URL opener has been removed under
   [ADR 020](020-named-login-only.md).
7. Before a real first cutover, the owner explicitly confirms readiness in their
   own terminal and remains present through activation and sign-in checks.
   Sequence: owner confirmation, fresh backup, deployment with browser access
   immediately closed and agent coordination continuing, approved import apply
   followed by the same dry-run, owner activation, a second hub restart, then
   owner sign-in and verification. The approved import and its dry-run may
   instead come before the deployment, on the version that is serving, because
   no migration separates the two versions and imported accounts open nothing
   there; the public hub's first cutover does it that way. The second restart
   needs no secret, so whoever deployed performs it. Agents may prepare and
   rehearse; they must not activate real accounts on the owner's behalf or
   receive secrets in chat.
   The import steps are superseded by [ADR 020](020-named-login-only.md); PR 2 implements this removal.

## Trust and recovery limits

This is an explicit startup gate, not a boundary against the hub OS owner.
Creating or replacing key material and directly editing the database remain
trusted administrative actions. There is no hot key reload. Filesystem stalls,
KDF capacity, scheduling and store contention can still delay an operation.

Legacy link grants and `iris_session` cookies were held only in process maps.
There is no SQLite legacy-session table to restore or clear. Deploying the new
binary and restarting discards those maps; activation clears named
`ConsoleSession` rows. Restarting an old binary also starts with empty maps, but
that binary can mint fresh legacy grants. A backup can restore named account
passwords, permissions and sessions, not legacy process-map cookie authority.

Restoring an older database can restore older passwords and permissions. Restore
into isolation with the old console key withheld, verify an operator, activate
with fresh key material and cleared sessions, then review restored grants through
a private literal-loopback named console before reopening public access.
The existing agent restore check alone is not an account-authentication rehearsal.

An arbitrary older binary may reopen its legacy console: it does not reject a
newer schema. A supported rollback therefore retains a named-authentication
backend, or closes public browser, login and UI API routes at the trusted proxy
before the older backend starts. Agent routes and authenticated machine diagnostic
uploads remain available. New code cannot constrain an independently launched
old binary. The deployment runbook must identify the actual proxy configuration
and rehearse that boundary.

That rule applies once a cutover has succeeded. A first cutover that fails goes
back to the backend that was serving, without the proxy boundary: that backend
served with login links when the cutover began, so the hub returns to the state
it was in and no further. Imported accounts stay in the database, unused, and so
does a key that activation created; a later deployment of the activation version
then starts with the console open. A named session made before going back is not
accepted by the older backend, and is accepted again by that later deployment
until it expires
([observed on a scratch hub](../research/console-activation-candidate-2026-11-10.md#going-back-and-another-attempt)).

For the current public host, only the owner can apply/reload the live Apache
filter and drain established browser sockets: `sudo` requires the owner's
password. Agents may prepare/review/rehearse but cannot claim that live action.
If the owner is unavailable, retain a supported named-authentication image or
stop/move the hub off the proxied port. The latter leaves the entire hub,
including agents, unavailable until the owner can establish the boundary. It
does not authorize an exposed old backend.

The temporary filter defaults to denial outside `/v1/` except local ACME,
denies the UI namespace except raw literal doctor PUT, and strips request
cookies so the old backend cannot use a legacy cookie on that exception.
The literal exception must use unescaped `THE_REQUEST`, because `REQUEST_URI`
is already decoded. Before reopening named authentication, remove both the
rewrite block and temporary Cookie removal; then login and require a subsequent
authenticated session GET to return 200. Leaving Cookie removal permits a login
response to set a cookie but strips it on that subsequent request, causing 401.

## Required evidence

[ADR 020](020-named-login-only.md) replaces the scrypt acceptance requirement
below with rejection and retained short Argon2id verification. The measured
evidence remains historical.

Use temporary synthetic accounts and the real rebuilt binary to prove closed
startup, owner bootstrap, imported short scrypt verification, activation followed
by restart, persistent absolute expiry, and refusal of old links and cookies.
Use two real browser contexts for live revocation and stale prepared requests;
also check account replacement across tabs sharing cookies. Rehearse restoration
with a fresh key and cleared sessions and the supported rollback proxy boundary.
Run the complete local Go, fixture and web checks at the final rebased commit.
No real account import or deployment is implied by those rehearsals.

## Provisional evidence, 2026-11-09

The [rehearsal record](../research/console-activation-rehearsal-2026-11-09.md)
records identities, checks and limits. The real rebuilt binary and real browser
contexts passed 44/44 checks, including actual owner/import CLI use and KDF work,
short imported scrypt verification/upgrade, restart expiry, live downgrade,
stale prepared and cached requests, an observed shared-cookie/old-CSRF 401 read,
and backup/restore with the old key withheld, fresh activation/session clearing
and private grant review. A fresh restored `ro` session received 403 when
replaying a cached send that had succeeded under the backed-up `rw` scope.

That report is `/tmp/iris-real-console-bdGopY/report.json`, SHA-256
`7e09a427327606b87ae934bc70b438f63fee50cd007a95bd18a1c20eaaba3cef`.
Its binary SHA-256 is
`f407697c4bf93c99759c34b60d75c1dfc3ef947372166ac2fbe1125e2b793211`,
from modified source `0fe50af` with uncommitted UI changes. It is provisional
evidence, not the final clean release candidate; atomic key publication and all
final local gates still require validation.

An actual Apache 2.4.69 rehearsal with an older backend build (`bc1eae9`, a commit
the repository no longer keeps) passed 50/50 checks
after replacing the decoded doctor exception with the raw-request condition.
Evidence is `/tmp/iris-apache-rollback-w578u1xj/`. It used rendered reference
vhosts, literal loopback, temporary synthetic credentials/state, self-signed TLS
and HTTP/1.1. UI/new Upgrade denial, Cookie stripping, authenticated doctor and
agent registration, ACME and HTTP redirect, and TLS 1.2/1.3 were observed.
Production configuration/certificate issuance or renewal, HTTP/2 and draining
established sockets across a reload were not verified.

The expanded `/tmp/iris-apache-rollback-paths-5qq8yavj/metadata.json` replayed
the original checks and passed 73/73. Outbound raw request-line witnesses prove
that no client normalization removed the tested double slashes or dot segments.
All twenty noncanonical UI GET/doctor PUT cases returned 503 with a valid legacy
cookie alone and with machine+cookie, including trailing slash/dot doctor paths.
Canonical doctor PUT with a query returned 200 for the valid machine bearer and
401 for that cookie alone. Both rehearsals used the same exact documented block.
No production Apache changes or real account imports were performed.

## Candidate evidence, 2026-11-10

Two records cover the image CI built for the pull request. The
[candidate run](../research/console-activation-candidate-2026-11-10.md) ran the
container beside the public hub: a closed start, activation, restart and a
browser sign-in on an earlier head, a start on a copy of a production backup,
and, on the final head's image, going back to the version before and another
attempt. The [access and import run](../research/console-access-and-import-2026-11-10.md)
ran the binary from the final head's image, `653916e` merged with `main` at
`1ffed57`. It covered an import on the version before, the switch, owner
bootstrap and recovery while closed, activation and restarts with an unchanged
absolute expiry, refusal of a legacy link cookie, the access checks with two
browser contexts and two tabs, keyboard use in a narrow and a wide window, and
going back with another attempt: 75 of 75 checks passed.

Not repeated on the candidate: the restore rehearsal with a fresh key, which ran
above on the provisional binary built from a modified `0fe50af`, and the Apache
boundary, which was rehearsed above with that older backend build.
