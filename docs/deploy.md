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
