# Hub API, version 1

The contract between `iris serve` and its clients: `iris hook`, `iris mcp`, `iris watch`,
`iris guard` and the web UI. Behaviour is defined in `docs/architecture.md`. This file fixes the
names so that independently built parts meet.

Requests and responses are JSON. Machine APIs use `Authorization: Bearer <token>`;
browser APIs use the named account session and CSRF contract below. Retryable
message and explicit-claim creation carry a client-chosen `key` (D9). Touch uses
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
 "capabilities":["shell_scope"],"unread":0,
 "attention":{"unread":0,"available":0,"monitored":false,"identity":null,"high_water":17}}
```

`capabilities` names what this hub accepts beyond the rest of version one. `shell_scope` means
that it takes reduced shell snapshots on `POST /v1/shell/scoped` (below). A hook sends one only
to a hub whose last registration answer named it.

`attention` is what is pending for the session. `unread` counts its mail that still needs a wake,
`available` counts the paths freed for this Execution, and `monitored` says its `iris watch` beat
recently. `identity` names the newest event still pending, `{"stream":"<uuid>","cursor":124}`, or
is `null` when nothing is. A hub that knows identities always sends the field, `null` included; a
hub from before sends none, so a client tells the two apart by the field's presence. Mail keeps
its identity after retention deletes its event. A freed path stays in the identity until
retention deletes its event, even after the path was taken. `high_water` is the event sequence's
high-water mark. A restore keeps the stream and takes the sequence back, so a cursor that a client
remembers above `high_water` means that the hub went back (ADR 006, decision 7). A client that
first checks after `high_water` has reached its remembered cursor cannot tell. Responses to
touch and shell reconcile requests, and to events reads with `attention=1`, carry the same block
(ADR 017).

For recovery after a hub restart, reattach with the same `session_id` and the last returned
`generation`. A recovering session without that generation returns `lost`; an expired
generation cannot be reattached. `generation:0` starts or resumes a non-recovering session.
Recovery keeps claim ownership and expiry but rotates fence numbers; use the `claims`
in the reattachment response. Old fence numbers and tokens are invalid.

An optional `retry_generation` reports the proof a hook discarded before retrying with
`generation:0`. It affects only the hub's generation-replacement diagnostic, grants no authority,
and is omitted by older clients. Resume and reattachment preserve the generation; a new
Execution after loss or end advances it.

A shim or `iris watch` that retains the binding reattaches by heartbeat: a heartbeat with the
current generation does the same work during the recovery window and returns the same response.
This can keep an idle session attached without a hook. A shim binds on its first tool call;
without a binding it has no generation to send. A heartbeat that confirms loss drops that
binding and never renews that Execution again. Codex id-mode sub-agent and foreign-session
lookups keep no binding and renew nothing. After the window the heartbeat returns `lost`, and
the session needs a new generation.

`POST /v1/bind` resolves a stdio MCP process to an Execution, following D6.

```json
{"client":"cursor","host":"...","execution":"","session_id":"",
 "client_pid":123,"client_pid_start_ms":1793917887311,"cwd":"..."}
```

It returns `{"execution_id":"...","generation":1,"agent":{...},"session_id":"...","hook_seen":true}`,
`unbound`, `ambiguous`, or `lost` for an explicit Execution that is unusable. The returned
`session_id` is the native session registered by the hook; an older hub may omit it, in which case
Kimi's native wake stays disarmed. Binding only looks up a usable Execution; it does not reattach
one. A lookup by session id excludes recovering Executions, so Codex id mode without a cached
binding answers `unbound` during recovery. `hook_seen` says whether any hook has reached the hub
for that Execution. Codex and Kimi wake require it to be true.

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
again moves nothing. A client that loses the response retries the same source, generation and
destination until the end is confirmed, renewing live Executions while the handover is pending.

### MCP tools answering `lost`

Every MCP tool's `lost` error has the same fixed recovery guidance, whether binding failed or
the hub refused the operation. The MCP shim preserves the error code and details, including
mutation retry keys; the HTTP API's messages and recovery rules are unchanged. `lost` alone
does not prove permanent session loss: it can describe a recovering Execution, an unknown,
ended or expired Execution, a wrong generation, a failed bind after confirmed loss, or a
replayed claim whose saved fence is no longer live.

In the Git worktree of the session owning the Execution, reattachment requires the hub to
accept the remembered generation before the persisted 120 s recovery deadline:

| Event | Recovery effect |
|---|---|
| An enabled `SessionStart` hook, including a resume of the same session | Registers with remembered proof. |
| A prompt on Claude Code, Codex, Copilot or Kimi; Antigravity's `PreInvocation` | Registers with remembered proof. Cursor and Grok prompts do not register. |
| A recognized pre-edit hook with paths inside the worktree | Registers before touching the paths. The edit is refused if recovery proof is missing. |
| A shell reconcile | Can register with proof. A command is not a reliable recovery action: a budget spent before registration reaches the hub leaves the session recovering. Registration may already have succeeded before a later timeout. ADR 018's breaker changes later hooks' scope; it does not register in the background. |
| A heartbeat from a shim retaining the binding | Reattaches with its generation. An unbound shim cannot send that proof; Codex id-mode sub-agent lookups never renew. |
| An unbound shim's heartbeat tick (ADR 027) | Looks the session up as a call without a handle would. It binds only an Execution live at the hub, so it cannot discover a recovering one, and it never registers or binds one found lost. It finds the generation a registering hook obtained, then renews it. Not for Codex, a process in id mode or `iris watch`. |
| Another MCP call | Binding lookups do not reattach. A call can discover a session a hook already registered; primary `whoami` also heartbeats after a successful bind. Initial legacy fallback registration carries no prior generation and cannot recover a recovering session. |
| Waiting past the deadline | Loses the old Execution and its reservations. A later registering hook can obtain a new generation, never revive the old one. |

Resume the session owning the Execution to fire its enabled `SessionStart` hook, or submit a
prompt on a client whose Iris hook registers it, then retry the tool. Hooks must be working
and reach the hub. A shell command is not a substitute when reconciliation keeps running
past its budget. Run `iris doctor` if hooks or binding still fail.
After generation replacement, legacy binding may need the current `SessionStart` execution
handle, or a shim restart with an updated `IRIS_EXECUTION`. An override may name a different
session: only that session's hooks reattach it, and a live override wins over call metadata.
Otherwise Codex id mode binds by session id and never uses a handle; a root shim can use its
remembered session when a call omits it.
For a replayed claim whose saved fence is no longer live, check current claims and request a
new claim with a fresh key; registering again does not restore that fence.

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

The response also carries the `attention` block described under `POST /v1/sessions`.
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

An undelivered (`pending` or `inflight`) non-human recipient carries `"unreachable":true` when it
has no active Execution: its Executions are lost, ended, or absent, using the same definition as
the delivery-problems query. The field is omitted for reachable recipients and for all `acked` or
`handled` deliveries. Human recipients always omit it, including `human`, `human:<repo>` and
`repo:` addresses resolved to a human inbox: the console can read their mail without a live
Execution. For example:

```json
{"recipient":"codex-host-api","repository_id":"rp_...","state":"pending","at":null,
 "unreachable":true}
```

This describes reachability now and cannot predict whether a reply will come. Mail stays queued
and can be delivered if the agent returns; its next active Execution removes the indication.
An active Execution does not guarantee a reply either. Existing states and timestamps are
unchanged, and clients may ignore the additive field.

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
The version includes the build label, full commit and dirty state, exactly as `iris version`
on the hub. Missing build metadata is reported as unknown.

## Events

`GET /v1/events?execution_id=...&after=123&limit=100` replays events in order:

```json
{"stream":"<uuid>","events":[{"cursor":124,"kind":"inbox_changed"}],"high_water":124}
```

Persisted consumer cursors advance monotonically and an unchanged cursor is not rewritten.
Wake-ticket streams check the ticket's generation in the same transaction as each event
snapshot.

The bearer-authenticated GET routes listed below accept `generation` in the query and reject
malformed or negative values. A positive value is checked by `GET /v1/events`,
`GET /v1/watch` and `GET /v1/messages/{id}/status`, and by `GET /v1/claims` and `GET /v1/agents`
when they name `execution_id` (but not `GET /v1/agents` with `all=1`); if it is stale, the hub
answers `lost`.

The following reads check Execution liveness but ignore a valid `generation`:
`GET /v1/inbox`, `GET /v1/messages/history`, `GET /v1/threads/{id}`,
`GET /v1/messages/{id}/body` and `GET /v1/deliveries/{id}/body`.
`GET /v1/status`, `GET /v1/hooks/seen`, `GET /v1/agents?all=1`, and claims or agents reads
without `execution_id` also ignore a valid `generation`. Without `generation`, or with
`generation=0`, these bearer-authenticated reads remain unqualified, as before; `iris watch`
and older shims rely on this.

The Codex wake runner always sends its positive binding's `generation` to both
`/v1/events` and `/v1/watch`. It omits `consumer_id`: its connection neither occupies the
`iris-watch` monitor slot nor saves a cursor that retains the log. It asks for `attention=1`
only on authoritative events reads; socket frames are triggers to read, never delivery proof.

Codex enables the runner by default; `iris mcp --client codex --wake=false` disables it.
Before admission, the local MCP child requires the exact thread's verified native idle status
through Codex's public app-server proxy, then obtains fresh attention and told state. Active,
unloaded, unavailable or unknown native state defers without quota use. Preflight failure
retries first ask authoritative attention again and stop when no wake is needed. The runner
creates no automatic ten-minute second wake for the same identity; prompt-hook reminders are
unchanged. These are client-side decisions under [ADR 023](adr/023-default-codex-wake-after-idle-check.md),
not new hub routes or an atomic native status-and-queue operation.

`attention=1` adds the attention block described under `POST /v1/sessions` to an events read; a
read that does not ask carries none. Watch frames never include the block.

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
The single-use ticket expires after two minutes for new handshakes. Its `expires_at` does not
close an established socket. Issuing a newer ticket for the same Execution rotates and closes
that socket; Execution loss or end and revocation of the issuing host token also close it.
Another Execution's ticket does not affect it. Disconnects and hub restarts end the connection.
A stolen ticket used before expiry can keep an already-open socket for the rest of the
Execution's life, with no fixed time limit while it stays live and no rotation or revocation
occurs. Ticket expiry cannot limit that socket's lifetime.

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

The browser console uses named accounts under ADR 014 and
[owner setup and restart](console-activation.md) under ADR 019 and ADR 020.
`Store.CallConsole` accepts only a trusted typed console principal and
rechecks its persisted session, enabled account/authentication version, operation
and object scope within one transaction. Profile/access methods use the same
checks; only operators list accounts or change access. Revision conflicts refuse
stale edits, and the last enabled operator cannot be disabled or demoted.

Members have no default repository access. `ro` and `rw` read
only the granted union when the filter is omitted. Explicit inaccessible objects
return `not_found`; prohibited operations return `forbidden`. Search matches,
titles, counts, participants and pagination use visible messages only. Received
cross-repository envelopes show body/sender but omit outside repository IDs,
recipient names/states and session metadata. Authorship never grants access.
Only operators acknowledge shared mail, revoke claims or view diagnostics.

Named sends require an account session; bearers and mixed credentials are
refused by the console boundary. Members supply `repository_id` with current
`rw`, explicit non-human Agent IDs with current `rw` on every destination, and
a reply parent visible in that origin. The batch commits atomically. Both
operators and members use `(account ID, retry key)` across sessions/restarts;
changed requests return `key_reused`. Exact retries reauthorize the moved message
and saved current recipients, without resolving addresses again. Deleted results
retain tombstones for 24 hours, and retired repository IDs cannot start sends.
New sends are limited to 120 a minute per account, with a separate limit of 120
cross-repository recipients a minute per account. These in-memory quotas reset
on restart; exact retries do not consume either quota.

Message, body, history, send and receipt projections include immutable
`sender_account_id`, `sender_username` and `sender_display_name` when attributed.
Old shared human messages have no named author. Human envelopes include
`reply_address: "human:<current repository ID>"`; this shared address and
`reply_to` never add recipients automatically. Per-account refresh revisions
advance only for authorized changes, independently of the global event sequence.
Ordinary startup loads an independent protected console key from
`<database>.console.key`, or `--console-key-file`. Missing, unreadable, malformed or
owner-token-reusing key material, or absence of an enabled operator, keeps the
browser namespace closed: requests return 401 with
`X-Iris-Console-Mode: closed`. Agent routes, authenticated doctor PUT and owner
create/reset/activation remain available. No key is created by ordinary
startup. An active console's unsigned session 401 shows the named login form;
CSRF tokens stay in memory and each login retry obtains a fresh challenge.
Only a missing console key sets the closed marker; an untrusted browser
transport does not. The UI reports unavailability and directs the user to the owner.

`GET /v1/ui/accounts` returns `{ "accounts": [...] }` with each account's
safe profile, role, disabled state, authentication/edit versions and `grants`.
Grants are an explicit array of `{ "repository_id": "rp_...", "mode": "ro" }`
or `rw`; `[]` means no repository grants. Account revisions and grants come from
one authorized transaction. Members cannot obtain this directory.
`PUT /v1/ui/accounts/{id}/access` takes `edit_revision`, `role`, `disabled` and
the complete `grants` array, and returns `{ "account": ... }` with the canonical
saved account and grants. `grants` is required for every role: a missing or null
value is `bad_request`, and `[]` is the only way to remove all grants. No grant is
inferred for a member. Non-member roles accept no grants, and `none` is forced
disabled. A stale revision, removal of the last enabled operator or a repository
that no longer exists returns `conflict` without changing access. The editor
reloads current state for review before another save. Every accepted `PUT` ends
all of the target's sessions, even one that changes nothing; the editor therefore
offers Save only after an edit. These endpoints expose no password record or login
key and do not change the process's startup gate.

- `GET /v1/ui/login` creates a short-lived single-use challenge bound to an
  HttpOnly pre-login cookie. `POST /v1/ui/login` consumes it, requires exact
  same-origin `Origin` and `X-Iris-CSRF`, and verifies bounded credentials.
- A successful login issues a fresh opaque 256-bit host-only cookie; SQLite stores
  only its domain-separated HMAC digest under the independent console key.
  Sessions have a twelve-hour absolute lifetime across restarts and at most
  sixteen active sessions per account. `GET /v1/ui/session` returns the current account and
  session-bound CSRF token, never a password record or session secret.
- Every account mutation requires exact origin and CSRF, including logout,
  password/profile/access changes, sends, shared read/ACK and claim revocation.
  The token belongs in the request header and UI memory, never a URL or browser
  persistent storage. WebSocket upgrades require exact origin and a valid account
  session.
- Mounted consoles also send their session CSRF token on HTTP reads. A supplied
  token must match the request cookie; a mismatch returns 401 before console data
  is served. This binds reads triggered by an existing socket to that tab's
  session when another tab replaces the shared cookie. Bootstrap reads may omit
  the token. The affected tab discards its console, drafts and cached locale;
  neither account's server session is revoked by this mismatch.
- `POST /v1/ui/logout` revokes the current session and clears its cookie. A
  same-origin retry after expiry or revocation only clears the stale cookie.
  Password changes verify the current password and revoke every session,
  including the caller's. Password resets, disabling and role/grant changes also
  revoke all sessions atomically; profile/locale changes preserve them.
- Login attempts are limited before password work to five per normalized login
  key in fifteen minutes and thirty aggregate per minute, with bounded expiring
  digest-keyed buckets. Unknown names consume the same quotas. Across login,
  change, create, reset and activation, at most two KDF jobs run concurrently
  with no queue.
  Refusal returns `rate_limited` and `Retry-After`; the limits reset on restart.
  Missing/disabled/unusable accounts and incorrect passwords return the same
  `unauthorized` status and `Invalid username or password`, with bounded dummy
  work after admission. Only Argon2id hashes are accepted. Stored scrypt hashes
  take the same dummy-verification path as unknown, disabled or unreadable accounts;
  login never converts a stored hash.
- Old sessions become `unauthorized` after an `rw` downgrade. Reauthenticated
  `ro` accounts can read but cannot send, reply or replay a cached send result.
  Authorization runs inside the write transaction. Socket frame admission shares
  the revocation boundary, and idle sockets wake on access/password changes;
  an already admitted frame may be in transit.

Owner-authenticated `iris admin console create <username>` and
`iris admin console reset <username>` create or recover a named operator through
`POST /v1/admin/console/accounts` and `POST /v1/admin/console/accounts/reset`,
never a browser session. Host tokens cannot invoke those paths. For create,
reset and activate, the CLI reads at most 1,027 stdin bytes and removes exactly
one optional final LF or CRLF before validating the 1,024-byte/UTF-8 password
bound. Bare CR, embedded line endings, spaces and additional final newlines are
preserved; there is no general trimming or normalization. Passwords never enter
command arguments, URLs or logs. The server verifies JSON password bytes exactly
and does not apply CLI line framing. Malformed UTF-8 and unpaired surrogate
escapes in account JSON are rejected before decoding can replace credential
characters. The owner endpoints work while browser authentication is closed.
Create/reset passwords require at least fifteen Unicode code points and at most
1,024 UTF-8 bytes. A failed console-key load refuses browser authentication
without stopping coordination. Account cookies use `iris_console_session` and
the pre-login `iris_console_login` namespace, with HttpOnly, Path=/,
SameSite=Strict and Secure outside literal-loopback HTTP. The retired
`iris_session` cookie grants no authority. The ADR 002 exact
public-origin/trusted-loopback-proxy requirements still apply. Activation's
session deletion applies to named `ConsoleSession` rows. An older binary can
restore earlier browser authority, so rolling back to it still requires the
proxy boundary.

On a hub whose database has no console account at all and that has no console
key, `create` also prepares sign-in. The no-account/no-key check and operator
creation are one transaction, including owner audit records `owner_provision`
and `activation_prepared`; the latter records preparation, not password
verification or successful key persistence. Only after commit does the hub
prepare the independent protected key. Success returns `{ "account": {...},
"account_created": true, "key_prepared": true, "restart_required": true }`.
The CLI prints this result: an operator was made, the key was prepared and a
restart is needed. The current process remains closed; one restart opens named
login. On every other hub `create` only makes an enabled operator and returns
`account_created: true`, `key_prepared: false`. In particular, `create`
prepares no key when an account exists without a key or a key exists without
any account. Restart readiness is separate from key preparation:
`restart_required` is true when the console is currently closed and a valid
key and an enabled operator are available after creation, so restarting would
open named login. On a hub with a valid key but no accounts, `create`
therefore returns `key_prepared: false`, `restart_required: true`; restart to
sign in with the new operator. On a hub with an account but no key, `create`
returns both flags as false; `activate` is still needed. Creating another
account on an already open console also returns both flags false. The CLI
separately reports account creation, key preparation and whether a restart is
required. If key preparation fails after commit, the error states that the
operator was created and the key was not prepared; recover the key through
owner review and use `activate` rather than creating that account again. For
an intended member or disabled account, save its role, state and grants in the
access editor and read them back before handing out its password.

`iris admin console activate <username> < /private/password` calls
`POST /v1/admin/console/activate` with `{ "username": "...", "password": "..." }`.
It uses the one-ending CLI line framing above, with valid UTF-8 and a
1,024-byte bound after framing. It accepts Argon2id credentials only, without
applying the new-password fifteen-code-point minimum: a short Argon2id password
retained through an earlier upgrade still verifies. An unreadable stored hash
is refused with fixed owner advice: this version does not read the stored
password format; recover the operator with `iris admin console reset <username>`,
then activate. Browser login receives no format-specific advice. The hub verifies
the password outside the store lock, then transactionally rechecks the enabled
operator snapshot/authentication version, clears every console session and records
`activation_verified`.
Only afterward does it create/load the independent key; malformed existing
material is refused. The record does not prove key persistence. Success returns
`{ "account": {...}, "restart_required": true }`, without a browser cookie or
hot activation. Restart with the prepared key opens named authentication.
An already active console returns 409 `conflict` without clearing sessions.
Normal restarts preserve original absolute session expiry. The owner performs
setup or recovery in their own terminal and stays through sign-in checks;
whoever deployed restarts after key preparation. Agents do not perform real
owner activation or receive passwords in chat.
See the [activation and recovery procedure](console-activation.md).

All owner account setup, recovery and activation endpoints require exactly one
owner bearer credential without a Cookie header. Host tokens and browser
sessions cannot invoke them. Account and grant import commands and routes have
been removed.

Browser routes below require named account authentication. The sole machine
exception is authenticated `PUT /v1/ui/doctor`, which works even while the console
is closed. Agents use their machine APIs. `/v1/ui/links` and `/ui/login` return
404. The retired cookie and machine bearers cannot establish browser sessions.

Open the hub's URL in a browser (`http://127.0.0.1:7878/` by default for a local
hub). The root serves the embedded sign-in UI. Public named
authentication requires `serve --public-url https://host` behind a trusted
loopback reverse proxy, with the exact public Host and `X-Forwarded-Proto: https`.
The proxy must strip incoming forwarding headers before setting its own.
Without that trusted-proxy mode, plain HTTP browser authentication is restricted
to literal loopback.

`GET /v1/ui/overview` returns everything the first screen needs:

Each visible repository also has `console_access`,
either `ro` or `rw` (`rw` for an operator).
It is presentation metadata, not authorization supplied by the browser. Member
queries remain scoped by current grants in the store transaction.

```json
{"repositories":[{"id":"rp_...","name":"iris","aliases":["..."]}],
 "agents":[{"id":"ag_...","name":"...","client":"claude","host":"...","repository_id":"rp_...",
            "worktree":"...","branch":"main","state":"active","last_seen":"...","unread":0}],
 "claims":[{"fence":1,"repository_id":"rp_...","path":"...","kind":"exact","holder":"...",
            "origin":"auto","expires_at":"..."}],
 "human_unread":0}
```

`GET /v1/ui/deliveries?repository_id=...` lists at most 200 of the human's own messages whose
delivery is in trouble, newest first. The cap counts messages after recipient filtering, not
recipient rows; each listed message includes all its qualifying recipients. `more` is `true` when
additional qualifying messages are omitted. There is no pagination. It looks back `window_s` seconds
(7 days). A recipient is listed when its delivery is still unacked and either it has no active
Execution (`unreachable`) or the message is older than `wait_s` seconds (15 minutes) (`waiting`).
The `waiting` age is measured from the message's creation, not from when a recipient last fetched
it or held it. `waiting` is a threshold, not a verdict: the recipient may only be busy. An acked
or handled delivery is never listed, so nothing already stored is offered for sending again.
With `repository_id`, only messages sent from that repository's view are listed. No body is returned.
`execution` is the recipient's latest Execution state: `active`, `lost`, `ended`, or `none`.

```json
{"messages":[{"id":"ms_...","thread_id":"ms_...","subject":"...","created_at":"...",
              "recipients":[{"name":"...","state":"pending","reason":"unreachable","execution":"ended"}]}],
 "window_s":604800,"wait_s":900,"more":false}
```

`GET /v1/ui/threads?repository_id=...&agent=...&q=...&before=...&limit=50` lists threads, newest
first, ordered by the last message visible in the view and then by thread id. `agent` matches an
agent name or id; `agent=human` selects conversations sent or received by the operator across
all repository mailboxes. Repository and search filters still apply. `q` is a full-text
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

`status` is `ok`, `not_configured`, `needs_human`, `silent` or `slow`. `silent` includes a failed
hook check or a hook that failed open in the last 24 hours; `detail` says which. A recent fail-open
detail warns that edits may have gone unclaimed. It does not establish that the client is down now
or that its hooks never reached the hub. `slow` means the hooks run,
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

This endpoint identifies a checkout, not the committing session. Session or Execution fields
do not exempt claims in another worktree. Each conflict includes the holder's `branch`,
`worktree` and `host`, as well as the claim's `fence`. The message conditionally explains
an own-session claim from another worktree and tells the holder to use the `release` MCP tool
with that fence from the holding session before retrying. It does not assert that the committer
is the holder. Release still checks generation and ownership; no new exemption is granted.

`POST /v1/shell/reconcile` accepts `execution_id`, `generation`, `kind` (`before` or
`after`), and `status`, a map of path to porcelain XY status. The hub stores a
snapshot per Execution. `before` establishes the baseline; `after` compares and
replaces it, returning the same response shape. The first post without a baseline
only records it. Multiple live Executions in the same host/worktree suppress
warnings and clear their baselines. Status strings are observations, never commands. Every
successful answer on this route and on `POST /v1/shell/scoped` carries the `attention` block
described under `POST /v1/sessions`. The planned shell hook change will use it to announce new
attention as an edit hook does (ADR 017).

Instead of `execution_id`, a reconcile may carry the session's registration fields, those of
`POST /v1/sessions` with `session_id` and the remembered `generation`. The hub then registers
the session exactly as `POST /v1/sessions` would, recovery proof included, and reconciles for
the Execution it gives, in one transaction. A shell hook therefore makes one round trip instead
of two. The response adds
`"session":{"execution_id":"ex_...","generation":4,"capabilities":["shell_scope"]}`, whose
generation the hook remembers as its next proof. A hub from before this form answers it with an
error, and the hook then registers and reconciles in two calls.

`POST /v1/shell/scoped` takes a reduced snapshot (ADR 018), in either form, with the same fields
plus `scope`:

```json
{"mode":"dirty","submodules":["modules/data","modules/tools"]}
```

The snapshot's status ran with `--ignore-submodules=dirty`, and `submodules` names the gitlink
paths in the index that status read: relative, clean, sorted, unique, and at most 500. The plain
route refuses a request with a scope, and this route one without. A hub from before this route
answers `not_found`, so a hub that does not understand scope never reads a reduced map as full.

A snapshot is compared only with a baseline of the same scope, the submodule paths included.
Any other baseline is replaced and nothing is attributed. In reduced scope a named submodule
path that the status does not report is unknown, not clean: nothing is released or renewed for
it. The hub clears reduced baselines whenever it starts, so the first reduced snapshot after a
restart only records its baseline. A hub from before this route never reads them.

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
finish. Named account sessions have independent revocation and absolute expiry;
bearer revocation does not revoke them. The retired browser cookie has no authority.

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
