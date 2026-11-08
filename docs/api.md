# Hub API, version 1

The contract between `iris serve` and its clients: `iris hook`, `iris mcp`, `iris watch`,
`iris guard` and the web UI. Behaviour is defined in `docs/architecture.md`. This file fixes the
names so that independently built parts meet.

Requests and responses are JSON. Authentication is `Authorization: Bearer <token>`. Retryable message and explicit-claim creation carry a client-chosen `key` (D9). Touch uses
`tool_call_id`; lifecycle calls are idempotent by session/generation, and release and ack
are idempotent by fence and delivery attempt respectively.

Where the key comes from matters, because a key the caller first learns from a response is lost
with that response. `iris mcp` uses the `key` a model passes. Without one, a send derives its key from
the Execution and generation, the operation, the arguments exactly as sent and a ten-minute
window, so a retry of the same call after an ambiguous outcome is deduplicated, while an
identical message sent on purpose in a later window goes through. A retry that crosses a window
boundary gets a new key. To send identical content twice within the window, pass a key. The web console makes one key when a draft starts, keeps it until the
send succeeds and makes a new one when the draft is edited, and it sends one draft at a time.
A claim without a key gets a fresh one, because the same claim after a release within the window
must be a new claim, not the released one replayed. Every mutation's response and error carry
the key used (gap review A9). Errors look like this:

```json
{"error":{"code":"conflict","message":"human readable","details":{}}}
```

Codes: `unauthorized`, `bad_request`, `not_found`, `conflict`, `key_reused`, `unbound`,
`ambiguous`, `lost`, `too_large`, `rate_limited`.

## Sessions

`POST /v1/sessions` registers or reattaches. It is called by the `SessionStart` hook, or by
`iris mcp` when no hook ran.

```json
{"client":"claude","session_id":"...","host":"...","cwd":"...","worktree":"...",
 "remote_url":"...","root_commit":"...","branch":"...","parent_session_id":"",
 "client_pid":123,"client_pid_start_ms":1793917887311,"single_session":true,
 "source":"hook","generation":0}
```

Response:

```json
{"execution_id":"ex_...","generation":1,"handle":"ex_...","repository_id":"rp_...",
 "agent":{"id":"ag_...","name":"claude-host-iris"},"lease_ms":900000,
 "peers":[{"name":"...","client":"...","state":"active","worktree":"...","branch":"..."}],
 "claims":[{"fence":1,"path":"...","kind":"exact","holder":"...","origin":"auto",
            "expires_at":"..."}],
 "unread":0}
```

For recovery after a hub restart, reattach with the same `session_id` and the last returned
`generation`. A recovering session without that generation returns `lost`; an expired
generation cannot be reattached. `generation:0` starts or resumes a non-recovering session.
Recovery keeps claim ownership and expiry but rotates fence numbers; use the `claims`
in the reattachment response. Old fence numbers and tokens are invalid.

A shim or `iris watch` reattaches by itself: a heartbeat with the current generation does the
same work during the recovery window and returns the same response, so an idle session that
fires no hook survives a hub restart. After the window the heartbeat returns `lost`, and the
session needs a new generation.

`POST /v1/bind` resolves a stdio MCP process to an Execution, following D6.

```json
{"client":"cursor","host":"...","execution":"","session_id":"",
 "client_pid":123,"client_pid_start_ms":1793917887311,"cwd":"..."}
```

It returns `{"execution_id":"...","generation":1,"agent":{...}}`, or `unbound`, or `ambiguous`.

`POST /v1/executions/{id}/heartbeat` takes `{"generation":1,"source":"shim"}`, where `source` is
`shim`, `watch` or `hook`. It returns `{"lease_ms":60000}`, or `lost`.

`POST /v1/executions/{id}/end` is a best-effort end of session. It takes `{"generation":1}`. With
`"hand_over":"ex_..."` it first moves the Execution's agent's unacknowledged deliveries to the agent
of that live Execution and wakes it, each moved delivery asking for attention again. A message the
destination already has, sent to both mailboxes, is settled in the stray mailbox instead, so it
arrives once. Only an Execution an MCP registered may hand over, and only to the one the client
process rung finds for it: top-level, of the same host, client, process id and start time; anything
else is refused as `bad_request`. An MCP server uses it when it gives up an Execution it had to
register for the session's hook's (D6), so mail sent to the stray mailbox is not stranded. Ending
again moves nothing.

## Claims

`POST /v1/claims/touch` is the pre-edit check-and-claim. It is one transaction (D10).

```json
{"execution_id":"ex_...","generation":1,"tool_call_id":"...",
 "paths":["src/a.go"],"rename_from":"","rename_to":""}
```

Response, always 200 when the request is valid:

```json
{"action":"allow","granted":["src/a.go"],"ignored":[],
 "conflicts":[{"path":"src/a.go","claimed_path":"SRC/A.go","holder":"codex-host-api",
               "origin":"auto","expires_at":"...","fence":7}],
 "message":"text shown to the user and the agent"}
```

`action` is `allow`, `deny`, `ask` or `warn`. Paths are repository-relative with forward slashes.
In a conflict, `path` is the path as requested and `claimed_path` is the holder's claim as written.
Claims compare by a key that ignores case and Unicode form, as `docs/adr/003-path-identity.md`
describes, and always display the path as written.

`POST /v1/claims` makes an explicit claim:

```json
{"execution_id":"ex_...","generation":1,"key":"...","kind":"prefix","path":"src/",
 "repo_wide":false,"ttl_s":0}
```

It returns `{"fence":12,"token":"...","expires_at":null}`, or 409 `conflict` with the conflicts in
`details`.

`POST /v1/claims/release` takes `{"execution_id":"...","generation":1,"fence":12}`, or `"all":true`. An optional `token` validates the complete stream/epoch fence returned
by explicit claim creation. Generation and ownership are always checked.

`GET /v1/claims?repository_id=...` lists live claims.

## Messages

`POST /v1/messages`:

```json
{"execution_id":"ex_...","generation":1,"key":"...","to":["codex-host-api"],
 "subject":"...","body":"at most 16 KiB"}
```

It returns `{"message_id":"ms_...","deliveries":[{"id":"dl_...","recipient":"..."}]}`.

`GET /v1/inbox?execution_id=...&limit=20` assigns deliveries to the caller and returns them. Each
fetch increases `attempt`. With `receipt=1` the deliveries keep needing attention until
`POST /v1/inbox/receipt` with `{"execution_id":"...","generation":1,"receipts":[{"id":"dl_...","attempt":2}]}`
says the response arrived; one whose receipt has not come within 60 s wakes its recipient once. A
receipt is not an acknowledgement, and one for an older attempt or another Execution changes
nothing (ADR 010). Without `receipt=1` a fetch quiets its mail at once, as before.

```json
{"deliveries":[{"id":"dl_...","attempt":1,"sender":"...","subject":"...","body":"...",
                "created_at":"..."}],"more":false}
```

A page holds at most `limit` deliveries (20 by default, 100 at most) and at most 512 KiB of JSON,
so it always fits the 1 MiB a client reads. It always carries at least one waiting delivery. `more`
is `true` when deliveries are waiting beyond it. Deliveries stay with the caller until acknowledged,
so a fetch before acknowledging returns the same page again: acknowledge it, then fetch the next.
A client refuses a response over 1 MiB as too large instead of reading part of it.

`POST /v1/deliveries/{id}/ack` takes `{"execution_id":"...","generation":1,"attempt":1}`. A repeated
ack succeeds, and a stale one returns `conflict`. Ack means accepted, not acted upon.

An ack may also carry `"handled":true`, which says the recipient has acted on the message (ADR 005).
"Handled" is the recipient's word, not a verified fact: Iris records that it was said, and when,
and cannot check the work. With it, one ack both accepts and marks handled, under the rules above.
On a delivery already accepted, any live Execution of the recipient agent may mark it handled,
with any or no `attempt`, because the work often ends in a later session. Another agent gets
`not_found`. A repeat keeps the first time, and nothing takes handled back.

`GET /v1/deliveries/{id}/body?execution_id=...&after=<offset>&limit=<bytes>` returns part of one
delivery's body, at most `limit` bytes as encoded in JSON (32 KiB at most and by default), cut on a
character boundary:
`{"delivery_id":"dl_...","sender":"...","body":"...","body_bytes":14400,"offset":0,"next_offset":4000}`.
`next_offset` is present while more remains. Only the recipient may read it, and reading changes no
attempt and settles nothing: only an ack does (ADR 008). The MCP `inbox` gives a call's bodies a
budget of 32 KiB together, counted as encoded in JSON, which is what a client's limit applies to. A body past it carries `body_truncated: true` beside `body_bytes`, a
`read_offset`, and a `body_note` of Iris's own saying the body is not the whole message, and the
`read` tool returns the rest.

`GET /v1/messages/{id}/status?execution_id=...` shows the sender what became of its message:

```json
{"message_id":"ms_...","recipients":[{"recipient":"codex-host-api","repository_id":"rp_...",
                                      "state":"handled","at":"..."}]}
```

`state` is `pending`, `inflight` (fetched by a live Execution, not acked), `acked` or `handled`.
`at` is when it was acked or said handled, and null otherwise. There is no subject or body. Any
Execution of the sending agent may ask; for anyone else the message does not exist, and the answer
is `not_found`. During a hub restart a recovering recipient's mail stays `inflight` for up to 120
seconds, then turns `pending` if that session is lost.

## Agent message history

`GET /v1/messages/history?execution_id=...&q=...&before=...&limit=20` lists
messages sent or received by the calling Execution's **agent**, across its sessions
and repositories. `q` searches subject and body as literal whitespace-separated
terms; all terms are required, with no FTS operators or column filters. Whitespace
alone means no filter. FTS5 still tokenizes text, so punctuation is not an exact
substring constraint. A search SQLite cannot parse returns `bad_request`.
Only matching messages are returned, never other messages from a matching thread.
`GET /v1/threads/{id}?execution_id=...&before=...&limit=20` lists that same
agent-scoped view of one thread. Unknown and invisible threads both return `not_found`.
Repository or agent query parameters do not broaden either view.

Both return:

```json
{"messages":[{"message_id":"ms_...","thread_id":"ms_...","sender":"...",
 "subject":"...","body":"...","created_at":"...","sent":true,
 "from_repository_id":"rp_...","from_repository":"...","cross_repository":false}],
 "next":"<opaque cursor>"}
```

Messages are ordered by creation time descending, then message id descending.
`limit` defaults to 20, at most 100, and HTTP pages fit within 512 KiB.
Pass a nonempty `next` as `before` with the same search and thread scope; an empty
`next` ends the list. A malformed cursor or changed scope returns `bad_request`.
The cursor survives removal of its anchor message. Pages are separate snapshots:
new sends appear on a fresh first page, and forgotten messages disappear.

MCP `history` accepts `q`, `before`, `limit`; `thread` accepts `thread_id`, `before`,
`limit`. Their bodies share the inbox's 32 KiB JSON-encoded budget and wrap subject
and body in `untrusted_data`. A cut body has the same `body_truncated`, `body_bytes`,
`read_offset` and `body_note` markers as inbox. Call `read` with its `message_id` and
`offset` equal to `read_offset` for the rest. `read` accepts exactly one of
`message_id` and `delivery_id`.

`GET /v1/messages/{id}/body?execution_id=...&after=0&limit=32768` reads a body the
caller sent or received, using the same byte offsets and encoded budget as the
delivery body route. Its response has `message_id`, `sender`, `body`, `body_bytes`,
`offset`, and `next_offset` while more remains. An invisible or missing message is
`not_found`; an offset outside the body is `bad_request`. Offsets inside a UTF-8
character move back to its start.

History, thread and body reads change no delivery attempt, assignment, attention,
ack or receipt. They cannot prove that a model read a message, reveal unseen replies,
restore forgotten messages or freeze a changing conversation (ADR 011).

## Peers and status

`GET /v1/agents?repository_id=...` lists agents and their current state.

`GET /v1/hooks/seen?host=...` says, for each client on that host, when its hooks last reached the
hub: `{"clients":{"codex":"2026-11-07T21:00:00Z"}}`. A hook counts when it registers a session,
claims a path automatically or reconciles a shell command. A heartbeat from the MCP shim or the
monitor does not, so this is the last hook, not the last sign of life. A client whose hooks never
reached the hub is absent. `iris doctor` puts it in each row's `last_hook_at`.

`GET /v1/status` returns only `{"status":"ok"}` without a valid bearer token. With a valid
bearer token it returns version, stream id, boot epoch and counts.

## Events

`GET /v1/events?execution_id=...&after=123&limit=100` replays events in order:

```json
{"stream":"<uuid>","events":[{"cursor":124,"kind":"inbox_changed"}],"high_water":124}
```

If `after` is older than the log keeps, it returns `{"reset":true,"high_water":200,...}`, and the
client must take a snapshot. The response includes `snapshot` with live `claims` and
`unread` count captured in the same transaction as `high_water`. Optional `stream` detects
a changed stream identity. Optional `consumer_id` persists a monotonic cursor; lower
`after` values never move that stored cursor backwards. These parameters also apply to
watch; a reset frame instructs the consumer to fetch the events snapshot.

`GET /v1/watch?execution_id=...&after=123` upgrades to a WebSocket. Authentication is the bearer
token with subprotocol `iris.v1`, or subprotocols `iris.wake` and the ticket. Each text frame is:

```json
{"stream":"<uuid>","cursor":124,"kind":"inbox_changed"}
```

Frames never carry message text. Kinds so far: `inbox_changed`, `claim_released`, `reset`,
`inbox_read`, which moves the cursor past mail already fetched and needs no attention, and
`claim_available`. That last one is sent only to an Execution that was refused a path, once the
path is free (D7). `claim_released` goes to every agent in the repository and is not a wake.

A monitor (`consumer_id=iris-watch`) with no saved cursor starts at the newest event, because a new
session is a new Execution and must not hear the agent's history again. If undelivered mail is
waiting it gets one `reset`. `pending` in the `/v1/events` response counts mail nobody has fetched;
it does not count mail that was fetched and not yet acknowledged.

`POST /v1/wake-tickets` takes `{"execution_id":"...","generation":1}`, and returns
`{"url":"ws://...","protocols":["iris.wake","<ticket>"],"expires_at":"..."}`.

## MCP tools

`iris mcp` exposes these. Each accepts an optional `execution` handle (D6).

| Tool | Does |
|---|---|
| `whoami` | Own agent, Execution, repository, binding rung |
| `peers` | Who else is working here |
| `send` | Send a message |
| `inbox` | Fetch messages |
| `ack` | Acknowledge a delivery; with `handled`, say it was acted on |
| `receipts` | Show the recipients' states of a message you sent |
| `read` | Read a sent or received body by `message_id`, or a received body by `delivery_id`, from a byte offset |
| `history` | List/search your sent and received messages, newest first, with an opaque cursor |
| `thread` | Page through only the messages you sent or received in one thread |
| `claim` | Make an explicit claim |
| `release` | Release a claim, or all |
| `claims` | List live claims |
| `watch_ticket` | Get a wake ticket for the `Monitor` tool |

## Threads

`POST /v1/messages` also takes an optional `reply_to` with a message id. A reply joins that
message's thread, and every message returns its `thread_id`. The recipient `human` is reserved: it
reaches the operator through the web UI, not through a session.

## Operator API, for the web UI

Agents never use these routes. They accept the bearer token, or the session cookie of the web UI.

`iris ui` calls `POST /v1/ui/links` with the bearer token, gets
`{"url":"http://127.0.0.1:7878/ui/login?code=..."}` and opens it. `GET /ui/login?code=...` works
once, sets an `HttpOnly`, `SameSite=Strict` cookie and redirects to `/`. Cookie login is refused
off loopback unless `serve --public-url https://host` explicitly enables a trusted loopback
reverse proxy. In that mode login and cookie authentication require the exact public Host and
`X-Forwarded-Proto: https`, and the cookie is also `Secure`. The proxy must strip incoming
forwarding headers before setting its own. Links expire after one minute and work once;
creating a link always requires the bearer token. Everything else under `/` serves the embedded web UI.

`GET /v1/ui/overview` returns everything the first screen needs:

```json
{"repositories":[{"id":"rp_...","name":"iris","aliases":["..."]}],
 "agents":[{"id":"ag_...","name":"...","client":"claude","host":"...","repository_id":"rp_...",
            "worktree":"...","branch":"main","state":"active","last_seen":"...","unread":0}],
 "claims":[{"fence":1,"repository_id":"rp_...","path":"...","kind":"exact","holder":"...",
            "origin":"auto","expires_at":"..."}],
 "human_unread":0}
```

`GET /v1/ui/threads?repository_id=...&agent=...&q=...&before=...&limit=50` lists threads, newest
first, ordered by the last message visible in the view and then by thread id. `q` is a full-text
search with the same literal terms as agent history: all whitespace-separated
terms are required; whitespace alone means no filter. FTS5 tokenization still
applies. A search SQLite cannot parse returns `bad_request`.
`limit` is 50 by default and 100 at most.

```json
{"threads":[{"id":"ms_...","subject":"...","participants":["..."],"count":3,
             "last_at":"...","unread":1}],"next":"ms_..."}
```

`next` is present when more threads follow. Pass it as `before` with the same filters for the next
page. It continues after that thread in the same order, so threads whose last messages share a
millisecond are neither skipped nor repeated. If the cursor's thread gets new mail between two
requests, it moves to the top, and the next page lists again the threads that were between it and
the top. Mail can repeat a thread across pages in this way, but it never leaves a gap. A `before`
that names no thread is read as a millisecond timestamp and returns the threads whose last message
is older.

`GET /v1/ui/threads/{id}` returns the thread's messages:

```json
{"id":"ms_...","messages":[{"id":"ms_...","sender":"...","recipients":[{"name":"...",
  "state":"acked"}],"subject":"...","body":"...","created_at":"..."}]}
```

`POST /v1/ui/threads/{id}/read` takes `{"repository_id":"..."}` and acknowledges the human
deliveries in that thread: that repository's, or every repository's when `repository_id` is
empty. It returns `{"read": n}`. The console calls it when the operator opens a thread, so the
human inbox's unread count goes down. There is no way to mark mail unread again.

`POST /v1/ui/messages` takes `{"key":"...","to":["..."],"subject":"...","body":"...","reply_to":""}`
and sends as `human`.

`POST /v1/ui/claims/{fence}/revoke` revokes any claim.

`GET /v1/ui/doctor` returns each host's last report, which `iris doctor` stored with `PUT /v1/ui/doctor`
(ADR 007):

```json
{"hosts":[{"host":"mac","checked_at":"...","clients":[{"client":"codex","version":"...",
  "status":"needs_human","detail":"Review hooks once in /hooks","last_hook_at":"..."}]}]}
```

A `PUT` carries `host`, the machine's name as its hooks give it, and replaces that host's report
only. One from a doctor too old to send `host` is kept under `unknown`, so it never replaces a real
host's. A host that never reported is absent. The console greys a report checked more than 24 hours
ago.

`status` is `ok`, `not_configured`, `needs_human`, `silent` or `slow`. `slow` means the hooks run,
but shell reconciliation passed its budget more than ten times in 24 hours, so those commands'
changes went unclaimed. A doctor sends it as `silent`, with `slow: ` in the detail, to a hub that
refuses it. `last_hook_at` is when that client's hooks on the doctor's host last reached the hub,
and empty when they never did or the hub cannot say. Before the first report, `hosts` is empty. `expires_at` on a claim is null when it has no expiry.

`GET /v1/ui/watch` upgrades to a WebSocket, with frames `{"cursor":124,"kind":"changed"}`. The UI
refetches on a frame. Frames carry no content.

## Cooperative Git and shell checks

`POST /v1/guard/check` accepts `host`, `worktree`, `common_dir`, `remote_url`,
`root_commit` and repository-relative `paths`. It creates no Execution and takes no
claims. The response contains `message` and `conflicts`; a nonempty message means a
live claim overlaps a path in another host/worktree. Repository case policy and
prefix boundaries apply. Same-worktree claims are excluded. Git's staged rename
source and destination are both checked by the CLI.

`POST /v1/shell/reconcile` accepts `execution_id`, `generation`, `kind` (`before` or
`after`), and `status`, a map of path to porcelain XY status. The hub stores a
snapshot per Execution. `before` establishes the baseline; `after` compares and
replaces it, returning the same response shape. The first post without a baseline
only records it. Multiple live Executions in the same host/worktree suppress
warnings and clear their baselines. Status strings are observations, never commands.

Instead of `execution_id`, a reconcile may carry the session's registration fields, those of
`POST /v1/sessions` with `session_id` and the remembered `generation`. The hub then registers
the session exactly as `POST /v1/sessions` would, recovery proof included, and reconciles for
the Execution it gives, in one transaction. A shell hook therefore makes one round trip instead
of two. The response adds `"session":{"execution_id":"ex_...","generation":4}`, whose generation
the hook remembers as its next proof. A hub from before this form answers it with an error,
and the hook then registers and reconciles in two calls.

Session registration also accepts `common_dir`, a host-scoped alias for linked
worktrees without a globally unambiguous remote. It never joins paths across hosts.

## Public service limits

Every route accepts at most 64 KiB of request body, including chunked bodies and routes
that do not consume JSON. Oversized requests return 413 (`too_large`). An Execution
may hold at most 500 live claims; a grant exceeding that limit fails atomically with
`too_large`, while touching existing claims is allowed. The HTTP server has a 10-second
read timeout, a 5-second header timeout and a 60-second idle timeout; upgraded sockets
are independent of these deadlines.

`GET /v1/status` without a valid bearer token returns only `{"status":"ok"}`. With a
valid bearer token it returns the full status document described above. A UI cookie
does not grant access to detailed status. Authentication failures are logged without
credentials. All `/v1/` and `/ui/login` responses carry `Cache-Control: no-store`.
The console has a restrictive Content-Security-Policy: scripts, styles and fonts are
same-origin, images are same-origin or data, connections are same-origin HTTP and the
hub WebSocket origin, and framing, forms and base URLs are disabled.

Both `/v1/watch` and `/v1/ui/watch` send WebSocket protocol pings every 25 seconds.
A missing pong within 10 seconds closes the socket. These are control frames, never
JSON events: `iris watch` responds while reading and browser WebSocket implementations
handle them without dispatching `message` events. Public wake-ticket URLs use `wss`
and the configured public authority.

## Named host tokens

The hub's own `hub.token` is the owner token. It remains valid, and logs name it
`(legacy)`; no client change is needed on upgrade. `iris token add <host>`,
`iris token list`, and `iris token revoke <host>` need the owner token: a host token
gets `forbidden` (403) on every `/v1/tokens` route, because a host that could manage
tokens could mint itself more or lock other hosts out. Otherwise host tokens have the
owner's authority, and names give attribution, not isolation between hosts. A UI
cookie cannot administer bearer tokens.

`POST /v1/tokens` takes `{"host":"workstation"}` and returns `{"name":"workstation",
"token":"..."}`. The random 256-bit token is returned only on creation; the database
stores a SHA-256 digest. Names contain 1–64 ASCII letters, digits, dots, underscores
or hyphens, starting with a letter or digit. An active duplicate returns `conflict`.
`GET /v1/tokens` returns `tokens`, each with `name`, `created_ms`, `revoked_ms`,
`last_used_ms`, and `requests`; no secret or digest is returned.
`DELETE /v1/tokens/{name}` revokes immediately and is idempotent for an existing name.
A revoked name can be added again with a fresh secret. The legacy file token is
rotated through its file rather than this API.

## Removing records

`POST /v1/admin/forget` removes one repository or one agent, with its executions,
claims, deliveries, messages, acknowledgements, cursors, shell snapshots and events.
It takes `{"repository_id":"..."}` or `{"agent":"..."}`, exactly one, and
`"dry_run":true` to report without removing. Only the owner token may call it; a host
token gets `forbidden` and no token gets `unauthorized`. `iris admin forget
--repository <id> | --agent <name> [--dry-run]` prints the response.

It refuses with `conflict` while any of those executions is active or holds an
unrevoked claim, and says how many of each in `details`. The response names the
`scope`, gives `counts` per table, and `detached_children`, the subagent executions of
other agents that lose their parent link. Mail goes with whichever end is removed, so
another agent can lose messages from its inbox. Events for other agents that refer to a
removed delivery stay, and read as `inbox_read`, so no watcher is woken for them. A
`changed` event refreshes the web console. Nothing is archived: a removed record is
gone, and the hub keeps no tombstone.

Every authenticated HTTP request logs its token name, method, path and peer, without
credentials or request bodies. Named token usage counts and last-use timestamps are
persisted. Revocation refuses new requests and closes existing bearer streams and
wake-ticket streams issued by that token. Already authenticated HTTP operations may
finish. Previously issued UI login links and cookies remain valid until their own
expiry; bearer revocation does not revoke those separate UI sessions.

Request quotas are in-memory fixed one-minute windows. A valid bearer token is
limited only by its credential digest; anonymous traffic cannot consume that quota.
All requests without a valid bearer token, including failed authentication, share
one separate anonymous bucket. TCP peer addresses and forwarded headers never
identify anonymous clients, so a loopback reverse proxy cannot cause an anonymous
flood to lock out authenticated agents.

`serve --token-rate` defaults to 6000 requests per minute per credential;
`--address-rate` retains its flag name for compatibility but now sets the aggregate
anonymous quota (default 12000 per minute). `--token-websockets` defaults to 100
concurrent sockets per credential. Zero selects the default; negatives are rejected.
These defaults support approximately 50 sessions sharing a host. Wake-ticket
upgrades without a bearer token consume anonymous quota and their issuer's token
quota; cookie streams without a bearer token consume anonymous quota and share a
separate socket quota. HTTP quota and socket-cap exhaustion return 429 with
`error.code` `rate_limited` and integer `Retry-After` seconds. Closing or failing an
upgrade releases its socket slot. Revocation does not reset a quota for the old secret.

Quotas reset on restart, are local to one hub process, and do not impose per-Execution
permissions. The window table is capped at 10000 entries; saturation refuses new
credential keys until an old window expires. Anonymous requests cannot grow this
table beyond their single fixed entry.
