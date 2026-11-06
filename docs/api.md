# Hub API, version 1

The contract between `iris serve` and its clients: `iris hook`, `iris mcp`, `iris watch`,
`iris guard` and the web UI. Behaviour is defined in `docs/architecture.md`. This file fixes the
names so that independently built parts meet.

Requests and responses are JSON. Authentication is `Authorization: Bearer <token>`. Retryable message and explicit-claim creation carry a client-chosen `key` (D9). Touch uses
`tool_call_id`; lifecycle calls are idempotent by session/generation, and release and ack
are idempotent by fence and delivery attempt respectively. Errors look like this:

```json
{"error":{"code":"conflict","message":"human readable","details":{}}}
```

Codes: `unauthorized`, `bad_request`, `not_found`, `conflict`, `key_reused`, `unbound`,
`ambiguous`, `lost`, `too_large`.

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

`POST /v1/bind` resolves a stdio MCP process to an Execution, following D6.

```json
{"client":"cursor","host":"...","execution":"","session_id":"",
 "client_pid":123,"client_pid_start_ms":1793917887311,"cwd":"..."}
```

It returns `{"execution_id":"...","generation":1,"agent":{...}}`, or `unbound`, or `ambiguous`.

`POST /v1/executions/{id}/heartbeat` takes `{"generation":1,"source":"shim"}`, where `source` is
`shim`, `watch` or `hook`. It returns `{"lease_ms":60000}`, or `lost`.

`POST /v1/executions/{id}/end` is a best-effort end of session. It takes `{"generation":1}`.

## Claims

`POST /v1/claims/touch` is the pre-edit check-and-claim. It is one transaction (D10).

```json
{"execution_id":"ex_...","generation":1,"tool_call_id":"...",
 "paths":["src/a.go"],"rename_from":"","rename_to":""}
```

Response, always 200 when the request is valid:

```json
{"action":"allow","granted":["src/a.go"],"ignored":[],
 "conflicts":[{"path":"src/a.go","holder":"codex-host-api","origin":"auto",
               "expires_at":"...","fence":7}],
 "message":"text shown to the user and the agent"}
```

`action` is `allow`, `deny`, `ask` or `warn`. Paths are repository-relative with forward slashes.

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
fetch increases `attempt`.

```json
{"deliveries":[{"id":"dl_...","attempt":1,"sender":"...","subject":"...","body":"...",
                "created_at":"..."}]}
```

`POST /v1/deliveries/{id}/ack` takes `{"execution_id":"...","generation":1,"attempt":1}`. A repeated
ack succeeds, and a stale one returns `conflict`. Ack means accepted, not acted upon.

## Peers and status

`GET /v1/agents?repository_id=...` lists agents and their current state.

`GET /v1/status` returns version, stream id, boot epoch and counts. It needs no token, and it reveals
nothing beyond those.

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

Frames never carry message text. Kinds so far: `inbox_changed`, `claim_released`, `reset`.

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
| `ack` | Acknowledge a delivery |
| `claim` | Make an explicit claim |
| `release` | Release a claim, or all |
| `claims` | List live claims |
| `watch_ticket` | Get a wake ticket for the `Monitor` tool |
