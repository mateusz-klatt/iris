# ADR 040 — Bind host tokens to their host

Status: accepted by the owner on 2026-11-25. Enforcement stays off until the owner switches it
on, as described below.

The owner agreed to address [issue #442](https://github.com/mateusz-klatt/iris/issues/442)
on 2026-11-25. This proposal is for the owner's acceptance before implementation.

## In short

Issue each host a token that can act only for that host's sessions and mailboxes. Keep
ordinary cross-host messaging and the named console operator's fleet view. Let existing
clients switch tokens in place, then have the owner explicitly end fleet-wide machine
access. A stolen host token will still give the holder that host's authority.

## Context

- `iris token add <host>` sends its argument as `host`. The owner-only mint handler
  uses it as the token's name and returns the new secret once. The store generates
  32 random bytes and persists the SHA-256 digest of the encoded bearer. A revoked
  label can be reused with a new secret. Migration 005 defines `HostToken` with a name,
  digest, creation and revocation times, and usage counters. It has no host column.
  [cmd/iris/token.go:23–32](../../cmd/iris/token.go#L23-L32),
  [internal/hub/tokens.go:85–99](../../internal/hub/tokens.go#L85-L99),
  [internal/store/tokens.go:13–39](../../internal/store/tokens.go#L13-L39),
  [internal/store/migrations/005.sql:1–9](../../internal/store/migrations/005.sql#L1-L9).
- `iris serve` reads its owner bearer from its own token file. The hub checks that
  bearer first and labels it `(legacy)`. Other bearers are looked up by digest and
  revocation state on each request, updating `last_used_ms` and `requests`. The HTTP
  principal carries only name and digest. Ordinary machine HTTP dispatch calls
  `Store.Call`, with the digest used for message rate attribution. The separate
  store authorization matrix has a principal kind and a console session digest,
  with no host predicate for a machine principal.
  [cmd/iris/main.go:133–152](../../cmd/iris/main.go#L133-L152),
  [internal/hub/tokens.go:14–51](../../internal/hub/tokens.go#L14-L51),
  [internal/store/tokens.go:68–83](../../internal/store/tokens.go#L68-L83),
  [internal/hub/hub.go:73–97](../../internal/hub/hub.go#L73-L97),
  [internal/hub/hub.go:247–278](../../internal/hub/hub.go#L247-L278),
  [internal/store/authorization.go:7–50](../../internal/store/authorization.go#L7-L50).
- Migration 001 stores `Agent.host` and `Execution.host_id` as text. There is no
  separate host registry. Session identity includes `(host_id, client, session_id,
  generation)`. Hooks and the resolver use the OS hostname. Migration 029 adds
  succession provenance to `Execution`. The stage 1a transaction selects the
  predecessor using the request host and requires the exact predecessor pair.
  [internal/store/migrations/001.sql:11–25](../../internal/store/migrations/001.sql#L11-L25),
  [internal/hook/runner.go:721–734](../../internal/hook/runner.go#L721-L734),
  [internal/binding/binding.go:130–139](../../internal/binding/binding.go#L130-L139),
  [internal/store/migrations/029.sql:1–5](../../internal/store/migrations/029.sql#L1-L5),
  [internal/store/succession.go:56–91](../../internal/store/succession.go#L56-L91).
- Local setup passes the resolved client token path to the hub service and to
  doctor. Antigravity and Kimi wake use the OS-account-pinned configuration,
  which accepts only the pinned `hub.token` path. For ordinary commands,
  `IRIS_TOKEN_FILE` also selects the directory containing `client.json`.
  [cmd/iris/setup.go:158–193](../../cmd/iris/setup.go#L158-L193),
  [internal/hubclient/pinned.go:30–46](../../internal/hubclient/pinned.go#L30-L46),
  [internal/wake/antigravity.go:329–332](../../internal/wake/antigravity.go#L329-L332),
  [internal/wake/kimi_once.go:102–124](../../internal/wake/kimi_once.go#L102-L124),
  [internal/hubclient/config.go:65–112](../../internal/hubclient/config.go#L65-L112).
- Restore-check and live doctor start loopback hubs with a throwaway owner
  bearer. Restore-check uses it on session and watch routes, and live doctor
  writes it to the token file used by child hooks and MCP.
  [internal/restorecheck/restorecheck.go:47–105](../../internal/restorecheck/restorecheck.go#L47-L105),
  [internal/doctor/live.go:254–274](../../internal/doctor/live.go#L254-L274),
  [internal/doctor/live.go:319–341](../../internal/doctor/live.go#L319-L341).

The trust boundary for machine routes is any valid bearer. A copied host token
therefore carries authority over other hosts' sessions and mailboxes when their
identifiers are known. A named token gives the owner attribution and independent
revocation. The HTTP `Host` header is a separate transport check.
[docs/architecture.md:1185–1208](../architecture.md#L1185-L1208),
[internal/hub/hub.go:113–129](../../internal/hub/hub.go#L113-L129).

| Current routes | How the hub selects the acting host or Execution today |
|---|---|
| `POST /v1/sessions`, `/v1/sessions/outside`, `/v1/sessions/succeed` | Registration and succession take `host` from the request. [internal/store/sessions.go:412–434](../../internal/store/sessions.go#L412-L434), [internal/store/succession.go:56–77](../../internal/store/succession.go#L56-L77) |
| `POST /v1/bind` | Session and process searches use request `host`. An explicit `execution` handle must match the supplied client and host. [internal/store/sessions.go:616–648](../../internal/store/sessions.go#L616-L648) |
| `POST /v1/shell/reconcile`, `/v1/shell/scoped`, `/v1/guard/check` | Both shell routes accept either session registration with the supplied host or an existing Execution. Guard uses the supplied host for checkout identity and can adopt repository aliases. [internal/store/reconcile.go:13–48](../../internal/store/reconcile.go#L13-L48), [internal/store/check.go:9–33](../../internal/store/check.go#L9-L33) |
| `POST /v1/executions/{id}/heartbeat` and `/end` | The path supplies the Execution ID. Recovery heartbeat and repeated end have branches before the normal live lookup. [internal/hub/hub.go:255–265](../../internal/hub/hub.go#L255-L265), [internal/store/sessions.go:694–712](../../internal/store/sessions.go#L694-L712), [internal/store/sessions.go:731–749](../../internal/store/sessions.go#L731-L749) |
| Claims, mail, and attention | Claim mutations and Execution-backed discovery, send, inbox and receipt, ack, message receipts, body reads, history, threads, events, watch, and wake tickets use the acting Execution. The all-agents list skips the Execution lookup even when `execution_id` is supplied. Existing mailbox rules check the Agent's participation, recipient, or delivery attempt. [internal/hub/hub.go:83–96](../../internal/hub/hub.go#L83-L96), [internal/store/api.go:44–52](../../internal/store/api.go#L44-L52), [internal/store/api.go:102–148](../../internal/store/api.go#L102-L148), [internal/store/messages.go:189–246](../../internal/store/messages.go#L189-L246), [internal/store/history.go:24–39](../../internal/store/history.go#L24-L39), [internal/hub/hub.go:319–347](../../internal/hub/hub.go#L319-L347), [internal/hub/hub.go:375–439](../../internal/hub/hub.go#L375-L439) |
| `GET /v1/hooks/seen` and `PUT /v1/ui/doctor` | Hook observations use the query's host, and a report uses its body's host. An omitted report host goes to `unknown`. [internal/hub/hub.go:195–206](../../internal/hub/hub.go#L195-L206), [internal/store/api.go:147–152](../../internal/store/api.go#L147-L152), [internal/hub/operator.go:54–68](../../internal/hub/operator.go#L54-L68), [internal/store/operator.go:188–197](../../internal/store/operator.go#L188-L197) |

## Options

| Option | Benefit | Cost and limit |
|---|---|---|
| 1. Keep the fleet credential and document it | Fits every existing deployment. Tokens are already independently revocable. | A stolen token retains fleet-wide machine authority until revocation. |
| 2. Bind tokens at issuance and enforce on every session and mailbox route | Limits stolen-token authority to one logical host. Existing clients can retain their wire protocol. | Requires all machine entry points to carry the principal, plus migration, host enrollment, and a coordinated cutover. |
| 3. Record mismatches before enforcing | Gives the owner evidence about current coordination and time to replace credentials. | Preserves fleet authority during observation. A token label, reported host, or recent usage cannot establish ownership or prove that an idle client has migrated. |
| 4. Also scope admin and console access per host | Could support delegating some operations to someone responsible for one host. | Repository repairs, operator mail, and repository grants span hosts. Delegation would need its own authority and audit rules. |

## Decision

Choose option 2, with a transition for existing credentials. Enforce newly bound
tokens from issuance, including during the transition. Use option 3 only to surface
remaining legacy usage. Defer option 4.

The bound value is the deployment's exact, case-sensitive logical host ID, which
matches today's `Execution.host_id` and the host string this installation puts on
the wire. The owner verifies it out of band before issuing the bearer. It can be
enrolled before any session exists. Multiple revocable labels can refer to the same
ID for rotation. Hosts that send the same ID share one identity and must use unique
IDs before the owner can isolate them. Keep token labels subject to their existing
validation.

### Host IDs and reserved identities

Require a host ID to be 1–255 bytes of valid UTF-8 with no Unicode whitespace,
control characters, or `/`, `\`, or `:`. The colon separates the host from the
path in local repository aliases. Reserve `operator` for the console and
`unknown` for reports from old clients that omit the host. Reject ASCII case
variants of both reserved names. For other names preserve the exact spelling
and compare byte for byte. Validate at issuance, for any configured `host_id`,
and on host fields supplied by bound callers. A deployment with
an existing ID that fails these rules can keep operating in compatibility mode
while it retires that logical host and enrolls a valid ID.

Reject `client: "human"` on machine registration, shell registration, bind and
succession in both modes. Reject every stored Execution whose `client` is
`human` when selected as the actor of a machine operation, including terminal
`end` and either watch path. Requests can still address human recipients.
[internal/store/operator.go:12–26](../../internal/store/operator.go#L12-L26),
[internal/store/operator.go:49–64](../../internal/store/operator.go#L49-L64),
[internal/store/operator.go:188–197](../../internal/store/operator.go#L188-L197),
[internal/store/sessions.go:211–218](../../internal/store/sessions.go#L211-L218),
[internal/store/sessions.go:412–425](../../internal/store/sessions.go#L412-L425).

### Authorization matrix

Carry the authenticated kind, digest and bound host into the store as a trusted
principal. Check the operation's authority in the same transaction as its lookup or
mutation, before expiry, recovery, cached replies, or other domain work. Check the
current digest and binding in that transaction so a concurrent revoke or credential
replacement has a defined order with the operation. Replace the store's current
machine denylist with an explicit allowlist of operations and selectors per
principal kind and mode. Missing principals and unlisted operations fail by
default on network entry points, including the separately wired watch, wake
ticket and doctor handlers. In-process maintenance such as the expiry sweep
uses a trusted internal entry point unavailable to any bearer.
[internal/store/api.go:17–39](../../internal/store/api.go#L17-L39),
[internal/store/authorization.go:39–69](../../internal/store/authorization.go#L39-L69).

| Bearer | `GET /v1/status` | `GET /v1/agents`, `GET /v1/claims` and other machine coordination | Owner API |
|---|---|---|---|
| None, invalid or revoked | Availability only | Refused | Refused |
| Bound host | Authenticated status | Allowed with the host and object checks below | Refused |
| Legacy named, in compatibility mode | Authenticated status | Fleet access under the compatibility rules | Refused |
| Owner, in compatibility mode | Authenticated status | Fleet access under the compatibility rules | Allowed |
| Owner, in enforced mode | Authenticated status | `401 unauthorized` with `host_binding_required` | Allowed |

Named console sessions use the console authorization matrix for browser
operations. The machine doctor upload is a distinct bearer route. Owner access
to authenticated status supports `iris status` and restore-check. Refusing owner
discovery after cutover also gives older doctor builds a failed token check
through their existing `GET /v1/agents` probe. A legacy named bearer is
inadmissible in enforced mode even if a NULL row reaches authentication.
[internal/hub/hub.go:247–253](../../internal/hub/hub.go#L247-L253),
[internal/hubclient/status.go:57–60](../../internal/hubclient/status.go#L57-L60),
[internal/store/api.go:167–181](../../internal/store/api.go#L167-L181),
[internal/doctor/doctor.go:898–913](../../internal/doctor/doctor.go#L898-L913).

- For registration, host searches, guard, and diagnostics, require the supplied
  `host` to match the bound ID. Check the host on both shell registrations and explicit
  bind handles. Check any resolved Execution independently.
- For an Execution-backed request, resolve its actual host from the stored row
  using the operation's real selector, including path IDs and the explicit bind
  handle. Check it even on a recovering or terminal row. Existing same-host
  generation, process, handover, lease, and Agent checks still apply. Authorize the
  acting Execution before any mailbox lookup, receipt, claim or replay. The
  predecessor ID on `succeed` remains proof evaluated within the authorized host.
  Whenever `/v1/agents` or `/v1/claims` supplies `execution_id`, validate the
  actor before expiry and before returning results, including for `all=1`.
  An owned live Execution still works with `all=1` and returns the ordinary
  directory. Discovery without an Execution keeps its current scope. Cross-host
  recipients remain available under the existing addressing rules.
- For wake tickets and bearer watches, authorize the Execution before allocating a
  ticket, rotating an existing ticket, taking a monitor slot, reading a cursor or
  upgrading a connection. A ticket carries its issuer's scope and is usable only
  for its original Execution and generation. Under the same hub lock as
  revocation, revalidate the issuing digest and scope before rotating or
  inserting any ticket. Apply the scope to continuing stream reads and preserve
  revocation of tickets and sockets.
  [internal/hub/hub.go:319–352](../../internal/hub/hub.go#L319-L352),
  [internal/hub/tokens.go:101–134](../../internal/hub/tokens.go#L101-L134).
- With ADR 039 stage 1b in place, `POST /v1/sessions/succeed/exact` is the only
  succession endpoint. Check the authenticated host before proof arbitration or
  replay. Keep owned create and replay semantics and carry over the proof
  mismatch assertions from the stage 1a test
  [TestHTTPSucceedRouteTwoValidHostTokens](../../internal/hub/succession_test.go#L405-L408).
  Replace its attribution-only cross-host success pin with refusal, and check
  own-host creation and replay with two different labels for that host.
  [internal/hub/succession_test.go:458–495](../../internal/hub/succession_test.go#L458-L495).

Use the existing HTTP 401 `unauthorized` envelope for a bearer unusable for the
acting host. Add an allowlisted `details.reason` of `host_mismatch` or
`host_binding_required` and a fixed message directing the user to `iris doctor`
and the host's owner-issued credential. The response contains no expected host,
session, mailbox, or bearer. Missing objects keep existing lookup and recovery
semantics. A known foreign Execution refusal can differ from a missing Execution's
ordinary response.

A refusal is a coordination failure under D3. Hooks yield the adapter's valid
allow output within the existing deadline, append the bounded and sanitized
failure record, and show an actionable warning where that event supports context.
Existing hooks already handle `unauthorized` as fail-open, with a generic credential
warning where supported. Keep host ID lookup and validation inside the guarded
hook execution and its 1.5 s budget. Give malformed host configuration and
both recognized authorization reasons fixed failure categories for the hook
log and doctor bundle. Never copy hub detail text or configuration values
into these reports.
MCP returns the typed `unauthorized` error and repair hint even if the resolver
already recorded a lost binding. A credential refusal takes precedence over
`LostError` wrapping. Authorization failure stops binding,
reattachment and succession attempts for the refused request. The refused reply
cannot cause a generation or end-marker write, a fallback registration or an
edit denial. A normal same-host conflict or proof refusal keeps its own behavior.
[docs/architecture.md:80–119](../architecture.md#L80-L119),
[cmd/iris/hook.go:41–43](../../cmd/iris/hook.go#L41-L43),
[internal/hook/failures.go:119–175](../../internal/hook/failures.go#L119-L175),
[internal/hook/runner.go:379–389](../../internal/hook/runner.go#L379-L389),
[internal/binding/binding.go:256–282](../../internal/binding/binding.go#L256-L282),
[internal/mcpserver/server.go:202–213](../../internal/mcpserver/server.go#L202-L213).

## Migration and operation

1. Add nullable `HostToken.host_id` and a singleton binding mode in SQLite.
   Backfill existing named rows, active and revoked, as NULL. Preserve their
   digest, usage and revocation. There is no trustworthy assignment from the
   label, IP address or past requests. The file-backed owner credential has
   no row to backfill. Use the pre-migration `PRAGMA user_version` to identify
   existing Iris databases. A versioned database without binding policy starts
   in compatibility mode unless deployment policy requires enforcement. This
   applies even with zero named tokens. An empty database starts enforced.
   Refuse a nonempty unversioned database.
2. Mint new tokens with separate `name` and `host_id` inputs on owner-only
   `POST /v1/tokens/host`. Return its label, host ID and one-time secret. The
   issuance command requires a response confirming the requested binding.
   Old hubs return an unsupported-route error before issuance. On the upgraded
   hub, the old mint route gives an explicit upgrade instruction because its
   single argument was a label. Keep existing list and revoke requests working.
3. On each client installation, place its new token in the existing user-only
   token file by atomic replacement. Keep the pinned path for wake and move any
   shared owner file first as described under [Local hub setup](#local-hub-setup).
   Keep the logical host ID. Check registration or reattachment, a mailbox read
   and the watch reconnect with the new credential, then revoke the old named
   credential when its last consumer has moved. HTTP reads the token file on
   subsequent requests. Reconnect existing watches and restart clients that
   changed configuration or cache it. A bound and a legacy token can coexist
   during rotation.
   [internal/hubclient/config.go:134–145](../../internal/hubclient/config.go#L134-L145),
   [internal/hub/tokens.go:101–117](../../internal/hub/tokens.go#L101-L117).
4. Once the owner confirms the active token inventory and verifies real traffic
   from every installation, stop the hub, rotate the owner file so distributed
   copies become invalid, and explicitly start it with
   `iris serve --require-host-tokens`. Restart drops old tickets and sockets.
   Persist the setting in managed service configuration. The rotated owner
   bearer can still enroll hosts and recover console accounts.

### Local hub setup

Keep the machine bearer at the OS account's pinned `hub.token` for Antigravity
and Kimi. Their `client.json`, session files and wake state keep their pinned
locations. Pinned readers continue to ignore environment overrides and use the
host ID from that account's configuration. Leave existing custom token paths
in use for installations that use ordinary client resolution. Before enabling
either pinned wake integration, setup checks that the hook and pinned profiles
resolve to the same hub URL, client token path and host ID. Report a conflict
and leave that wake integration disabled until the profiles agree.

On a new local installation, `iris setup` uses a separate private
`owner.token` at the hub configuration directory for the service. Set the
service's existing `--token-file` to that owner path, start it enforced, then
use the owner API to enroll the effective local host. Install its bound secret
at the client path before integrating hooks or running doctor. Verify that
owner and client paths name distinct files and keep the owner file out of
client configuration and child environments. A rerun preserves a configured
`host_id` and reuses a working local enrollment. If issuance succeeds but
installation fails, recover the setup-owned label by revoking and reissuing
it. Leave an existing healthy client credential in place until the replacement
can be installed. Remote setup uses an owner-issued host token supplied by the
user.

An existing service keeps its configured owner path during compatibility
until the owner migrates it. If the service and client share a file, copy the
current owner credential to the separate private path, reconfigure the service
and verify its owner access there before replacing the shared client file
with the enrolled host token. Rotate the service's owner secret at cutover.
A service restart or reinstall preserves its owner path and enforcement
setting.
[cmd/iris/setup.go:158–193](../../cmd/iris/setup.go#L158-L193),
[cmd/iris/service.go:104–109](../../cmd/iris/service.go#L104-L109),
[cmd/iris/service.go:208–237](../../cmd/iris/service.go#L208-L237),
[internal/hook/antigravity_wake.go:581–606](../../internal/hook/antigravity_wake.go#L581-L606).

### Owner commands

Use the proposed `--owner-token-file PATH` option before the subcommand in
`iris token` and networked `iris admin`. Examples include `iris token
--owner-token-file PATH list` and `iris admin --owner-token-file PATH merge
--dry-run --from A --into B`. Resolve the hub URL through ordinary client
configuration first, keeping existing `IRIS_HUB_URL` and `client.json`
precedence. Apply the owner option only to bearer loading after resolution.
An unreadable explicit file fails before any request. With the option omitted,
retain the old bearer lookup so existing owner installations keep working.
Setup prints the separate owner file option for management commands. Local
backup, restore-check and mark-restored take their authority from filesystem
access and need no owner file.
[cmd/iris/token.go:17–21](../../cmd/iris/token.go#L17-L21),
[cmd/iris/admin.go:25–29](../../cmd/iris/admin.go#L25-L29),
[cmd/iris/admin.go:87–111](../../cmd/iris/admin.go#L87-L111),
[internal/hubclient/config.go:65–112](../../internal/hubclient/config.go#L65-L112).

### Enforcement and restore

Enforce when the startup flag, the SQLite mode, or a private deployment
enforcement marker requires it. The marker lives alongside the database as
`<database>.host-tokens`, outside the SQLite backup. Either persisted record
is sufficient even without the flag. Before serving in enforced mode, record
the requirement in both places, with an atomic write of the external marker.
A corrupt or unreadable policy record stops startup. There is no compatibility
override for an enforced deployment.

At every enforced open, refuse before accepting traffic if any active
`HostToken` has a NULL host ID. A marked restore onto an enforced deployment
also requires all copied named credentials to have been revoked before
startup. Check these conditions before consuming the ADR 006 restore marker
or committing stream, epoch or recovery changes. If a cutover is interrupted
after writing the external marker, the next start still requires enforcement.
[internal/store/store.go:466–497](../../internal/store/store.go#L466-L497).

Keep the service policy and deployment marker when replacing SQLite. On
replacement hardware, recreate the enforced startup setting before restoring.
For an enforced destination, prepare a stopped copy with the proposed
`iris admin mark-restored --require-host-tokens <database>`. This offline
operation migrates the copy if necessary, revokes all copied named tokens
including previously bound credentials, records enforced mode, and sets the
ADR 006 restore marker in one database transaction. As with mark-restored,
checkpoint the changes into the main file under the lock before reporting
success. Start the prepared copy with the destination's policy and current
owner file, then enroll fresh credentials. An older or compatibility backup
cannot reopen fleet authority on that deployment. A raw database replacement
with no restore marker can
conceal that it is a restore and revive a bound credential. Follow the marked
restore procedure. Recovering without either policy record or service settings
requires recreating the enforcement requirement explicitly.

Use an enforcement-aware binary for rollback. An older binary may ignore
the schema and policy. Running one requires a separate deployment with the
matching pre-migration backup, where fleet trust applies and post-backup
changes are absent.
[ADR 006](006-retention-and-restore.md),
[docs/deploy.md:455–467](../deploy.md#L455-L467),
[internal/store/restore.go:103–121](../../internal/store/restore.go#L103-L121).

### Restore-check and live doctor

Restore-check reports production startup blockers on the untouched snapshot.
Use the snapshot's saved policy by default and pass `--require-host-tokens`
when the destination requires enforcement. Report any copied active credentials
that would block the destination's startup. For the recovery rehearsal, disable
copied credentials in the scratch copy before starting an enforced loopback
hub. Preserve the snapshot's restore-marker state so the rehearsal still
reports a missing marker. Use an independent random owner bearer for status
and mint bound scratch bearers after startup for the restore-check host and
the exact host of the sampled Execution. Run generation-floor, terminal end,
and watch reset probes with the matching host bearer. Select a sample eligible
for machine enrollment and report when there is none. This rehearsal never
writes to the input backup and reports recovery results separately from
production startup readiness.
[internal/restorecheck/restorecheck.go:37–108](../../internal/restorecheck/restorecheck.go#L37-L108),
[internal/restorecheck/restorecheck.go:245–273](../../internal/restorecheck/restorecheck.go#L245-L273).

`iris doctor --live` starts a fresh enforced loopback hub. Keep its scratch
owner bearer in the harness and give the child only a private scratch file
containing a bound bearer. Enroll the effective host ID that the scratch
client profile will send, using the OS hostname by default. Point the child
hook and MCP process to that profile through the existing scratch hub URL and
token-file overrides. The live check verifies hook and MCP traffic on the
scratch hub. A client that drops the overrides may still contact its configured
hub and cannot pass the scratch check. Pinned wake helpers still read the
OS-account configuration.
[internal/doctor/live.go:161–204](../../internal/doctor/live.go#L161-L204),
[internal/doctor/live.go:254–275](../../internal/doctor/live.go#L254-L275),
[internal/doctor/live.go:319–341](../../internal/doctor/live.go#L319-L341).

### Host rename

A new OS name can retain the same logical ID. Add an optional
client `host_id` setting defaulting to the OS hostname. The hook, MCP resolver,
watch and wake helpers, guard, and doctor use one shared lookup. Pin the old ID
on upgraded installations before renaming the OS, then restart processes and
check the bindings. The same ID preserves existing sessions, Agent mailboxes,
and outside-git and checkout identity. With an older binary, defer the OS rename
until those consumers are upgraded, or retire the old logical host and enroll
the new name with its own token. Drain the old mailbox with the old installation
before retiring it and arrange for senders to use the new Agent names. Use the
same retirement path if an existing logical ID is reserved or malformed.
Retained old mail remains available to the operator. Changing the logical ID
creates a new identity. No rename rewrites the host column on a live token.

### Doctor and owner diagnostics

Add bearer-authenticated `GET /v1/token-scope?host=<id>` for the effective
local host, including outside git. Use ordinary transport checks and a
read-only credential lookup. The probe runs no expiry and changes no token
counters, legacy observations, sessions or reports.

| Probe response | Doctor result |
|---|---|
| `200 {"scope":"bound"}` for a bound bearer matching the supplied host | `bound` for the local ID. |
| `200 {"scope":"legacy"}` for a named legacy or owner bearer admitted to coordination in compatibility mode | `legacy`. Show the migration hint. |
| `401 unauthorized` with the fixed `details.reason` for a host mismatch or required binding. Missing, invalid or revoked bearers get the usual generic 401. | `refused`. Show the credential repair hint. Never echo the expected host. |
| Iris's structured `404 not_found` with message `unknown API route` | `unverified` on an older hub. Keep any legacy connectivity indication separate from scope verification. |
| Invalid host syntax, transport failure or another unexpected response | `unavailable`. Keep local diagnostics available and show a fixed explanation. |

Require a valid host ID on the probe. For a valid ID, authorize the bearer
before returning any scope. A malformed ID on an authenticated request gets
`400 bad_request` with fixed text. An unauthenticated request always gets
the generic 401. Print the local ID and repair hint in interactive doctor
output. Doctor bundles include the allowlisted result only, with no host ID
or raw detail. An older doctor's generic token probe cannot prove host scope.

Bind hook-seen reads, doctor uploads and guard checks to the reporting host.
Bound callers must supply a matching host on these routes. A legacy upload
without a host still goes to `unknown` during compatibility. The named
console operator continues to oversee the fleet and read host reports under
the existing console rules.
[internal/doctor/doctor.go:881–913](../../internal/doctor/doctor.go#L881-L913),
[internal/doctor/doctor.go:1015–1027](../../internal/doctor/doctor.go#L1015-L1027),
[internal/doctor/bundle.go:46–57](../../internal/doctor/bundle.go#L46-L57),
[internal/hub/console_auth.go:147–182](../../internal/hub/console_auth.go#L147-L182),
[internal/store/authorization.go:52–86](../../internal/store/authorization.go#L52-L86).

Extend owner-only `GET /v1/tokens` and `iris token list` with the current
`host_token_mode` (`compatibility` or `enforced`),
`legacy_owner_last_machine_use_ms`, and each label's `host_id` and
`last_machine_use_ms`. Keep existing fields for older callers. Active
labels with a NULL host ID are the migration inventory. The machine-use
timestamps start NULL. Persist the latest successful compatibility-mode
coordination call or accepted watch handshake per legacy label and one
timestamp for the owner in SQLite. Reissuing a revoked label clears its
timestamp. Exclude status, scope probes, token management, owner admin
requests, and time-driven maintenance. These observations survive restart
and contain no requested host, bearer, payload, or per-call history. A quiet
credential may still be configured on an idle machine.
[internal/hub/tokens.go:53–85](../../internal/hub/tokens.go#L53-L85),
[internal/store/tokens.go:42–59](../../internal/store/tokens.go#L42-L59),
[internal/store/tokens.go:68–83](../../internal/store/tokens.go#L68-L83).

## Compatibility

The v0.1.15 tag is available. The v0.1.16 column below describes the intended
ADR 039 stage 1b protocol and is a verification target for its release build.
Ordinary requests from both versions already send the logical host where needed,
or identify an Execution. Binding adds no required request fields or capability
probe to those paths.

| Hub | v0.1.15 client | v0.1.16 client |
|---|---|---|
| v0.1.15 | Existing machine access and v0.1.15 session behavior. | Ordinary requests keep their existing behavior. The exact succession route is unavailable as specified by ADR 039. |
| Stage 1a on main (unreleased) | Ordinary requests keep the stage 1a admission rules. The client does not call `succeed`. | Same ordinary paths. The exact succession route is unavailable. |
| v0.1.16 with ADR 039 | Ordinary requests follow ADR 039's legacy admission rules. | Stage 1b admission, renewal and exact succession apply. |
| ADR 040, compatibility | Existing credentials continue to work. Replacing the token file with a bound bearer enforces own-host requests immediately. | Same token transition. Exact succession is subject to the acting host check. |
| ADR 040, enforced | A matching bound bearer works for the existing protocol. A leftover fleet credential or host mismatch gets an explicit credential refusal. | Same credential rules, including exact succession. |

Both earlier hub versions treat a previously minted named bearer according to
their fleet policy. Host isolation is a property of the hub handling the
request. Keep ADR 039's unsupported-route and local-state rollout rules. In
stage 1b the stage 1a route returns `404 not_found` before any mutation. A host
authorization failure has a distinct response and halts succession.
[ADR 039](https://github.com/mateusz-klatt/iris/pull/437).

## Limits

A compromised host can act as any machine session on its own logical host and
exercise that host's ordinary coordination permissions. A copied bearer still
works from another machine as the enrolled host. Binding limits lateral
impersonation and mailbox access. It provides neither OS-user isolation nor
machine attestation. The fleet agent directory remains visible to a host bearer
through `GET /v1/agents?all=1`. Repository reads without an Execution can reveal
agent worktree paths, branches and claims for known repository IDs, including
outside-git contexts. A host can still register and make claims as itself in a
git repository it can identify through the existing repository rules.
[internal/store/addressing.go:120–134](../../internal/store/addressing.go#L120-L134),
[internal/store/sessions.go:822–835](../../internal/store/sessions.go#L822-L835).

Transitional fleet credentials carry their existing authority until cutover.
Owner-file rotation is an operational step. An unrotated owner copy retains
administrative authority after cutover and can mint bound credentials for any
host. A process that can read the owner file has that same authority, including
under a shared OS account. Hook fail-open preserves the agent session at the
cost of Iris coordination for a refused request.

## Deterministic verification

Use two logical hosts with distinct named credentials, real HTTP handlers and
SQLite stores, a fake clock for lease and ticket boundaries, and barriers for
revoke and reconnect races. Assert domain state and response shape as well as
status. Token authentication counters and bounded refusal observations may
advance independently of domain state.

| Case | Required result |
|---|---|
| Migrate existing and empty databases | Upgrade a v0.1.15 schema and a migration-029 schema, including a versioned database with zero named tokens. Preserve token data, revoked rows and owner access. Reopen in compatibility. Refuse cutover with active NULL rows, revoke them, then reopen in enforced mode. |
| Local setup and wake | Start a fresh enforced setup and verify private, distinct owner and client files, local enrollment before integration and a successful doctor run. Retry after an interrupted install and keep a healthy enrollment and configured host ID. Migrate a shared owner path before replacing the pinned client token. Exercise both wake helpers with a pinned host ID and conflicting environment overrides. Check session proof and wake-state continuity. |
| Mint, rotate and select credentials | Older hubs refuse the new issuance route without creating a token. New hubs reject legacy mint with a repair hint. Overlap labels for the same host and revoke either independently. Rotate the owner secret and reject its previous copy. Point owner commands at a file in a different directory with its own conflicting `client.json`. Assert the original configuration and URL precedence are used. |
| Restore policy | With the flag alone, the database mode alone, and the external marker alone, require enforced startup. Replace the enforced database with a compatibility or pre-migration snapshot, omit the flag and check that the marker still holds the floor. Refuse active NULL rows or active copied tokens on a marked restore before consuming the marker, changing the epoch, or serving. Prepare the copy offline, restore on a new stream, reject all copied tokens and enroll fresh ones. |
| Restore-check and live doctor | Rehearse an enforced snapshot, a compatibility snapshot headed for an enforced destination, and an ineligible sample. Check production blockers separately from recovery, unchanged backup bytes, and generation-floor and watch outcomes with the scratch bound bearers. Run live doctor with its scratch-bound token and check hook and MCP success. Verify the owner bearer stays in the harness and a wrong-host token cannot register. |
| Claimed host and stored host | Cover ordinary and outside registration, bind by session, process and handle, both forms of both shell routes, guard, heartbeat including recovery, and end including repeated end and handover. Exercise matching host, foreign host, omitted required host, and an owned host field paired with a foreign Execution. Refusals leave the session, claims and repository identity untouched. |
| Authorization, discovery and reserved identities | Exercise the principal matrix in each mode, including owner full status and enforced owner 401 on agents, claims, watches, diagnostics and session/mail routes. Check missing principal and unlisted operation refusal. On `agents?all=1`, deny a foreign supplied Execution and allow an owned live one or no selector. Reject reserved or malformed host IDs at enrollment. Reject `client=human` on machine routes and any stored human Execution even on repeated end. Verify normal human mail and console account behavior. |
| Mailbox, claims, streams | Table-test every route in the inventory, including Execution-backed reads, replay keys, receipts and delivery attempts. Deny foreign acting Executions before any read, mutation, cursor advance or ticket rotation. Revoke a label between ticket validation and lock acquisition and verify a sibling label's live socket and ticket survive. Also revoke during admission and on an open socket. Run legitimate cross-host send, receipt and reply. |
| Succession | Carry the two-token pin onto stage 1b's exact handler. Require 404 and zero mutations on the retired stage 1a route. On the exact handler test valid same-host proof, wrong proof on the right host, foreign proof with foreign host, and a foreign replay of an existing child. On refusal assert no child, renewal, event, generation notification or cached reply. Keep the replay no-renewal assertions. |
| Doctor and owner diagnostics | Check matching, legacy, foreign, revoked, missing and malformed probe input plus structured unknown-route and transport failures. Assert no probe writes and no raw host, detail or bearer in the bundle. Confirm only the owner can read the inventory, timestamps survive restart, label replacement clears the per-label timestamp, and status, probes and owner admin traffic leave them unchanged. Check own-host hook observations and doctor uploads and refuse foreign host access. |
| Host rename | Pin the old ID, change the reported OS name, and check that all client entry points keep the same logical identity. Verify retirement and re-enrollment as a new host with old mail retained. |
| Wire and local failure | Run the client/hub combinations above, including the real v0.1.15 binary. Replace the token file during the compatibility rehearsal and exercise the same clients after cutover. For every hook adapter assert bounded valid allow output, a deadline on config lookup and fixed sanitized categories. Refuse initial registration and a later operation after a successful registration. Assert the refused reply supplies no local proof and leaves previously accepted proof intact. Exercise MCP at bind and tool execution, including a resolver already marked lost after restart. Keep the typed 401 and repair hint without fallback or credential disclosure. |

## Effort and sequencing

Implement after owner acceptance. Land ADR 039 stage 1b first so this change can
test the final succession route and error behavior. It needs no ADR 040 schema
or client setting to ship. Budget 3–4 engineering days for migration, issuance,
owner diagnostics and the authorization matrix, 3–4 for setup, the pinned wake
configuration, owner commands and doctor, 2–3 for startup and offline restore
policy and scratch hubs, and 2–3 for verification, release-binary rehearsals
and deployment documentation. The total is 10–14 engineering days. Ship the
compatibility release before requiring any existing installation to cut over.
Update the API and architecture alongside implementation and make owner
activation contingent on the mixed-version matrix passing and the deployment's
credential inventory being migrated.
