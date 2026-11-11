# Activate, recover and restore the named console

The console uses named accounts under [ADR 019](adr/019-console-activation.md)
and [ADR 020](adr/020-named-login-only.md). On a fresh hub, owner `create`
prepares sign-in in one action; a restart opens it. A restored database without
its key, or a hub whose key was lost, uses explicit owner activation.

The owner performs account setup and any required activation in their own
terminal and signs in personally. Agents may prepare and rehearse this
procedure; they must not receive passwords or activate real accounts in the
owner's place. Keep passwords and machine credentials out of chat. Before a
deployment that needs setup or recovery, agree who will restart the hub and
keep the owner present through the sign-in checks.

Rehearsals are recorded in
[the rehearsal record](research/console-activation-rehearsal-2026-11-09.md), with
their limits. A rehearsal is evidence about the binary it ran; it authorizes no
production activation.

## First cutover on the public hub

The first cutover to named login is complete. Its deployed commit, import,
activation, restart and sign-in checks are recorded in
[the public-hub cutover record](status.md). Use the procedures below for current
setup, recovery and rollback.

## Startup and credentials

`iris serve` loads an existing independent console key from
`<database>.console.key`. For `--db /data/iris.db`, the default is
`/data/iris.db.console.key`. `--console-key-file /private/console.key` selects
another hub-local path. At every start the hub sets the key file to mode 0600 on
Unix, or to the owner-only Windows DACL, as it does for the owner token: a key with
wider permissions is tightened and loaded, not refused, and Iris cannot tell
whether it was read while they were wider. Keep this key separate from the owner
bearer.

A missing, unreadable or malformed key, a key file that is not a regular file or
cannot be protected (on Unix, one owned by another user), reuse of owner-token
material, or absence of an enabled operator leaves browser authentication
closed. Startup never creates the key. Agent coordination, authenticated
`PUT /v1/ui/doctor`, and owner account create/reset/activate remain available.
Only a missing console key produces `X-Iris-Console-Mode: closed`; an untrusted
browser transport does not. The frontend reports that the console is unavailable
and asks the user to contact the owner. An owner or host bearer cannot create a
browser session. Activation clears the named `ConsoleSession` table. An older
binary can restore earlier browser authority; that is why rolling back to it
still needs the proxy boundary below.

The hub OS owner and owner bearer remain trusted. This gate does not prevent an
OS administrator from replacing key material or editing stopped database files.
There is no hot key reload.

## Owner setup and activation

Configure the CLI for the intended hub and its protected owner-token file. The
owner can create and recover accounts while browser authentication is closed.
Do not let saved client configuration select a different hub.

1. Before replacing a running image, take a fresh consistent database backup and
   keep a copy off the hub's volume. Protect the owner credential separately and
   preserve any existing console key privately. Install the selected validated
   image using [deploy.md](deploy.md#deploying-the-public-hub-from-an-image-ci-built).
   Confirm that agent coordination and authenticated `iris doctor` uploads still
   work if the browser console is closed.
2. To create a new account, the owner uses a fresh password. If the account
   already exists, skip to step 3:

   ```sh
   iris admin console create USERNAME < /private/new-password
   ```

   Only when the database has no console account at all and the hub has no console
   key does `create` also prepare the key. The check and operator creation share
   one transaction with the owner audit records `owner_provision` and
   `activation_prepared`. Key preparation happens only after that transaction
   commits. The response reports `account_created: true`, `key_prepared: true`
   and `restart_required: true`: the operator was made, the key was prepared and
   a restart is needed. Continue at step 5; no separate `activate` is needed.

   On every other hub `create` only makes an enabled operator and reports
   `account_created: true`, `key_prepared: false`. A hub with a valid existing
   key but no accounts needs a restart after account creation: the response
   reports `restart_required: true`, even though no key was prepared. Continue
   at step 5, then sign in with the new operator. On a hub with an account but no
   key, `create` reports `restart_required: false`; activation at step 4 is still
   needed. Creating another account on an already open console also reports
   `restart_required: false` and needs no activation or restart.

   The CLI prints operator creation, key preparation and restart readiness
   independently. `restart_required` is true when the console is closed now and
   a valid key and an enabled operator are available after creation, so a restart
   would open named login. For a member or disabled account, save the intended
   role, disabled state and grants in the access editor and read them back before
   giving its password to anyone.

   If key preparation fails after the transaction commits, the operator remains
   without a key and the running console stays closed. Resolve the key failure
   through trusted owner review, then use `activate` with that operator at step 4;
   do not retry `create` for the same account.
3. If an existing account needs password recovery, use owner `reset`
   deliberately. Otherwise continue at step 4:

   ```sh
   iris admin console reset USERNAME < /private/recovery-password
   ```

   `reset` replaces the password, makes that account an enabled operator,
   deletes its repository grants and revokes all its sessions. It changes
   authority. Record any intended role, disabled state and grants before a
   recovery reset, and restore and read them back in the access editor before
   handing out the new password. It does not recover a lost owner credential
   or bypass named password login.

   New create/reset passwords need at least fifteen Unicode code points and at
   most 1,024 UTF-8 bytes. Protect password files. For create, reset and activate,
   the CLI removes exactly one final LF or CRLF as line framing, then validates
   the remaining password. A bare CR, embedded line endings, spaces and any
   additional final newline remain password bytes. It performs no general
   trimming or normalization. Never put passwords in argv, URLs, logs, chat or
   reports.
4. For a closed hub that did not prepare its key through `create`, including a
   restored database without its key, key loss, or failed fresh-hub key
   preparation, the owner types the enabled operator's password into `activate`
   in their own terminal. This verifies possession of the password and prepares
   the key. No agent sees the password or runs this for them:

   ```sh
   iris admin console activate USERNAME < /private/operator-password
   ```

   For a private Bash prompt instead of a file, keep the variable unexported and
   use the following block. Input is hidden, spaces are preserved, and a read or
   CLI failure remains a failure after the password variable is removed:

   ```bash
   (
     set +x
     set -o pipefail
     if IFS= read -rs -p 'Password: ' iris_password; then
       printf '\n' >&2
       if printf '%s' "$iris_password" | iris admin console activate USERNAME; then
         iris_status=0
       else
         iris_status=$?
       fi
     else
       iris_status=1
     fi
     unset iris_password
     exit "$iris_status"
   )
   ```

   Activation accepts valid UTF-8 input bounded to 1,024 bytes **after** removing
   one optional final LF or CRLF. The server verifies those submitted password
   bytes exactly; it does not remove line endings or normalize passwords.
   It verifies Argon2id credentials only. The fifteen-code-point minimum for
   new passwords does not apply to verification: a short Argon2id password
   retained through an earlier upgrade still activates a restored hub. An
   unreadable stored hash is refused with owner advice: this version does not
   read the stored password format; recover the operator with
   `iris admin console reset <username>`, then activate. Browser login returns
   only the generic invalid-credentials failure. Verification runs outside the
   store lock; a transaction rechecks the account, enabled operator role and
   authentication version, clears every console session, and records `activation_verified`.
   This record proves password verification, not later key persistence.

   Only then does activation create or load the independent protected key.
   Account creation on a fresh hub uses the same key preparation after its
   creation transaction commits. Key preparation refuses malformed existing
   material instead of overwriting it. The required publication policy creates
   a private temporary file in the same directory, protects it before writing
   any key bytes, then writes, syncs and closes it before atomically linking
   it at the final path without replacing an existing file. An existing final
   file is loaded and validated. On the candidate image, activation published
   the key this way, with mode 0600 and the hub's user as its owner
   ([candidate record](research/console-activation-candidate-2026-11-10.md)).
   An interrupted publication and two competing ones were exercised only by
   the tests, with injected failures, not on the hub's volume. If an existing
   file is incomplete or malformed, keep the console closed and have the owner
   review and privately quarantine it before choosing fresh material;
   activation must not overwrite or silently repair it. If persistence fails,
   the running browser gate stays closed; resolve the key failure through
   trusted owner review before retrying. Success returns `restart_required:
   true` and a safe account result. It issues no cookie and does not open this
   process's gate. Activation on an already active named console returns HTTP
   409 (`conflict`) without clearing sessions.
5. After `restart_required: true` from `create` or `activate`, whoever deployed
   restarts the hub with the same database and prepared key path. It need not be
   the owner. The selected image is already deployed; repeating an unchanged
   `compose up -d` does not ensure a restart.

   ```sh
   docker restart iris
   ```

   Normal later restarts retain the key, database and each session's original
   twelve-hour absolute expiry; they do not extend the deadline.
6. The owner opens the hub's URL in a browser (`http://127.0.0.1:7878/` by default
   for a local hub) and personally signs in using the reviewed named operator.
7. The owner reloads the page and still sees their inbox, which is a second
   authenticated read of the session. Whoever deployed checks that agents are
   still listed as active and, five minutes after the restart, that the hub has
   not restarted by itself. The owner stays until these checks are done.

What a candidate has to show before it is released, such as access edits seen
from two browsers, is not part of a cutover. It is listed, with what each check
must show, in
[the rehearsal record](research/console-activation-rehearsal-2026-11-09.md#what-a-candidate-is-checked-for).

## Profiles, locales and access review

The native profile, password, logout and account-language forms retain the
owner-provided UI export provenance recorded in
[ADR 014's adaptation inventory](adr/014-operator-accounts.md#export-inventory-and-adaptation-boundaries).
They use Iris account identities, revisions and authorization. A signed-in
account's UI locale takes precedence over browser preferences; browser
preferences apply while signed out. Correspondence locale remains stored, with
its editor hidden until Iris defines its use. Locale values are data and do not
request translation or carry instructions. Profile and locale changes preserve
sessions; password and access changes revoke them.

After setup and restart, an enabled operator opens Access, selects an account
by its immutable Iris identity, and reviews its role, disabled state and each
repository's access. Members have no grants by default. Choose no grant, `ro` or
`rw` for each existing repository, then save using the displayed account revision.
`ro` permits scoped reading. `rw` also permits sending/replying to agents, with
`rw` required on the sending repository and every destination repository. Neither
mode permits shared-mail acknowledgement, claim changes or administration.
Operators have global console authority without per-repository grants; `none`
is always disabled. A disabled account cannot sign in even when grants exist.

Every accepted access edit revokes all target sessions, so the editor offers Save
only once something has changed. Editing your own access requires signing in
again. A stale revision returns `conflict`: reload the account list, compare the
current saved permissions and deliberately reapply the intended edit. Do not
retry an old revision or overwrite someone else's update. Removing or disabling
the last enabled operator is refused; have the owner provision and verify another
named operator before removing that role. An absent account row is not a reason
to grant access by a similar display name. Save, reload and read access back
before handing out a newly created or recovered account's password.

Use temporary synthetic databases, protected scratch keys and passwords for
rehearsal, with two real browser contexts for live revocation and prepared stale
requests. Also check shared-cookie account replacement across tabs, narrow/wide
layouts, keyboard operation, restart expiry, fresh-key restoration and the proxy
rollback boundary. The
[rehearsal record](research/console-activation-rehearsal-2026-11-09.md) holds what
was observed: real-binary/browser/restore checks passed 44/44 and expanded local
Apache checks 73/73. Those modified builds do not establish readiness of a final
clean candidate or the production proxy.

If login or key loading fails, keep the console closed while agent
coordination continues. Follow owner recovery above and the rollback boundary
below; restoring old login links alongside named accounts is not a supported
recovery path. The owner token and hub OS administrator remain trusted. These
controls cannot contain a compromised browser, hub or trusted host process.

## Restore a database with browser access isolated

A restore can reinstate old passwords, roles, disabled states and repository
grants, and loses mail and coordination writes made after the backup. Preserve
a fresh backup of current state and review those losses first. The agent-only
`iris admin restore-check` is useful but does not verify account authentication.

A backup from before 2026-11-11 10:06 UTC, including older public hub backups,
or a database with imported accounts can hold scrypt hashes. This version accepts
Argon2id only; it never converts stored hashes. It cannot sign in with a scrypt
hash or recover its password. Existing sessions are not revoked merely because
the account has a scrypt hash; withholding the key and activating with fresh key
material prevents restored sessions from retaining browser authority.

Every password set during recovery is fresh and unpredictable and known only to
the person performing recovery. Keep it private until recovery finishes. After
restart, loopback accepts sign-ins; another user of the hub's host could reach
it with a password they already know. Keep the public proxy closed to the
console until all access has been restored and checked.

1. Work on an isolated copy, never the live volume. Keep the **old console key
   withheld**: do not copy it to the restore's default or explicit key path.
   Stop the target hub before replacing its database/WAL/SHM files, as described
   in [the database restore procedure](deploy.md#retention-and-restore-adr-006).
   Keep public browser, login and UI API routes closed throughout recovery.
2. Run the restored candidate as the only process opening that copy, on an
   unused literal-loopback address with a fresh scratch owner token and a new,
   absent console-key path. For example:

   ```sh
   iris serve --listen 127.0.0.1:17878 --db /private/restore/iris.db --token-file /private/restore/hub.token --console-key-file /private/restore/fresh-console.key
   ```

   Omit `--public-url` for this private candidate. Use its explicit loopback URL
   and scratch owner-token file for every administrative CLI call; do not let
   saved client configuration select the production hub. Missing key material
   keeps the candidate's browser gate closed while setup works.
3. Choose and review the recovery operator. If its hash is scrypt or its
   password needs recovery, use `iris admin console reset <username>`. Then run `activate` with a password
   the owner has verified. This creates fresh independent key material and clears
   all restored console sessions in the activation transaction. Restart the
   candidate with that fresh key.
4. Sign in through the private literal-loopback named UI. Review restored
   accounts, roles, disabled states and every grant. Before resetting any other
   scrypt account, including a member or disabled account, record its role,
   disabled state and grants outside the console. The access editor includes
   disabled members' grants. Reset makes the account an enabled operator,
   deletes its grants and revokes its sessions; it does not preserve access.
   In the access editor, click Reload, select the account again, apply its
   recorded role, state and grants, save, reload and check that they read back
   as recorded. A form left open before the reset holds stale access, and saving
   it returns a conflict.

   Reset passwords that must no longer be valid, and remove unwanted restored
   authority through supported owner/account operations. Never use direct SQL
   edits behind a serving hub as a recovery shortcut. There is no separate reset
   command that preserves role and grants.
5. Hand out passwords or reopen access to anyone else only after every account's
   access reads back as recorded. If a save fails or a check differs, keep the
   isolation and the record taken before reset unchanged. Repeat only these
   steps: click Reload, apply that record, save and read back. Never record the
   account again after reset: its temporary state is operator with no grants.
6. Take a consistent `iris admin backup` of the reviewed candidate, then stop it
   before installing that backup in the target; do not copy a live main file alone.
   Preserve the new key with that database, start one named-authentication hub,
   repeat readiness/sign-in/revocation checks, and only then reopen public browser
   access. Do not copy the withheld old key back. The recorded local synthetic
   restore rehearsal passed; a production restore still requires owner review and
   verification of its actual backup, grants and deployment.

## Rollback while preserving the browser boundary

Migration 024 drops the two import ledgers. Rollback across it requires the
matching pre-deploy database backup and loses writes made since that backup.
A binary from before 024 can still open a version-24 database: there is no
maximum-version fence, and it fails only where it touches a dropped table.
This procedure cannot enforce the rollback choice made by the hub's OS owner.

Use a supported backend that retains named authentication, its reviewed database
and console key. A version has named authentication when its commit is the
activation change or a later one on `main`; `iris version` prints the commit, and
`docs/status.md` records the commit of the cutover. Migration compatibility and
lost writes still require the backup review in [deploy.md](deploy.md#deploying-the-public-hub-from-an-image-ci-built).
An arbitrary older Iris binary can reopen legacy browser authority on a newer
database; the newer binary cannot constrain a separately launched old process.

If an older backend must run, **first** close public browser, login and UI API
access at the trusted proxy. On the current public host, changing/reloading Apache
requires `sudo` with the owner's password. Only the owner can install the live
filter, reload the actual vhost and close/drain its established browser sockets.
Agents can prepare, review and rehearse the configuration; they cannot perform
or claim that production step without the owner.

If the owner is unavailable, keep a supported named-authentication image running.
If it cannot remain running, stop the hub or move it off the proxied port rather
than expose an old backend. That fallback makes the whole hub, including agents,
unavailable until the owner has applied and checked the proxy boundary.

In this repository's
[`deploy/apache-iris.conf`](../deploy/apache-iris.conf), place the following in
the `<VirtualHost *:443>` block **before every `ProxyPass`**, including the
WebSocket rules. Keep the existing forwarding-header policy. On an installed
vhost, edit that actual TLS vhost while preserving its certificates and ACME
configuration:

```apache
# While the entire browser console is closed, never forward legacy cookies.
RequestHeader unset Cookie
RewriteEngine On
RewriteCond %{REQUEST_METHOD} !=PUT [OR]
RewriteCond %{THE_REQUEST} !^PUT[[:space:]]+/v1/ui/doctor(?:\?[^[:space:]]*)?[[:space:]]+HTTP/[0-9.]+$
RewriteRule ^/?v1/ui(?:/|$) - [R=503,L]

RewriteCond %{REQUEST_URI} !^/v1/
RewriteCond %{REQUEST_URI} !^/\.well-known/acme-challenge/
RewriteRule ^ - [R=503,L]
```

This defaults to denial outside `/v1/`, except local ACME, and blocks the console
namespace, static browser assets and root page. Only raw literal
`PUT /v1/ui/doctor` (with an optional query) passes the UI namespace rule; the
backend must still authenticate its machine bearer. The temporary Cookie removal
prevents an older backend from treating a legacy browser cookie as authority for
that upload. Other `/v1/` agent and owner
routes remain available. The existing port-80 ACME handling and fixed HTTPS
redirect stay unchanged. ACME remains local on port 443 as well.

Apache documents [negation, exact string comparison and OR conditions](https://httpd.apache.org/docs/2.4/mod/mod_rewrite.html#rewritecond),
[decoded URL matching](https://httpd.apache.org/docs/2.4/mod/mod_rewrite.html#rewriterule)
and [non-redirect response codes for R](https://httpd.apache.org/docs/2.4/rewrite/flags.html#flag_r).
The UI rule matches the decoded path, including percent-encoded letters. The
exception checks the unescaped `THE_REQUEST`; decoded `REQUEST_URI` cannot
reserve the literal doctor spelling. The original decoded-only exception let
two encoded doctor PUT spellings reach the old backend; the corrected raw check
denied them with 503. Encoded path separators returned 404 before rewriting
under Apache's default [AllowEncodedSlashes Off](https://httpd.apache.org/docs/2.4/mod/core.html#allowencodedslashes).
The 503 flags stop rewriting without redirecting. The temporary
[RequestHeader unset Cookie](https://httpd.apache.org/docs/2.4/mod/mod_headers.html#requestheader)
removes the request cookie before the backend handler, preventing legacy-cookie
authority on the permitted doctor upload.

The owner must run `apachectl configtest`, reload the intended vhost, and verify
the block **before** starting the older backend. Check root/assets, login and UI
API requests return 503, and the permitted doctor PUT returns 401 without a valid
bearer even with a valid old browser cookie. Authenticated doctor PUT and agent
requests must still reach the backend. Check trailing/double slashes, dot segments,
encoded letters/separators, other methods, ACME and the normal HTTP redirect
against the actual configuration. Close/drain existing public console
WebSockets too: a rule for new requests does not revoke established connections.
Record the tested Apache version, vhost, backend binary and results before using
this boundary operationally. Keep the block until a supported named backend and
reviewed state are verified, then remove both the temporary rewrite block and
`RequestHeader unset Cookie` before reopening named browser login. Run configtest
and reload, then sign in and require the next authenticated `GET /v1/ui/session`
to return 200. Leaving Cookie removal in place allows the login response to set
a cookie, but strips that cookie from later requests, so the next session GET
returns 401. No production Apache change has been applied by this runbook.
