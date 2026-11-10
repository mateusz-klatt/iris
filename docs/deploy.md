# Public Iris hub

Before running commands for the first named-console deployment, the owner must
explicitly confirm in their own terminal that they are present and ready to
complete activation, the second restart and sign-in checks. Take the backup
before deploying: the first replacement of the legacy binary immediately closes
browser access while agent coordination continues. An approved import apply and
its repeated dry-run precede owner activation; then the owner runs
`docker restart iris`, signs in and completes the checks. Agents may prepare and
rehearse but must not activate real accounts on the owner's behalf. Passwords
and machine credentials stay out of chat.

The intended path is client HTTPS → Cloudflare HTTPS → Apache → HTTP on
127.0.0.1:7878. Use Cloudflare Full (strict), a valid origin certificate, and
WebSocket support. Disable caching and script rewriting/injection for this hostname;
Iris sends no-store and a CSP that permits only its own scripts. Cloudflare terminates
TLS and can see credentials and message content. This is a shared-trust service for
one developer or a small team, not a multi-tenant boundary (D13).

The browser console uses named accounts. Follow
[console activation and recovery](console-activation.md) before opening browser
access. The owner creates an operator or performs a separately approved
[account import](accounts-import.md), verifies its password with
`iris admin console activate <username> < /private/password`, then personally
performs the second restart. Create/reset/activate remove exactly one final LF
or CRLF from stdin as line framing before password validation; all other bytes
are preserved and HTTP password verification remains exact.
Deploying this code does not authorize a real import or activate a missing key.
Old login links and cookies grant no browser authority, and `iris ui` opens the
ordinary sign-in URL without reading a machine token.

`iris serve` loads a separate protected 256-bit console key from
`<database>.console.key`, or the path selected by `--console-key-file`.
It creates no key during ordinary startup. Missing, unreadable, malformed or
owner-token-reusing material leaves browser authentication closed while agents,
authenticated doctor uploads and owner setup/recovery/import remain available.
An enabled operator is also required. Normal restarts retain that key, database
and each session's original twelve-hour absolute expiry; there is no hot reload.

Protect the database, owner token and independent console key separately in
backups. A restore can reinstate older passwords and grants. Restore a copy in
isolation with the old key withheld, activate a verified operator using fresh
key material and cleared sessions, then review restored authority through a
private literal-loopback named UI before public reopening. See the exact
[restore and rollback boundary](console-activation.md#restore-a-database-with-browser-access-isolated).
Local real-binary/browser/restore checks passed 44/44 on a modified checkout;
the expanded actual local Apache rollback rehearsal passed 73/73. See the
[provisional evidence and limits](research/console-activation-rehearsal-2026-11-09.md).
Final clean-candidate validation and target-deployment checks remain pending.

The files in `deploy/` are written for this topology. No release binary is needed:
`deploy/Dockerfile` builds the locked Node console and embeds it in a CGO_ENABLED=0
Go binary. Its final scratch image runs as uid/gid 65532 and contains the binary,
CA certificates and a writable /data. `deploy/compose.yaml` uses Linux host networking
so the loopback listener is reachable by the host's Apache; no port is published.
The named `iris-data` volume holds the database, WAL and token on local Linux storage.
Do not put it on WSL drvfs, NFS or another network filesystem. Keep volume backups
and the token private. A bind-mounted replacement must be writable by uid 65532.

### Deploying the public hub from an image CI built

The public hub's machine does other work, and building the console and the binary starves it, so
it compiles nothing. On every push to `main`, a CI job of its own builds the deployment image with
`make image` from a checkout no other step has touched, checks that it names that commit with a
clean source, and keeps it for 7 days as a workflow
artifact named `iris-image-<full commit>`. The artifact holds `iris-image.tar.gz`, from `docker
save`, and `iris-image.txt`, which records the commit, the platform, the runner's image ID and the
layer list. The image is linux/amd64, which is what the hub's machine runs.

A pull request's run keeps an image too, for one day, as `iris-rehearsal-image-pr<number>-<head
commit>`. It is built from the merge of the pull request's head into `main` and names that merge
commit, which exists nowhere else. It is there to rehearse a change before it is merged, on a
loopback address with a scratch database. It is never deployed: the procedure below takes only
`iris-image-<commit>` from a run of a push to `main`.

To deploy, in one shell on the hub's machine, from a checkout of that commit (for its
`deploy/compose.yaml`):

1. Select the successful run of a push to `main` for exactly that commit, and download its image:

   ```sh
   commit=$(git rev-parse HEAD)
   run=$(gh run list --workflow ci.yml --branch main --event push --commit "$commit" --status success --limit 1 --json databaseId --jq '.[0].databaseId')
   test -n "$run"
   attempt=$(gh api "repos/mateusz-klatt/iris/actions/runs/$run" --jq .run_attempt)
   artifact=$(gh api "repos/mateusz-klatt/iris/actions/runs/$run/artifacts" --jq "[.artifacts[] | select(.name == \"iris-image-$commit\")] | max_by(.created_at) | .id")
   work=$(mktemp -d)
   gh run download "$run" --name "iris-image-$commit" --dir "$work"
   archive=$(sha256sum "$work/iris-image.tar.gz" | cut -d' ' -f1)
   grep -qx "commit=$commit" "$work/iris-image.txt"
   grep -qx "platform=linux/amd64" "$work/iris-image.txt"
   ```

2. Keep the image that is running under its own commit, then load the new one, give it its commit
   tag and compare it. Loading moves the `iris-iris:latest` tag and changes nothing that is running:

   ```sh
   previous=$(docker exec iris /iris version | sed -n 's/.*(commit \([0-9a-f]*\);.*/\1/p')
   test -n "$previous"
   docker tag "$(docker inspect --format '{{.Image}}' iris)" "iris-iris:$previous"
   docker load -i "$work/iris-image.tar.gz"
   docker tag iris-iris:latest "iris-iris:$commit"
   test "$(docker image inspect --format '{{json .RootFS.Layers}}' "iris-iris:$commit")" = "$(sed -n 's/^layers=//p' "$work/iris-image.txt")"
   test "$(docker run --rm "iris-iris:$commit" version)" = "dev (commit $commit; source clean; console unverified)"
   image=$(docker image inspect --format '{{.Id}}' "iris-iris:$commit")
   ```

   The comparison uses the layer list because an image ID depends on how a Docker daemon stores
   images: the runner uses containerd's image store, and a daemon on overlay2 may report a different
   ID for the same image.

3. Back up the database, as in [Retention and restore](#retention-and-restore-adr-006), and copy
   the file off the volume:

   ```sh
   docker exec iris /iris admin backup /data/iris.db "/data/before-$commit.db"
   ```

4. Replace the container. `--no-build` and `--pull never` make a missing or misnamed image an error
   instead of a compile or a pull here, which the compose file's `build` section would otherwise
   allow:

   ```sh
   docker compose -f deploy/compose.yaml up -d --no-build --pull never
   ```

5. Take the command-line `iris` for this machine out of the same image, where it is at `/iris` (the
   release image has it at `/usr/local/bin/iris`):

   ```sh
   container=$(docker create "iris-iris:$commit")
   docker cp "$container:/iris" "$work/iris"
   docker rm "$container"
   install -m 755 "$work/iris" "$HOME/.local/bin/iris"
   ```

6. The image has no healthcheck, so check readiness by hand, then record what was deployed. Repeat
   the restart check after a minute, because a crash loop shows only as a rising count:

   ```sh
   curl -fsS --retry 30 --retry-connrefused --retry-delay 1 http://127.0.0.1:7878/v1/status
   test "$(docker inspect --format '{{.State.Status}} {{.RestartCount}}' iris)" = "running 0"
   version="dev (commit $commit; source clean; console unverified)"
   test "$(docker exec iris /iris version)" = "$version"
   test "$(iris version)" = "$version"
   printf '%s commit=%s run=%s attempt=%s artifact=%s archive_sha256=%s image_id=%s\n' "$(date -u +%FT%TZ)" "$commit" "$run" "$attempt" "$artifact" "$archive" "$image" >> "$HOME/iris-deploys.log"
   ```

   These values answer different questions. The run and attempt name the CI execution, the artifact
   ID names GitHub's stored copy, the checksum identifies the downloaded archive, and the image ID
   identifies the loaded image on this host. None of them is a registry digest: `docker save` and
   `docker load` produce none.

Before rolling back, verify the previous image retains supported named authentication.
For an older backend, first close public browser/login/UI API access using the
[Apache rollback boundary](console-activation.md#rollback-while-preserving-the-browser-boundary),
preserving agent routes and authenticated doctor PUT. Do this before the old backend starts;
a binary or database rollback alone can restore legacy browser authority.
The current host requires the owner's password for Apache `sudo`; only the owner
can install/reload the live filter and drain existing browser connections. Agents
can prepare and rehearse it. While the owner is unavailable, keep a supported
named-authentication image, or stop/move the hub off the proxied port; the latter
makes agent coordination unavailable too. Do not start an exposed old backend.
Then return the tag to the reviewed previous image, replace the container the same way, and take
the command-line `iris` from that image with step 5:

```sh
docker tag "iris-iris:$previous" iris-iris:latest
docker compose -f deploy/compose.yaml up -d --no-build --pull never
```

The artifact expires after 7 days, so the images on the host are the rollback. Keep the one running
and the two before it, and remove older ones with `docker image rm iris-iris:<commit>`.

Going back to the image alone is enough only when the deployment added no migration; check with
`git log --oneline "$previous..$commit" -- internal/store/migrations`. Iris does not refuse an older
hub on a newer database, and it is not safe there. After a migration, stop the hub and restore the
backup from step 3, which also loses the mail and other writes made since.

What ties the image to the source is that it came from the CI run of that commit, by GitHub's record
of the run. Nothing signs it, and anyone who can push to `main` or change the workflow decides what
CI builds. The version string is an assertion made by the build: the checks above catch the wrong
file, not a dishonest build. CI runs only `iris version` from this image; its non-root run on a
fresh volume uses the release image, so step 4 is the first time this image serves.

### Building on the host

Another deployment without that constraint can build from the repository root:

```sh
make image
docker compose -f deploy/compose.yaml up -d
```

`make image` reads the commit before Docker collects the build context, so a checkout that something
else can change may stamp a commit other than the one built. CI's checkout is exclusive, so the race
does not arise there.

Both ways start the sole container named `iris`, with restart unless-stopped.
`make image` stamps the full commit and source checkout's dirty state before building the console;
`VERSION` optionally names the build. Direct Docker builds must pass `COMMIT` and `MODIFIED`
(`true`, `false`, or `unknown`) as build arguments. The context excludes `.git`, so the image
cannot discover that identity by itself. `iris version` inside the container and authenticated
hub status report the same identity; unauthenticated status remains only `{"status":"ok"}`.
The hub creates /data/hub.token once. Retrieve it through an administrator-controlled
channel and distribute it privately; do not paste it into chat, URLs or shell history.
Setup cannot fetch or provision this credential. Back up the database with
`iris admin backup` (below) or while the hub is stopped; copying only a live .db can omit WAL data.

### Retention and restore (ADR 006)

The hub sweeps at start and then hourly. It drops events after 7 days, idempotency records after a
day, claims finished 30 days ago, and Executions that ended 30 days ago and that no mail names. Mail
is never deleted, so the database grows with it; `GET /v1/status` and `iris doctor` show its size.
The first sweep after an upgrade logs what it removed, which can be a lot once.

To take a backup while the hub runs, on the hub's machine:

```sh
iris admin backup /data/iris.db /path/to/new-backup.db
```

In the container built from `deploy/Dockerfile`, run it as
`docker exec iris /iris admin backup /data/iris.db /data/<name>.db` and copy the file out; the
release image has the binary at `/usr/local/bin/iris`. The copy keeps SQLite's temporary storage
in memory, so the image's lack of a temporary directory does not matter. It reads the database read-only, in one read transaction (SQLite's
`VACUUM INTO`), so the copy is one moment of the live database with its WAL applied. It writes an
owner-only `<name>.partial` beside the destination, runs SQLite's integrity check on it, and only
then links it into place. It refuses a destination or `.partial` file that exists, the live
database among them, and never replaces a file that appears meanwhile. A failed backup leaves
nothing behind. It prints the schema version, the counts of messages and events, and the
restore-check command for the new file. It does not copy `hub.token` or the independent
console key; back up both separately and keep all credential-bearing files private.

To test a restore, never use the live volume. Copy a backup out of the archive, then run:

```sh
iris admin restore-check /path/to/backup-copy.db /path/to/empty-scratch-dir
```

It copies the backup into the scratch directory and opens it there, as a restored hub starts, in a
throwaway hub on a loopback port with a token of its own. It then checks:

- that every session active at backup time is recovering, none live;
- that the restored hub answers status;
- that a session presenting a generation the backup never saw gets no restored authority;
- that a monitor whose cursor is ahead of the restored history gets a reset.

It also prints what the first retention sweep would remove. A failed check prints `FAIL` and exits
non-zero. This checks agent recovery, not named browser authentication. Before a real
restore, stop the hub and follow [console isolation, fresh-key activation and grant
review](console-activation.md#restore-a-database-with-browser-access-isolated). Replace
`iris.db` with the reviewed backup while stopped, removing its old `-wal` and `-shm`
files. Keep the old console key withheld. Agents then reattach with proof or register
afresh; reopen browser access only after the named-console recovery checks pass.

### Repairing a repository identity split (ADR 012)

History rewrites can leave one origin attached to historical roots and several
repository ids for the current root. Upgrading registration prevents further
unambiguous splits; it does not combine existing repositories. Only an operator
holding the hub's owner token can merge them. `iris admin forget` deletes mail and
history and must never be used as a substitute.

1. Identify the source and destination ids from the console's overview or the
   authenticated `GET /v1/ui/overview` response. Compare origin aliases, roots,
   hosts, agents and mail. The destination keeps its id, root, name and policies.
   Choose it deliberately; a common root alone also describes forks. The merge
   accepts explicitly selected differing roots, because that decision belongs to
   the operator. Merge one source at a time.

2. Take a new database backup using `iris admin backup` as above, and separately
   secure the owner token. Run `iris admin restore-check` on the backup with a new
   empty scratch directory. Require a passing check before proceeding. Keep this
   backup unchanged for rollback; rehearse using another copy.

3. In a private scratch directory, copy the backup to `rehearsal.db` and start the
   new binary on an unused loopback port, with a new scratch token:

   ```sh
   umask 077
   cp /path/to/backup.db /path/to/private-scratch/rehearsal.db
   IRIS_HUB_URL=http://127.0.0.1:17878 IRIS_TOKEN_FILE=/path/to/private-scratch/hub.token \
     iris serve --listen 127.0.0.1:17878 --db /path/to/private-scratch/rehearsal.db \
       --token-file /path/to/private-scratch/hub.token
   ```

   Keep this process in its own terminal. In another terminal, explicitly select
   that scratch hub and token, then plan and rehearse the merge:

   ```sh
   export IRIS_HUB_URL=http://127.0.0.1:17878
   export IRIS_TOKEN_FILE=/path/to/private-scratch/hub.token
   iris admin merge --from <source-id> --into <destination-id> --dry-run
   iris admin merge --from <source-id> --into <destination-id>
   ```

   Inspect the reported moved row counts, source and destination identities, alias
   spellings and coalescing, contact and mailbox slot changes. Verify mail and bindings in
   the copy after applying. The hub's ordinary restart recovery and retention
   apply to this copy; its running clients cannot reattach to a scratch token.

4. Resolve any refusal before the real run. Overlapping live claims between
   unrelated Executions refuse the entire operation, including case-folded and
   prefix overlaps. Ask the holders to finish or release, or deliberately revoke
   the claims through the console. Ended, expired or revoked claims remain history
   and move unchanged. Different case or conflict policies also refuse. Distinct
   nonempty repository contacts refuse: explicitly clear or choose a contact
   using `iris admin contact` first. A source-only contact transfers; an existing
   destination contact remains. These choices are never made by the merge.

5. On the operator's machine, explicitly set `IRIS_HUB_URL` and `IRIS_TOKEN_FILE`
   to the intended hub and its owner token. Review a fresh dry run there, then run
   the same command without `--dry-run`. Each call uses one transaction; a refusal
   or storage failure rolls back every change. A dry run reserves nothing and
   does not expire leases. New claims or contact changes between calls can cause
   the real run to refuse safely.

6. Check that every host's peers and claims share the destination repository,
   bindings retain their Execution ids and generations, and historical mail is
   still accessible. The source id ceases to exist; update any saved repository
   ids. Existing Execution handles continue to work, and peer and claim reads with
   those handles use the current repository even when a client sends its cached
   source id. Updated clients refresh cached binding data through the existing
   bind route and consume heartbeat identity; `whoami` reports that response.
   Agent names and mailbox ids
   never change, even if two mailboxes have the same host/client/worktree slot:
   the source receives the first free slot and its mail stays separate. No mail
   is combined or acknowledged by the merge. Retried old operations still return
   their saved results, which can contain the retired repository id. Operator
   retries search the preserved operator mailbox namespaces before choosing the
   sender for new mail, so a lost response cannot cause a second send merely
   because the recipient moved. Exact saved retries explicitly naming a retired
   source id also work; fresh sends from that id are refused. Colliding keys with
   a unique exact request return that request's saved result. A changed request
   with no exact match, or multiple exact origins, gets `key_reused`; inspect
   history before deliberately choosing a new key.

Old MCP clients already send an Execution handle with peer and claim reads, so the
updated hub fixes their scope immediately. Their `whoami` still prints cached
identity because that client discards heartbeat data: upgrade or restart the MCP
process to refresh it. Updated clients also work with older bind responses and
lease-only heartbeat replies, but an older hub retains its server-side bugs.
Repository-only API requests have no Execution context and keep their explicitly
selected scope; update any retired ids in those integrations.

Rollback requires stopping the hub and restoring the complete pre-merge backup
using the restore procedure above. It also rolls back any later mail and other
writes, so stop clients and preserve a fresh backup before choosing that recovery.
There is no automatic inverse merge.
Do not roll back only the hub binary while keeping the merged database: older
code can lose operator retry continuity and select a nonexistent human mailbox.

Root discovery remains bounded to 128 loose first-parent commits. Packed or longer
histories report an unknown root; an unknown host cannot select among contradictory
remote histories. The operator merge can restore host-local continuity but cannot
infer the right history for a new host that supplies no root. Active registration
with a changed known root is refused rather than moving its claims implicitly;
repair the identities or end the session and register afresh after reviewing it.
Hooks still fail open on coordination failure, and neither hooks nor the guard
can enforce claims on edits they do not mediate.

Use `deploy/apache-iris.conf` as the complete HTTP and HTTPS vhosts, after enabling
ssl, headers, rewrite, proxy and proxy_http on Apache 2.4.47 or later. Provision the
certificate at /etc/letsencrypt/live/iris.klatt.ie/ first, with the ACME webroot at
/var/www/letsencrypt. The ACME path is served locally on both ports; all other HTTP
traffic redirects to the fixed HTTPS hostname. Keep a copy of the previous vhost.
Run `apachectl configtest` before reloading Apache. Adjust log directories for the
host distribution. Neither building this change nor its tests edits Apache.

Header choices are deliberate:

- `ProxyPreserveHost On` passes the public authority to Iris so its Host check and
  login-origin check see the intended hostname. `ProxyRequests Off` prevents this
  vhost from becoming a forward proxy.
- `ProxyAddHeaders Off` stops Apache from constructing a second forwarding chain.
  The `RequestHeader unset` lines discard client claims about source IP (`For`),
  authority (`Host`, `Server`, `Port`), transport (`Proto`, `Protocol`, `Ssl`) and
  rewritten paths (`Prefix`, `Uri`, `Path`); `Forwarded` removes the standard combined
  form. Header names are case-insensitive and duplicate values are removed too.
  Iris ignores all other forwarding fields. Do not introduce another forwarding
  consumer without extending this explicit removal list: mod_headers unsets literal
  header names, not wildcard names.
- `RequestHeader set X-Forwarded-Proto "https"` runs after removal and supplies
  Apache's own transport assertion. Only `--public-url` makes Iris trust that field,
  and then only from a loopback peer for the exact public Host. A matching header
  without that flag grants nothing. Do not expose the HTTP listener remotely.
- `Strict-Transport-Security` asks browsers to remember HTTPS for a year. It is sent
  even on errors, without imposing policy on other subdomains or requesting preload.
- `X-Content-Type-Options: nosniff` prevents MIME sniffing. Clearing the ordinary
  response table before setting the always table avoids duplicate values from Iris.
  Iris also sets its own CSP and no-store headers; preserve those through Cloudflare.

The two watch ProxyPass rules use upgrade=websocket and a 3600-second backend timeout
before the generic HTTP rule. Their distinct backend paths avoid worker sharing with
the catch-all. Iris sends protocol pings every 25 seconds, below the approximately
100-second Cloudflare idle cutoff, and closes a socket without a pong in 10 seconds.
A proxy or network restart can still disconnect clients; both CLI and console reconnect.
The custom access-log format omits query strings. Login links are retired;
credentials and request bodies must remain absent from Cloudflare and global logs too.

Apache directive details: [proxying and upgrades](https://httpd.apache.org/docs/2.4/mod/mod_proxy.html)
and [request/response headers](https://httpd.apache.org/docs/2.4/mod/mod_headers.html).

On each Linux, WSL or Windows client, run:

```sh
iris setup --hub https://iris.klatt.ie --clients claude
```

Omit --clients to integrate all detected clients, or give a comma-separated list.
Setup shows the exact configuration and token paths, saves client.json, skips the
local service for this remote HTTPS URL, and reports missing credentials honestly.
Place the privately obtained hub token at the displayed path with user-only access
(0600 on Unix or an owner-only DACL on Windows). Then use `iris doctor` to check
connectivity, authentication and the source of the URL. `iris ui` opens the ordinary
named sign-in page without reading that machine credential. After explicit owner
activation, sign in with a named account; the public session cookie is Secure,
HttpOnly and SameSite=Strict.

Every client command resolves environment, then client.json, then defaults. Discovery
looks beside IRIS_TOKEN_FILE if it is set, otherwise beside the platform's default
Iris hub.token (os.UserConfigDir/iris). The file contains hub_url and token_file;
relative token_file paths are relative to client.json. An environment override wins
per field. GUI and IDE sessions read the saved file without shell-profile exports.
Malformed files fail explicitly; hooks still follow their fail-open contract.

After cutover, check unauthenticated /v1/status returns only {"status":"ok"}, doctor
authenticates, UI login sets the secure cookie, and an idle monitor remains connected
for several minutes. This repository's tests cover the hub and its clients; they do
not prove the deployed Apache, certificates or Cloudflare settings are correct.

Rollback must preserve named authentication or first close browser/login/UI API
routes at the proxy, as in [the activation rollback runbook](console-activation.md#rollback-while-preserving-the-browser-boundary).
Do not restore a previous vhost that reopens browser access to an old backend.
The owner must run `apachectl configtest` and reload Apache (for example
`systemctl reload apache2`) before that backend starts. Keep the Iris volume;
review state and backups before re-enabling the console.

The hub logs every failed authentication, and the first use of each token since it started,
by name and peer, never the secret. `serve --access-log` adds one line, about 120 bytes, for every
authenticated request. It is off by default, because at that rate a busy hub fills its log in days.

Named console accounts have repository authorization; machine processes still
share their host's bearer authority. Failed-auth logs may be flooded; use edge
limits and bounded log retention. Backups, credential
rotation, origin firewall policy and Cloudflare account security remain operator tasks.

## Replacing an existing vhost

Do not replace a working vhost with `deploy/apache-iris.conf`. That file is a reference for a new
host. An existing vhost carries certbot's TLS options and its own ACME webroot, and swapping those
can break certificate renewal without any immediate symptom. Change only the proxy lines, keeping a
copy of the file for rollback:

```apache
ProxyPass "/v1/ui/watch" "http://127.0.0.1:7878/v1/ui/watch" upgrade=websocket timeout=3600 retry=0
ProxyPass "/v1/watch" "http://127.0.0.1:7878/v1/watch" upgrade=websocket timeout=3600 retry=0
ProxyPass "/" "http://127.0.0.1:7878/" timeout=60 retry=0
ProxyPassReverse "/" "http://127.0.0.1:7878/"
```

The WebSocket lines must come first, because Apache uses the first match. Check that the vhost also
removes client-supplied `X-Forwarded-*` and `Forwarded` headers and then sets `X-Forwarded-Proto https`.

## Moving a host to its own token

Every host starts on the hub's own token, the owner token in `hub.token`. A host token gives the same
access to the API, under its own name, and is refused on `/v1/tokens` and `/v1/admin/forget`. This was
rehearsed on 2026-11-07 against a local hub, with a Claude Code session running throughout. The
measured result is given after each step.

Do one host at a time. The examples never show a token: write it straight to a file, and never paste it
into a chat, a ticket or an Iris message.

1. On a machine that holds the owner token, create the host's token and keep it out of the terminal:

   ```bash
   umask 077
   iris token add <host> > <host>.token
   ```

   `iris token add` prints the token once, and the hub keeps only its digest. Adding a name that is
   already active is refused with `conflict`. To replace a lost token, revoke the name first, with the
   cost described under "Revoking a host token" below.

2. Move `<host>.token` to the host by hand, for example with `scp`, and delete the copy you made.

3. On the host, replace the contents of its token file, and keep the file's path. The file is the one
   `iris setup` printed: `hub.token` in the Iris configuration directory, unless `client.json` or
   `IRIS_TOKEN_FILE` names another.

   ```bash
   CONFIG="$HOME/Library/Application Support/iris"   # Linux: ~/.config/iris
   install -m 600 <host>.token "$CONFIG/hub.token.new"
   rm <host>.token
   IRIS_TOKEN_FILE="$CONFIG/hub.token.new" iris doctor              # token: ok
   IRIS_TOKEN_FILE="$CONFIG/hub.token.new" iris token list > /dev/null # must be refused: forbidden
   mv "$CONFIG/hub.token.new" "$CONFIG/hub.token"
   ```

   Check the new token before it replaces the old one. If `iris doctor` shows `token: ok` and `token
   list` is refused as `forbidden`, the file holds a working host token and not the owner's. Measured
   on the first real switch, of `laptop` on 2026-11-07. Until the `mv`, the old file still works, so a
   failed check costs nothing.

   Iris reads the file on every request, so nothing needs a restart. Measured: from the next request,
   the running session's hooks, its `iris mcp` and its heartbeats all used the host token. The session
   saw no error, and the edits it made in the same minute were claimed. Changing `token_file` in
   `client.json`, rather than the file's contents, needs every running `iris mcp` and `iris watch` to be
   restarted, because they read `client.json` once.

4. Check it:

   ```bash
   iris doctor              # hub: ok, token: ok
   iris token list          # on the owner's machine: <host>  active
   ```

   Measured: `iris doctor` reported `token: ok`. From the host, `iris token list` and `iris admin
   forget` were refused with `forbidden: only the hub's own token may do this`.

5. Remove every other copy of the owner token from the host, such as backups and old files from
   `scp`. The owner token cannot be revoked for one host: it is the hub's own credential. The host
   stops holding it once its file holds the host token. To invalidate a copy that may have leaked, the
   hub's `hub.token` must be replaced. That ends access for every host still on it, so do it only after
   all hosts have moved.

### Revoking a host token

`iris token revoke <host>` takes effect at once. Measured with a Claude Code session still running:

- `iris doctor` on the host reported `token: not valid (token was rejected)` and exited 1.
- Hooks failed open, as D3 requires: the edits were allowed but not claimed, and recorded in
  `hook-failures.log`. Each one also told the model: "Iris: the hub rejected this host's token, so
  this session's edits are allowed but not claimed, and other agents cannot see them." The model
  quoted it and asked for `iris doctor`.
- `whoami` failed with `unauthorized`, because it proves its binding to the hub instead of answering
  from cache.
- `iris watch` printed one line, "Iris: the hub rejected this host's token, so notifications have
  stopped.", and printed nothing more while it retried.
- The hub logged `reason="revoked token \"<host>\""`. An unknown token logs `unknown bearer token`,
  and a missing one `bearer token required`.

Edits still go through unclaimed until the host has a working token. So revoke a host's token only
after its sessions have ended, or after its file holds a replacement.
