# Public Iris hub

The intended path is client HTTPS → Cloudflare HTTPS → Apache → HTTP on
127.0.0.1:7878. Use Cloudflare Full (strict), a valid origin certificate, and
WebSocket support. Disable caching and script rewriting/injection for this hostname;
Iris sends no-store and a CSP that permits only its own scripts. Cloudflare terminates
TLS and can see credentials and message content. This is a shared-trust service for
one developer or a small team, not a multi-tenant boundary (D13).

The files in `deploy/` are written for this topology. No release binary is needed:
`deploy/Dockerfile` builds the locked Node console and embeds it in a CGO_ENABLED=0
Go binary. Its final scratch image runs as uid/gid 65532 and contains the binary,
CA certificates and a writable /data. `deploy/compose.yaml` uses Linux host networking
so the loopback listener is reachable by the host's Apache; no port is published.
The named `iris-data` volume holds the database, WAL and token on local Linux storage.
Do not put it on WSL drvfs, NFS or another network filesystem. Keep volume backups
and the token private. A bind-mounted replacement must be writable by uid 65532.

For an operator performing deployment, from the repository root:

```sh
docker compose -f deploy/compose.yaml build
docker compose -f deploy/compose.yaml up -d
```

These commands start the sole container named `iris`, with restart unless-stopped.
The hub creates /data/hub.token once. Retrieve it through an administrator-controlled
channel and distribute it privately; do not paste it into chat, URLs or shell history.
Setup cannot fetch or provision this credential. Back up the database using SQLite's
backup mechanism or while the hub is stopped; copying only a live .db can omit WAL data.

### Retention and restore (ADR 006)

The hub sweeps at start and then hourly. It drops events after 7 days, idempotency records after a
day, claims finished 30 days ago, and Executions that ended 30 days ago and that no mail names. Mail
is never deleted, so the database grows with it; `GET /v1/status` and `iris doctor` show its size.
The first sweep after an upgrade logs what it removed, which can be a lot once.

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
non-zero. To restore for real, stop the hub, put the backup in place of `hub.db` (removing its
`-wal` and `-shm` files), and start it: every session then reattaches with proof, or registers
afresh.

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
The custom access-log format omits query strings so one-time login codes are not
recorded there. Check Cloudflare and any global logging policy for the same exposure.

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
connectivity, authentication and the source of the URL, and `iris ui` for a login link.
The link expires after one minute and works once; its public session cookie is Secure,
HttpOnly and SameSite=Strict. Do not share the link.

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

Rollback: stop the new container if port ownership needs to return to the previous
service, restore the previous vhost file, run `apachectl configtest`, and reload Apache
(for example `systemctl reload apache2`). Restore the previous service using its own
operational procedure. Keep the Iris volume; do not delete credentials or database as
part of rollback. Before later re-enabling Iris, inspect its state and backup.

The hub logs every failed authentication, and the first use of each token since it started,
by name and peer, never the secret. `serve --access-log` adds one line, about 120 bytes, for every
authenticated request. It is off by default, because at that rate a busy hub fills its log in days.

Remaining operational limits: no per-user authorization, per-IP rate limit, global
session/socket quota, token revocation API or automatic database retention. Failed-auth
logs may be flooded; use edge limits and bounded log retention. Backups, credential
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
