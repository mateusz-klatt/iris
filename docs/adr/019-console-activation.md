# 019: Activate the named console explicitly, then restart

Status: proposed; implementation and rehearsal are under review.

## Problem

ADR 014 requires an enabled operator whose password has been verified before
named browser authentication is activated. Setup and recovery must work before
that gate opens. A missing key must close the console while agents keep working.
The existing key helper creates a missing file, so ordinary startup cannot use
it as evidence of an explicit activation.

## Decision

1. The browser console is named-account-only. Remove link creation, link exchange,
   in-memory grants, the `iris_session` cookie authority and the anonymous
   operator fallback. Owner and host bearers never mint browser sessions.
   Authenticated machine diagnostic uploads and agent routes keep their existing
   authority. The owner may create, recover or import accounts while the console
   is closed.
2. Startup only loads the independent protected console key. By default it is
   `<database>.console.key`; `--console-key-file` selects another hub-local file.
   Missing, unreadable, malformed or reused owner-token material leaves the
   browser namespace closed. Opening named authentication also requires an
   enabled operator in the database. The protected key is an explicit durable
   opt-in marker, under the trusted hub OS owner's control.
3. `iris admin console activate <username>` reads the password exactly from stdin
   and calls an owner-only endpoint. It verifies the bounded supported password
   profile outside the store mutex, then rechecks the verified account snapshot,
   authentication version and enabled operator role inside one transaction.
   That transaction clears all console sessions and records `activation_verified`.
   It does not claim that later key persistence succeeded.
4. Only after successful verification does activation create or load the protected
   independent key. It never overwrites malformed existing material. An I/O
   failure leaves the running console closed. Success reports `restart_required`;
   activation does not issue a cookie or change the running process's gate.
   Calling activation on an already open console is refused without clearing its
   sessions. Normal restarts preserve the key, database and existing absolute
   session expiries.
5. Imported credentials retain their original minimum password length. Activation
   accepts the supported bounded scrypt profile and the normal Argon2id profile,
   with the existing UTF-8/1,024-byte verification bounds. It does not apply the
   fifteen-code-point policy for newly created passwords to imported passwords.
6. `iris ui` resolves and validates the ordinary console origin, prints it and
   opens it best-effort. It does not read a credential file or request a login
   link. The frontend distinguishes a closed console from an ordinary signed-out
   named session.

## Trust and recovery limits

This is an explicit startup gate, not a boundary against the hub OS owner.
Creating or replacing key material and directly editing the database remain
trusted administrative actions. There is no hot key reload. Filesystem stalls,
KDF capacity, scheduling and store contention can still delay an operation.

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

## Required evidence

Use temporary synthetic accounts and the real rebuilt binary to prove closed
startup, owner bootstrap, imported short scrypt verification, activation followed
by restart, persistent absolute expiry, and refusal of old links and cookies.
Use two real browser contexts for live revocation and stale prepared requests;
also check account replacement across tabs sharing cookies. Rehearse restoration
with a fresh key and cleared sessions and the supported rollback proxy boundary.
Run the complete local Go, fixture and web checks at the final rebased commit.
No real account import or deployment is implied by those rehearsals.
