# ADR 040 — Bind host tokens to their host

Status: Proposed

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
| Claims, mail, and attention | Claim mutations and Execution-backed discovery, send, inbox and receipt, ack, message receipts, body reads, history, threads, events, watch, and wake tickets use the acting Execution. Existing mailbox rules then check the Agent's participation, recipient, or delivery attempt. [internal/hub/hub.go:83–96](../../internal/hub/hub.go#L83-L96), [internal/store/api.go:44–52](../../internal/store/api.go#L44-L52), [internal/store/api.go:102–148](../../internal/store/api.go#L102-L148), [internal/store/messages.go:189–246](../../internal/store/messages.go#L189-L246), [internal/store/history.go:24–39](../../internal/store/history.go#L24-L39), [internal/hub/hub.go:319–347](../../internal/hub/hub.go#L319-L347), [internal/hub/hub.go:375–439](../../internal/hub/hub.go#L375-L439) |
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
validation and validate host IDs independently. Preserve valid hostname spelling,
including case, and reject empty or malformed IDs at issuance.

Carry the authenticated kind, digest and bound host into the store as a trusted
principal. Check the operation's authority in the same transaction as its lookup or
mutation, before expiry, recovery, cached replies, or other domain work. Check the
current digest and binding in that transaction so a concurrent revoke or credential
replacement has a defined order with the operation. Require an explicit authorization
rule for each machine entry point, including the separately wired watch, wake ticket
and doctor handlers.

- For registration, host searches, guard, and diagnostics, require the supplied
  `host` to match the bound ID. Check the host on both shell registrations and explicit
  bind handles. Check any resolved Execution independently.
- For an Execution-backed request, resolve its actual host from the stored row
  using the operation's real selector, including path IDs and the explicit bind
  handle. Check it even on a recovering or terminal row. Existing same-host
  generation, process, handover, lease, and Agent checks still apply. Authorize the
  acting Execution before any mailbox lookup, receipt, claim or replay. The
  predecessor ID on `succeed` remains proof evaluated within the authorized host.
  Cross-host recipients and repository-wide claim and peer discovery are part of
  coordination. A host credential keeps its current repository-only discovery
  and status visibility. Anonymous status still reports only availability.
- For wake tickets and bearer watches, authorize the Execution before allocating a
  ticket, rotating an existing ticket, taking a monitor slot, reading a cursor or
  upgrading a connection. A ticket carries its issuer's scope and is usable only
  for its original Execution and generation. Apply the scope to continuing stream
  reads and preserve revocation of tickets and sockets.
- Stage 1a's `succeed` checks the authenticated host before proof arbitration.
  Owned requests retain the same create and replay semantics. When ADR 039's
  `succeed/exact` is exposed, apply the same rule to it, including before replay.
  Keep the proof mismatch assertions in
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
warning where supported. The new clients decode the fixed reason. MCP surfaces a
structured tool error with the repair hint. Authorization failure stops binding,
reattachment and succession attempts for the refused request. The refused reply
cannot cause a generation or end-marker write, a fallback registration or an
edit denial. A normal same-host conflict or proof refusal keeps its own behavior.
[docs/architecture.md:80–119](../architecture.md#L80-L119),
[internal/hook/runner.go:379–389](../../internal/hook/runner.go#L379-L389),
[internal/hook/failures.go:275–283](../../internal/hook/failures.go#L275-L283),
[internal/mcpserver/server.go:202–213](../../internal/mcpserver/server.go#L202-L213).

## Migration and operation

1. Add nullable `HostToken.host_id` and a durable hub-wide cutover state. Backfill
   existing named rows, active and revoked, as NULL. Preserve their digest, usage
   and revocation. There is no trustworthy assignment from the label, IP address
   or past requests. The file-backed owner credential has no row to backfill.
   Open existing deployments in compatibility mode. Legacy named bearers and the
   owner bearer retain today's machine access in this mode. Display the mode,
   active unbound labels and recent legacy machine use to the owner. Observations
   have bounded cardinality and contain no bearer or payload content.
2. Mint new tokens with the label and exact host ID as separate inputs on a
   dedicated owner-only route. A new issuance command must get an affirmative
   bound result from that route. Old hubs return an unsupported-route error
   before issuance. On the upgraded hub, the old mint route gives an explicit
   upgrade instruction because its single argument was a label. Keep existing
   list and revoke requests working. The updated list shows host ID and binding
   state without printing secrets.
3. On each client installation, place its new token in the existing user-only
   token file by atomic replacement. If clients on the hub share the owner file,
   configure them to use a separate user-only file before the switch. Keep the
   logical host ID. Check registration or reattachment, a mailbox read and the
   watch reconnect with the new credential, then revoke the old named credential
   when its last consumer has moved. HTTP reads the token file on subsequent
   requests. Reconnect existing watches and restart clients that changed
   configuration or cache it. A bound and a legacy token can coexist during rotation.
   [internal/hubclient/config.go:134–145](../../internal/hubclient/config.go#L134-L145),
   [internal/hub/tokens.go:101–117](../../internal/hub/tokens.go#L101-L117).
4. Once the owner confirms the active token inventory and verifies real traffic
   from every installation, stop the hub, rotate the owner file so distributed
   copies become invalid, and explicitly start it in enforced mode. Persist the
   decision at startup and refuse cutover while any active unbound named token
   remains. Restart drops old tickets and sockets. Future starts honor the
   durable decision. The rotated owner bearer is for owner administration,
   including enrollment and console account recovery. New setups use separate
   files and start enforced.
   An older binary may ignore the new schema and policy. Keep it off the
   migrated database, including during a service rollback. If an older hub is
   required, use the matching pre-migration backup under ADR 006. That restores
   fleet trust and loses changes made after the backup.
   [ADR 006](006-retention-and-restore.md).

**Host rename.** A new OS name can retain the same logical ID. Add an optional
client `host_id` setting defaulting to the OS hostname. The hook, MCP resolver,
watch and wake helpers, guard, and doctor use one shared lookup. Pin the old ID
on upgraded installations before renaming the OS, then restart processes and
check the bindings. The same ID preserves existing sessions, Agent mailboxes,
and outside-git and checkout identity. With an older binary, defer the OS rename
until those consumers are upgraded, or retire the old logical host and enroll
the new name with its own token. Drain the old mailbox with the old installation
before retiring it and arrange for senders to use the new Agent names. Retained
old mail remains available to the operator. Changing the logical ID creates a
new identity. No rename rewrites the host column on a live token.

**Doctor and the operator.** Bind hook-seen reads, doctor uploads, and guard
checks to the reporting host. A bound caller must send that host even to the
doctor upload route. The legacy `unknown` report behavior is available during
compatibility for legacy credentials. New doctor checks the local effective
host against the authenticated scope on a read-only request, including outside
git. Report bound, legacy, refused, or unverified on an older hub, with the
local host ID and repair hint. Keep local diagnostics available on failure.
An older doctor's generic token probe can succeed without proving the host.
The named console operator continues to oversee the fleet, read host reports,
and act through the console account and its repository authorization. Machine
routes cannot register the synthetic human identity or act through its mailbox.
Sending to a human remains an ordinary recipient operation.
[internal/doctor/doctor.go:881–913](../../internal/doctor/doctor.go#L881-L913),
[internal/hub/console_auth.go:147–182](../../internal/hub/console_auth.go#L147-L182),
[internal/store/operator.go:12–26](../../internal/store/operator.go#L12-L26),
[internal/store/authorization.go:52–86](../../internal/store/authorization.go#L52-L86).

**Owner commands.** Use the owner file explicitly for `iris token` management
and networked `iris admin` commands, including forget, merge, unalias, contact
and console account provisioning. Bound machine tokens have no admin or browser
authority. Backup, restore-check, and mark-restored continue to use the hub's
local database access.
[internal/hub/tokens.go:53–85](../../internal/hub/tokens.go#L53-L85),
[internal/hub/owner_accounts.go:18](../../internal/hub/owner_accounts.go#L18),
[cmd/iris/admin.go:87–111](../../cmd/iris/admin.go#L87-L111).

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
machine attestation. The transitional fleet credentials carry their existing
authority until cutover, and the owner secret and operator accounts remain
sensitive. Hook fail-open preserves the agent session at the cost of Iris
coordination for a refused request.

## Deterministic verification

Use two logical hosts with distinct named credentials, real HTTP handlers and
SQLite stores, a fake clock for lease and ticket boundaries, and barriers for
revoke and reconnect races. Assert domain state and response shape as well as
status. Token authentication counters and bounded refusal observations may
advance independently of domain state.

| Case | Required result |
|---|---|
| Migrate existing and empty databases | Upgrade a v0.1.15 schema and a migration-029 schema. Preserve named token data, revoked rows, and owner access. Reopen in compatibility, reject cutover with active NULL rows, revoke them, then reopen in enforced mode. Fresh setup starts enforced. |
| Mint and rotate | Older hubs refuse the new issuance route without creating a token. New hubs reject legacy mint with a repair hint. Labels can overlap during same-host rotation, including issuance before the first registration. Revoking either label affects only that secret. Old owner copies fail. In enforced mode the new owner credential can manage tokens and run admin commands. It is refused on session, mailbox, diagnostics and watch routes. |
| Claimed host and stored host | Cover ordinary and outside registration, bind by session, process and handle, both forms of both shell routes, guard, heartbeat including recovery, and end including repeated end and handover. Exercise matching host, foreign host, omitted required host, and an owned host field paired with a foreign Execution. Refusals leave the session, claims and repository identity untouched. |
| Mailbox, claims, streams | Table-test every route in the inventory, including Execution-backed reads, replay keys, receipts and delivery attempts. Deny foreign acting Executions before any read, mutation, cursor advance or ticket rotation. Check ticket and bearer watches, revoke during admission and on an open socket, and run legitimate cross-host send, receipt, reply and human mail. |
| Succession | Exercise the stage 1a test pin on the exposed stage 1a handler, and the same authority cases on stage 1b's exact handler. Test valid same-host proof, wrong proof on the right host, foreign proof with foreign host, and a foreign replay of an existing child. On refusal assert no child, renewal, event, generation notification or cached reply. Keep exact replay's existing no-renewal assertions. |
| Doctor, operator, and rename | A host can read its hook observations and upload only its own report. Assert safe diagnostics on refusal and a console account's current grants. Machine credentials cannot register the human identity or use browser/owner routes. Pin the old ID, change the reported OS name, and check that all client entry points keep the same logical identity. Verify retirement and re-enrollment as a new host with old mail retained. |
| Wire and local failure | Run the client/hub combinations above, including the real v0.1.15 binary. Replace the token file during the compatibility rehearsal and exercise the same clients after cutover. For every hook adapter assert bounded valid allow output and sanitized reporting. Refuse initial registration and a later operation after a successful registration. Assert that the refused reply supplies no local proof and leaves previously accepted proof intact. Exercise MCP at bind and tool execution, and assert an actionable structured error with no fallback, success output, or credential disclosure. |

## Effort and sequencing

Implement after owner acceptance. Land ADR 039 stage 1b first so this change can
test the final succession route and error behavior. It needs no ADR 040 schema
or client setting to ship. Then budget 2–3 engineering days for migration,
issuance and the machine authorization matrix, 2–3 for setup, doctor, client
identity and owner cutover, and 2–3 for the deterministic tests, release-binary
rehearsals and deployment documentation. Ship the compatibility release before
requiring any existing installation to cut over. Update the API and architecture
alongside implementation and make owner activation contingent on the mixed-version
matrix passing and the deployment's credential inventory being migrated.
