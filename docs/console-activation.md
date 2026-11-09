# Activate, recover and restore the named console

The console uses named accounts under [ADR 019](adr/019-console-activation.md).
Deploying a binary does not authorize an account import or a production cutover.
This runbook describes owner operations; perform a real import only after its
source, manifest, backup and apply have been explicitly approved.

**Before any deployment command**, the owner must explicitly confirm in their
own terminal that they are present and ready to complete activation, the second
restart and sign-in checks. On the first cutover from the legacy binary,
deployment closes browser access immediately; it stays closed until the owner
activates a named operator and restarts the hub again. Agent coordination keeps
working during that closed-console stage. Agents may prepare and rehearse this
procedure; they must not activate real accounts in place of the owner. Keep
passwords and machine credentials out of chat.

Local rehearsal results are recorded [below](#provisional-rehearsal-evidence):
44 real-binary/browser/restore checks and 73 Apache rollback checks passed.
The binary was built from a modified checkout. Release validation must be recorded
separately at the exact PR head, including the atomic key-publication correction,
a clean rebuilt binary and complete local checks. The historical results below
do not substitute for that validation or authorize production activation or import.
No production configuration or real account data was changed for the rehearsals.

## Startup and credentials

`iris serve` loads an existing independent console key from
`<database>.console.key`. For `--db /data/iris.db`, the default is
`/data/iris.db.console.key`. `--console-key-file /private/console.key` selects
another hub-local path. Key files require mode 0600 on Unix or the owner-only
Windows DACL. Keep this key separate from the owner bearer and source-system keys.

A missing, unreadable or malformed key, reuse of owner-token material, or absence
of an enabled operator leaves browser authentication closed. Startup never creates
the key. Agent coordination, authenticated `PUT /v1/ui/doctor`, and owner account
create/reset/import remain available. A closed browser namespace reports
`X-Iris-Console-Mode: closed`; the frontend reports that the console is unavailable
and asks the user to contact the owner. An untrusted browser transport can also
produce that header; it does not by itself diagnose missing activation.
Old login links, `/v1/ui/links`, `/ui/login` and the `iris_session` cookie grant
no browser authority. An owner or host bearer cannot create a browser session.
Legacy login grants and cookies existed only in process maps, never in a SQLite
legacy-session table. Deploying the new binary and restarting discards those maps.
Activation clears the named `ConsoleSession` table, not persisted legacy grants.
An old binary also starts with empty maps after restart, but can mint fresh legacy
grants; that is why an older rollback still needs the proxy boundary below.

The hub OS owner and owner bearer remain trusted. This gate does not prevent an
OS administrator from replacing key material or editing stopped database files.
There is no hot key reload.

## Owner setup and activation

1. After the owner's terminal confirmation, take a fresh consistent database
   backup **before replacing the running image**. Protect the owner credential
   separately and preserve any existing console key privately. Keep public
   browser access isolated during cutover.
2. Deploy the selected validated CI image using
   [deploy.md](deploy.md#deploying-the-public-hub-from-an-image-ci-built).
   On this first cutover the new process has no activated console key, so the
   browser is immediately closed while agent routes and authenticated doctor
   uploads continue. Confirm those machine paths still work. Do not replace a
   working Apache vhost merely to change the backend image.
3. Configure the CLI for the intended hub and its protected owner-token file.
   The present owner creates an operator or performs the separately approved
   import from [accounts-import.md](accounts-import.md). For an import, review
   its dry-run, run the approved apply, then repeat the same dry-run and require
   the expected skips before activation. A deployment never implicitly approves
   importing real accounts. Owner creation and recovery work while the browser
   gate is closed:

   ```sh
   iris admin console create ReviewedOperator < /private/new-password
   iris admin console reset ReviewedOperator < /private/recovery-password
   ```

   `reset` recovers that named account as an enabled operator and revokes all its
   sessions. Use it deliberately; it changes authority. New create/reset passwords
   need at least fifteen Unicode code points and at most 1,024 UTF-8 bytes.
   Protect password files. For create, reset and activate, the CLI removes
   exactly one final LF or CRLF as line framing, then validates the remaining
   password. A bare CR, embedded line endings, spaces and any additional final
   newline remain password bytes. It performs no general trimming or
   normalization. Never put passwords in argv, URLs, logs, chat or reports.
4. The owner personally verifies possession of the enabled operator's password
   and prepares the key from their own private terminal:

   ```sh
   iris admin console activate ReviewedOperator < /private/operator-password
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
       if printf '%s' "$iris_password" | iris admin console activate ReviewedOperator; then
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
   It verifies supported Argon2id or imported bounded scrypt credentials. The
   fifteen-code-point minimum for new passwords does not apply to verification
   of an imported password. Verification runs outside the store lock; a
   transaction rechecks the account, enabled operator role and authentication
   version, clears every console session, and records `activation_verified`.
   This record proves password verification, not later key persistence.

   Only then does activation create or load the independent protected key. It
   refuses malformed existing material instead of overwriting it. The required
   publication policy creates a private temporary file in the same directory,
   protects it before writing any key bytes, then writes, syncs and closes it
   before atomically linking it at the final path without replacing an existing
   file. An existing final file is loaded and validated. Validation of
   that publication correction is still pending for the final candidate.
   If an existing file is incomplete or malformed, keep the console closed and
   have the owner review and privately quarantine it before choosing fresh material;
   activation must not overwrite or silently repair it. If persistence
   fails, the running browser gate stays closed; resolve the key failure through
   trusted owner review before retrying. Success returns `restart_required: true`
   and a safe account result. It issues no cookie and does not open this process's
   gate. Activation on an already active named console returns HTTP 409
   (`conflict`) without clearing sessions.
5. After `restart_required: true`, the owner performs the **second restart** with
   the same database and prepared key path. The selected image is already
   deployed; repeating an unchanged `compose up -d` does not ensure a restart.

   ```sh
   docker restart iris
   ```

   Normal later restarts retain the key, database and each session's original twelve-hour absolute
   expiry; they do not extend the deadline.
6. The owner opens the ordinary configured Iris URL, or runs `iris ui`, and
   personally signs in using the reviewed named operator. That command resolves
   and validates the origin, prints it and opens the sign-in page best-effort.
   It does not read the machine token (`Token()`), mint a link or authenticate a
   person.
7. Verify the intended capabilities, a subsequent authenticated session GET,
   agent status and authenticated doctor uploads. The owner remains present
   through these checks and any recovery needed before public reopening.

Before reopening public browser access, use two separate browser contexts to
check member default denial, explicit `ro` then `rw` grants, stale access edits,
last-operator refusal, live revocation and a prepared send/retry after downgrade.
Also replace the account in another tab sharing cookies: the old tab's next
authenticated read must discard its previous console state. Check narrow/wide
layouts and keyboard operation. The local browser results below are provisional;
repeat the required checks on the final clean candidate and target deployment.

## Restore a database with browser access isolated

A restore can reinstate old passwords, roles, disabled states and repository
grants, and loses mail and coordination writes made after the backup. Preserve
a fresh backup of current state and review those losses first. The agent-only
`iris admin restore-check` is useful but does not verify account authentication.

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
3. Review the restored operator identity. Recover its password with owner `reset`
   if required, then run `activate` with a password the owner has verified. This
   creates fresh independent key material and clears all restored console sessions
   in the activation transaction. Restart the candidate with that fresh key.
4. Sign in through the private literal-loopback named UI. Review restored
   accounts, roles, disabled states and every grant before public reopening.
   Reset passwords that must no longer be valid, and remove unwanted restored
   authority through supported owner/account operations. Never use old links or
   direct SQL edits behind a serving hub as a recovery shortcut.
5. Take a consistent `iris admin backup` of the reviewed candidate, then stop it
   before installing that backup in the target; do not copy a live main file alone.
   Preserve the new key with that database, start one named-authentication hub,
   repeat readiness/sign-in/revocation checks, and only then reopen public browser
   access. Do not copy the withheld old key back. The local synthetic restore
   rehearsal below passed; a production restore still requires owner review and
   verification of its actual backup, grants and deployment.

## Rollback while preserving the browser boundary

Use a supported backend that retains named authentication, its reviewed database
and console key. Migration compatibility and lost writes still require the
backup review in [deploy.md](deploy.md#deploying-the-public-hub-from-an-image-ci-built).
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
namespace, old link exchange, static browser assets and root page. Only raw literal
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

## Provisional rehearsal evidence

Both rehearsals ran on 2026-11-09 with temporary synthetic accounts/databases and
literal-loopback listeners. Reports contain safe checks and identities, not
credentials. Paths below are local evidence locations, not shipped release assets.

| Rehearsal | Observed result | Limits |
|---|---|---|
| Rebuilt Iris and real browser contexts | 44/44 checks passed | Modified source `0fe50af`; final clean candidate and atomic key-publication correction remain unvalidated |
| Actual Apache 2.4.69 and old Iris `bc1eae9676c3a9f6cb96b1ae08233026f8b8e4e9` | 73/73 checks passed with the corrected raw doctor condition and Cookie stripping | Rendered local vhost, self-signed TLS, HTTP/1.1; no installed production configuration changes |

The real-binary report is `/tmp/iris-real-console-bdGopY/report.json`:

| Identity | SHA-256 |
|---|---|
| Report | `7e09a427327606b87ae934bc70b438f63fee50cd007a95bd18a1c20eaaba3cef` |
| Executed Iris binary | `f407697c4bf93c99759c34b60d75c1dfc3ef947372166ac2fbe1125e2b793211` |
| Embedded UI manifest | `488a2c470e170b9204a4c360e515b0dca83e7a7cf9094d12ee886a42a23238b8` |
| UI patch, `/tmp/iris-console-activation-ui.patch` | `c4037d8f08c389b9a1fa4c5c388c0397cc49cd98a67559807172a215b24a4c8e` |
| Browser driver, `v4-observed-read-restored-cache-authorization` | `78d81a7a79f5e62c7063b53c40223806bf27922edc1a5318893985ab62cde47d` |

The binary reports source `0fe50af` with `source_modified: true`; the embedded UI
includes uncommitted changes. Its observed workflows included actual owner
create/reset/import CLI calls, real KDF verification and upgrade of a short
synthetic imported scrypt password, activation without hot opening, restart and
unchanged absolute session expiry. Two browser contexts exercised operator-to-member
access editing, a live member stream and a prepared draft. `rw` to `ro` downgrade
closed the session and cleared the draft; stale prepared sends and cached retries
returned 401, and a newly authenticated `ro` send returned 403. An actual next read
with the old mounted CSRF and a replacement shared cookie returned 401.

The actual backup CLI captured named sessions. With the old key withheld, the
restored hub stayed closed; fresh-key activation cleared all restored sessions,
and tested old cookies were rejected after restart. Through the private UI the
owner reviewed a resurrected `rw` grant and reduced it to `ro` before reopening.
A fresh restored `ro` session could read the message but received 403 replaying a
previously successful cached `rw` send. These are observed synthetic flows, not
proof about a production backup or every historical credential.

Initial 50-case Apache evidence is `/tmp/iris-apache-rollback-w578u1xj/`: `report.md`,
`metadata.json`, rendered `httpd.conf`, `rollback-block.conf`, `configtest.txt`,
sanitized logs and independent cleanup verification. The old binary SHA-256 was
`b0cb7dd39e7d99f612edbca06a73ca1446972e159f62a5857a458a045076f58f`.
Apache used pinned image
`httpd@sha256:f6a86043744215e4be90cf8804375d90022a711d892c9aa2d7085c089ee2cb54`.
The follow-up `/tmp/iris-apache-rollback-paths-5qq8yavj/metadata.json` replayed
those checks and added raw-path cases, passing 73/73. Its recorded outbound
request-line bytes match the supplied raw targets, so the client did not normalize
away the double slashes or dot segments. The documented Apache block SHA-256 is
`21a003def78e3012537af069fdbed56feb04344273d60c44bb21ecd3f40ec0d3`,
matching both rehearsals exactly.

| Actual request/control | Observed status |
|---|---|
| Root, assets, login links, plain/encoded UI paths and actual UI WebSocket Upgrade | 503 |
| Doctor GET, HEAD, POST, PATCH, DELETE, OPTIONS and TRACE; PUT with a trailing slash or encoded doctor spelling | 503 |
| Literal doctor PUT, including an optional query, with valid owner/host bearer | 200 |
| Doctor PUT with no bearer or invalid bearer, including a valid old cookie | 401 |
| Agent status and actual registration | 200 |
| HTTP and HTTPS local ACME proof | 200 |
| Normal HTTP redirect to fixed HTTPS origin | 308 |
| Status over separately negotiated TLS 1.2 and TLS 1.3 | 200 |
| Encoded separators `/v1%2fui/overview` and `/v1/ui%2fdoctor` | 404 before the backend |
| Raw UI GET `//v1/ui/overview`, `/v1//ui/overview`, `/v1/./ui/overview`, `/v1/x/../ui/overview`, each with cookie-only and machine+cookie | 503 in all eight cases |
| Raw doctor PUT `//v1/ui/doctor`, `/v1//ui/doctor`, `/v1/./ui/doctor`, `/v1/x/../ui/doctor`, `/v1/ui/doctor/`, `/v1/ui/doctor/.`, each with cookie-only and machine+cookie | 503 in all twelve cases |

All twenty noncanonical UI/doctor cases were denied without redirects or
successful authentication. A normal literal doctor query returned 200 with a
valid machine bearer and 401 with the same valid legacy cookie alone.
Production certificate trust, issuance and
renewal, external DNS/networking, HTTP/2, rejection of older TLS versions and
draining established WebSockets across reload were not tested. The new Upgrade
was blocked, but an existing connection still requires explicit closure. Owned
temporary processes and containers were stopped/removed; no other service was
restarted. Record final clean-candidate local gates in the associated PR and verify
target-deployment readiness before owner activation.

A separate nine-request named-console control is recorded in
`/tmp/iris-named-cookie-strip-ir2vzzqq/metadata.json`. With the preliminary
`f407697c` binary, normal proxy login returned 200 and `Set-Cookie`; the same
session survived a restart. Retaining only request `Cookie` removal caused the
next session read to return 401, while the same cookie worked directly. A login
challenge still set a response cookie, but its submission returned 403 because
the request cookie was stripped. Removing that single directive restored the
original session to 200. This validates the leave-closed-mode check without
claiming anything about the final release binary or installed production vhost.
