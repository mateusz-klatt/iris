# Activate, recover and restore the named console

The console uses named accounts under [ADR 019](adr/019-console-activation.md).
Deploying a binary does not authorize an account import or a production cutover.
This runbook describes owner operations; perform a real import only after its
source, manifest, backup and apply have been explicitly approved.

**Before any deployment command**, the owner tells whoever will deploy, in that
session, that he is present and ready for activation, the second restart and the
sign-in checks. On the first cutover from the legacy binary, deployment closes
browser access immediately; it stays closed until the owner activates a named
operator and the hub is restarted again. Agent coordination keeps working during
that closed-console stage. Agents may prepare and rehearse this procedure; they
must not activate real accounts in place of the owner. Keep passwords and
machine credentials out of chat.

Rehearsals of this procedure are recorded in
[the rehearsal record](research/console-activation-rehearsal-2026-11-09.md), with
their limits. A rehearsal is evidence about the binary it ran; it authorizes no
production import or activation.

## First cutover on the public hub

This is the whole first cutover, in order. The sections after it are the
reference for each step.

Words used here. "The console" is the hub's web page for people; agents do not
use it and keep working through every step. "The coordinator" is the agent
session on the hub's machine that deploys the hub. "The activation version" is
the first version with password login. It has no login links, so from the moment
it is deployed the console is closed to everyone until activation and one more
restart. "A dry run" is an import that reports what it would do and writes
nothing.

Only the owner knows the password. No agent may see it, type it or ask for it.
Only the owner can change the web server on the hub's machine, because that
needs `sudo` and his password.

Before the day, without the owner: the activation version is merged with every
check green and CI has built its image; a rehearsal on that image is recorded;
and this section has been read by two simpler models that both restated it
correctly.

On the day the owner is present. The steps take about 15 minutes; set aside 30.

1. The owner writes "start the login cutover" in the coordinator's session.
   Nothing below happens before that.
2. The coordinator takes backup 1 of the hub's database and copies it off the
   hub's volume, as in
   [Backup, review and apply](accounts-import.md#backup-review-and-apply), step 1.
3. The coordinator runs the import's dry run, with the source file and the
   manifest that the owner has already reviewed, and shows the result to the
   owner. Expected: every account in that source is listed as created, none
   skipped, none quarantined. Any other result: stop, and tell the owner what
   the dry run printed. The hub is unchanged; only the backup file was added.
4. The owner writes "import" in the coordinator's session. The coordinator runs
   the import with `--apply`, then the same dry run again. Expected now: none
   created, every account skipped, none quarantined. Any other result: stop, and
   tell the owner what was printed. The accounts exist but cannot be used yet,
   and the console still works with links.
5. The coordinator deploys the activation version, as in
   [Deploying the public hub from an image CI built](deploy.md#deploying-the-public-hub-from-an-image-ci-built).
   That procedure takes backup 2 just before it replaces the running hub, and
   keeps the previous image under its commit. From now on the console is closed
   and the hub's log says `Iris console closed`. Agents keep working.
6. The owner, in his own terminal on the hub's machine, runs this with the
   operator's username in place of `<username>`, and types the password:

   ```sh
   IFS= read -rs -p 'Password: ' p && echo && printf '%s' "$p" | IRIS_HUB_URL=http://127.0.0.1:7878 IRIS_TOKEN_FILE="$HOME/.config/iris/hub.token" iris admin console activate <username>; unset p
   ```

   The password is not shown and is not stored. Expected: the answer contains
   `"restart_required":true`. If the owner mistypes, he tries again. The hub
   allows five tries in 15 minutes; after the fifth wrong try he waits
   15 minutes, counted from that try. If two careful tries in a row are refused,
   he stops trying: that is a fault, see below.
7. The coordinator restarts the hub with `docker restart iris`. Deploying again
   would do nothing here, because the image has not changed. Expected: the hub's
   log says `Iris console open`.
8. The owner opens the hub's address in a browser and signs in with the
   username and the password.
9. Three checks. The owner reloads the page and says whether he still sees his
   inbox. The coordinator checks with its Iris `peers` tool that the other
   sessions are still listed as active. Five minutes after step 7 the
   coordinator checks that `docker inspect --format '{{.RestartCount}}' iris`
   prints 0.
10. The coordinator adds the commit and the time to the deploy log on the hub's
    machine, and sends the other agents one Iris message: sign-in is by password
    now, and login links are gone.

If something goes wrong:

- In steps 2 to 4 the console is not closed. Stop and report. Nothing needs to
  be undone, because imported accounts cannot be used before activation.
- In steps 5 to 8 a fault is one of three things: the hub does not start;
  activation refuses two careful tries; or the sign-in page does not load or
  refuses the username and password that activation accepted. The coordinator
  tells the owner in its session what it sees. The fault is fixed when the step
  that failed has given its expected result. If that has not happened
  10 minutes after the failure, the coordinator proposes to go back, and the
  owner decides.
- Going back means exactly this: start the previous image again, as the
  paragraph "To roll back" in
  [deploy.md](deploy.md#deploying-the-public-hub-from-an-image-ci-built) says.
  The database stays as it is, because the activation version adds no
  migration. Nothing is restored and no message is lost. Link login works
  again, and the imported accounts stay, unused.
- After a cutover that succeeded, a version from before it must never serve the
  public address as it is, because it would bring login links back.
  [Rollback while preserving the browser boundary](#rollback-while-preserving-the-browser-boundary)
  says what has to happen first and who can do it.

Who may do what. Only the owner: say "start the login cutover"; say "import";
type the password; decide to go back; change the web server. The coordinator:
the backups, the dry runs, the import after the owner's word, the deploy, the
restart, the checks of the sessions and of the restart count, and going back
after the owner's decision. Nobody: skip a step, or continue after a result this
section does not expect.

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

These are the same steps for any hub, with the detail the first section leaves
out. "Whoever deploys" is the person or the session that runs the deploy
procedure; on the public hub that is the coordinator.

1. After the owner's confirmation, whoever deploys takes a fresh consistent
   database backup **before replacing the running image**, and keeps a copy off
   the hub's volume. Protect the owner credential separately and preserve any
   existing console key privately. On a first cutover nothing has to be closed
   at the proxy: the new binary keeps the console closed by itself until
   activation and the second restart.
2. Whoever deploys installs the selected validated CI image using
   [deploy.md](deploy.md#deploying-the-public-hub-from-an-image-ci-built).
   On this first cutover the new process has no activated console key, so the
   browser is immediately closed while agent routes and authenticated doctor
   uploads continue. Confirm both: an agent's status call answers, and
   `iris doctor` on a host still uploads its report. Do not replace a working
   Apache vhost merely to change the backend image.
3. Configure the CLI for the intended hub and its protected owner-token file.
   The owner creates an operator, or approves the import described in
   [accounts-import.md](accounts-import.md#backup-review-and-apply). That section
   gives the expected results: the dry run lists every account it would create;
   the apply creates them; the same dry run afterwards reports each of them as
   skipped and none as created. An approved import can also run before step 2,
   on the version that is running: no migration separates the two versions, and
   an import that stops there closes nothing. The public hub's first cutover
   does it that way. A deployment never implicitly approves importing real
   accounts. Owner creation and recovery work while the browser gate is closed:

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
4. The owner types the enabled operator's password into `activate`, in his own
   terminal. That is the proof that he knows it, and it is what prepares the
   key. No agent sees the password or runs this for him:

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
5. After `restart_required: true`, whoever deployed performs the **second
   restart** with the same database and prepared key path. It need not be the
   owner. The selected image is already deployed; repeating an unchanged
   `compose up -d` does not ensure a restart.

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
7. Three checks close a cutover, the same three as in the first section. The
   owner reloads the page and still sees his inbox, which is a second
   authenticated read of his session. Whoever deployed checks that agents are
   still listed as active and, five minutes after the second restart, that the
   hub has not restarted by itself. The owner stays until these are done.

What a candidate has to show before it is released, such as access edits seen
from two browsers, is not part of a cutover. It is listed, with what each check
must show, in
[the rehearsal record](research/console-activation-rehearsal-2026-11-09.md#what-a-candidate-is-checked-for).

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
and console key. A version has named authentication when its commit is the
activation change or a later one on `main`; `iris version` prints the commit, and
`docs/status.md` records the commit of the cutover. Migration compatibility and lost writes still require the
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
