# 014: Named operator accounts for the console

Status: accepted.

## Problem and scope

R7 gives a person one console for sessions, mail and claims. Today that person is
anonymous to Iris: a bearer-authenticated request creates a one-minute login link,
which creates a twelve-hour cookie held in memory. There is no logout, account,
profile or password. A restart loses browser sessions. Both owner and host tokens
can currently create links; revoking a token does not revoke its earlier links or
cookies. D13 and ADR 002 describe this deliberately broad trust model.

The owner wants named console accounts, logout, profile, password changes and
permissions. This decision extends D13's browser authentication and D16's console;
it keeps D1's single Go hub, D9's transactional store, R8's hook guarantees and all
section 5 invariants. It does not introduce multi-tenant hosting or change machine
token authority. The owner accepted the four decision groups, with the member
model amended to explicit per-repository `ro`/`rw` grants. Implementation requires
later PRs; this ADR implements no feature.

The UI transfer source is the owner's exported React/TypeScript/Vite code in
`~/iris-ui-import/ui`, inspected only inside that directory. The destination is
`web/`. Adapt selected views and styles to Iris's contracts. The supplied account
table and hash format are an import contract, not a database inspected for this
decision. No source database, source server code or production system was opened.

## Verified starting points

Checked against commit `72308ccfa3aa574e207208c5409757864ce538ff`:

| Code | Consequence for this decision |
|---|---|
| [`internal/hub/operator.go`](../../internal/hub/operator.go), `uiLink`, `uiLogin`, `cookieAuthorized` | Random link/cookie secrets live in maps. Cookies use HttpOnly, SameSite=Strict and Secure for TLS/public mode. Cookie authorization returns a boolean, without an account principal. |
| [`internal/hub/hub.go`](../../internal/hub/hub.go), `ServeHTTP`; [`internal/hub/tokens.go`](../../internal/hub/tokens.go), `ownerOnly` | Host/Origin checks, CSP, no-store and body bounds already exist. Any valid bearer can reach link creation. Token administration and repository contact/merge/forget require the owner token. |
| [`internal/hub/limits.go`](../../internal/hub/limits.go) | Cookies share the aggregate anonymous quota. Existing bearer/request limits are not password-attempt limits. |
| [`internal/store/operator.go`](../../internal/store/operator.go), `human`, `operatorSender`, `readThread` | Human mailboxes are shared per repository, including preserved mailboxes after repair. Reading through the POST read action acknowledges human deliveries. Accounts must not be confused with agent Executions. |
| Same file, `overview`, `visibleIn`, `threads`, `thread`, `ui_events` | Overview and refresh cursors are global; an empty repository filter means all messages. Search can match another message in a thread, and thread detail returns all recipients. These paths need authorization-aware queries, not frontend filtering. |
| [`internal/store/messages.go`](../../internal/store/messages.go), [`internal/store/history.go`](../../internal/store/history.go), [`internal/store/addressing.go`](../../internal/store/addressing.go) | Agent projections reduce human senders to `human`; addresses resolve across repositories. Account authorship needs message metadata and projection changes, and member sends need explicit origin/recipient authorization. |
| `operatorCached` in `internal/store/operator.go`; [`internal/store/merge.go`](../../internal/store/merge.go), [`internal/store/operator_retry_merge_test.go`](../../internal/store/operator_retry_merge_test.go) | Retries are currently shared-mailbox scoped and checked before recipient resolution. Repair moves messages/agents while preserving mailbox IDs, so new account-scoped retries must survive repair without bypassing current permissions. |
| [`internal/store/store.go`](../../internal/store/store.go); [`internal/store/api.go`](../../internal/store/api.go) | The store serializes one connection and `BEGIN IMMEDIATE` transactions using `modernc.org/sqlite`; migration version is 16. Ordinary `Store.Call` expires coordination records before dispatch, so it is unsuitable for a no-write import dry-run. |
| [`web/src/App.tsx`](../../web/src/App.tsx), [`web/src/api.ts`](../../web/src/api.ts), [`web/src/preferences.ts`](../../web/src/preferences.ts) | Existing navigation, ID-based recipients, reviewed/idempotent sends and WebSocket refresh must survive adaptation. UI locale currently lives in browser storage. |

## Decisions

### 1. Accounts are separate from machine credentials and human mailboxes

The import ledger parts are superseded by [ADR 020](020-named-login-only.md); PR 2 is not yet implemented.

Add Iris-owned `ConsoleAccount`, `ConsoleRepositoryAccess`, `ConsoleSession` and
`ConsoleImport` records, plus `ConsoleOperation` for account-scoped send retries,
through the store's migration mechanism. These are intended, not existing tables.
Accounts have fresh Iris IDs, username and unique login key, password hash, role,
disabled flag, display name, two locale preferences, created/last-login times,
an authentication version and an edit revision. A grant
relates an account to an existing Iris Repository ID with exactly one mode, `ro`
or `rw`; absence grants nothing. `rw` includes `ro` and message send/reply only.
The import ledger relates a caller-selected stable source identifier and source
row ID to the Iris account. Source numeric IDs are never Iris identities.

Human mail remains a shared operational inbox. Named accounts do not create private
mailboxes, active agent Executions or personal unread state. Both operators and
members send through the existing human Agent and ended synthetic operator
Execution of the selected sending repository. Only operators acknowledge shared
deliveries. This transport identity does not identify the author by itself.

Persist immutable `sender_account_id` plus at-send username/display-name snapshots
on every new account-authenticated console message, taken from the server principal,
never request fields. Show the person alongside the shared human transport sender
in console threads/participants, send results, delivery/receipt views and the agent
inbox/history/body projections and MCP wrappers. Receipts still describe the actual
recipient delivery and ACK/handled state; a member sending a reply does not set any
ACK or handled receipt. The current projection of every human sender as `human`
must not hide the new account authorship. Do not expose role, locale or other
profile fields as sender metadata. Old messages remain unattributed shared human
mail. Keep account IDs and at-send attribution after profile edits, disabling,
demotion or repository repair; display names never determine authority.

Record the authenticated account ID with reads, claim revocations and security/
account changes in transactional console action records as well. Action logs alone
are insufficient for message attribution. “My messages” must explain that it is
shared operator mail; members see “Messages”, without a personal-inbox claim.
Authorship does not make a shared message private or grant lasting access to it.
Console send accepts an account session only, including for operators; a machine
bearer cannot stand in for a person or supply account attribution. Machine sends
continue through the existing agent APIs with their existing authority and retry
namespaces, without asserting a named console author.

Use a deterministic login key: Unicode NFC, Unicode case folding, then NFC again,
with the chosen Unicode behavior fixed by Iris's pinned Go dependencies. Preserve
the username for display. Reject empty names, invalid UTF-8, control characters,
leading/trailing whitespace and names over 64 Unicode code points. Apply the same
rules at import, creation and login. Login-key collisions require explicit reviewed
renames; never select the first matching account. Username changes are outside the
first console release. Display names are bounded to 128 code points and rendered
as text. Role/permission data never comes from profile updates.

### 2. Two useful roles, with per-repository ro/rw and default denial

Imported role and grant mappings are superseded by [ADR 020](020-named-login-only.md); PR 2 is not yet implemented.

| Source/global role | Iris role and console authority |
|---|---|
| `admin` | `operator`: all repositories; read, send/reply, acknowledge human mail, revoke claims, view host diagnostics and manage console account roles/disabled state/`ro`/`rw` grants. This does not confer owner-token powers. |
| `member` | `member`: no repository access by default. `ro` grants the read scope below; `rw` adds send/reply under the checks below. May edit its own profile/password and log out, including with no grants. |
| Anything else | `none`, forced disabled, no login or protected access. Import reports the quarantined row; later explicit operator review is required to enable a recognized role. No fallback to member or operator. |

Disabled always wins over a role or grant. Unknown stored roles and malformed
permission records also deny access. Only an enabled operator manages access; the
last enabled operator cannot be disabled or demoted through the console. Use an
edit revision to refuse stale access updates. Account creation and password
recovery are owner-authenticated CLI operations in the first release, without
passwords in argv, logs or URLs. No public registration, email reset or invitation
workflow is included. Recovery creates/resets a named operator, never an anonymous
browser session, and invalidates that account's sessions.

The supplied account table contains no project assignments. Imported members start
with no `ro` or `rw` grants. If a later transfer supplies assignments, every source
project must have an owner-reviewed mapping to one existing Iris Repository ID;
never infer it from a name, slug, remote, path or numeric ID. A source `viewer`
assignment may become an `ro` grant for a member through that explicit mapping.
A source project `operator` assignment is refused: it must not promote a member
to global Iris operator or silently lose its write meaning. Unknown assignment
roles, unmapped/ambiguous projects and assignments that purport to restrict a
global operator are refused. Removing or downgrading such assignments requires an
explicitly revised manifest before apply. An account-only import does not claim
to have transferred project permissions.

Adding Iris `rw` does not reinterpret source project `operator` as equivalent:
its source capabilities include unsupported functions. Keep that import refusal;
an Iris operator can grant `rw` explicitly after account import and review.

`ro` reads repository names, aliases, agents, sessions, claims, message lists/
details, receipts and delivery state in that repository. `rw` includes exactly
those reads and the ability to send messages and reply to agents. Neither mode
permits acknowledging/marking shared mail read, creating/releasing/revoking claims,
managing accounts or grants, changing repository contacts, merging/forgetting
repositories, token management, hub administration or uploading diagnostics.
Opening a thread uses GET only: the existing POST read action is a mutation even
if the UI calls it automatically. Host doctor reports are deployment-wide and can
contain paths, so members cannot read them. Members cannot list other accounts or
their profiles; visible message authorship is message metadata, not profile access.

Every member send supplies an explicit sending `repository_id` with current `rw`;
do not infer it from a reply or the first recipient as the operator path does today.
Recipients must be explicit immutable non-human Agent IDs. Resolve their current
repositories and require `rw` on every one in the same transaction as the send.
Reject human/repository shorthand addresses for member sends; no contact fallback,
group expansion or name reuse may bypass those checks. A mixed-recipient batch
fails atomically if any recipient is unknown, inaccessible or has only `ro`.
Cross-repository sends are allowed only with `rw` on the sending repository and
every recipient repository. A readable `ro` destination is still not writable.
A reply parent must be readable in the selected sending repository, not merely
share a thread ID. Keep existing message/body/recipient bounds, reviewed sends,
idempotency and bounded send quotas; use account identity for console quotas.

For members, apply the grant predicate before search, grouping, pagination and
counts. An omitted repository means the union of allowed repositories, never the
whole hub. Explicit inaccessible repository/object IDs return `not_found` without
disclosing existence; disallowed operations return `forbidden`. A cross-repository
message is readable when its sender or at least one delivery is in an allowed
repository, following ADR 004's per-message visibility. Show its body and sender
name as the received message envelope, but omit outside-repository recipient
names/states and outside repository IDs, aliases and session metadata. Replies in
other repositories do not make an entire thread visible. Hidden messages cannot
match a search, supply a title/participant, contribute counts, or affect pagination.

Agent replies remain addressed to the sending repository's shared human mailbox,
not to a private account. Supply an explicit `reply_address` such as
`human:<repository id>` in the agent envelope; bare `human` would name the agent's
own repository during a cross-repository exchange. The reply's sender is the agent,
and its parent/thread link preserves the account-authored message. Recipients stay
explicit: neither `reply_to` nor `reply_address` implicitly delivers a message or
compels an agent to reply. All current `ro`/`rw` readers of the sending or receiving
repository see only the messages
visible there, regardless of which person sent the original. For example, a member
with `rw` on A and B can send from A to an agent in B; a reply from B delivered to
A is readable with `ro` on A, with outside metadata redacted as above. Later replies
in C are not visible unless their own sender or delivery reaches an allowed
repository. Losing all relevant grants hides even one's own earlier messages and
receipts. Thread linkage and original authorship never create a continuing grant.

All named console sends, including operator sends, use `ConsoleOperation` keyed by
`(account ID, retry key)` across the hub, independent of cookie/session, shared
human Agent and repository ID. Its fingerprint covers the operation and original
request, including a member's explicit sending repository, recipient IDs and reply
parent. Preserve the accepted resolved origin as well when an operator's request
uses the existing origin inference. The same key used by two accounts is
independent; a different operation/request
under one account's key returns `key_reused` (409). Store the accepted message/
delivery IDs, resolved recipient IDs and result atomically with the message and
authorship. Do not use the shared-mailbox `Operation` cache to answer named-account
retries or attribute its historical entries to a person. Machine retries retain
their existing namespaces.

Authenticate and check current permissions before returning any cached send result.
For an exact replay, authorize the persisted message's current sending repository
and each persisted recipient's current repository, including the member's
non-human-recipient restriction, then return the accepted result without
re-resolving addresses or creating another message. Missing/forgotten
objects fail closed; retain a retry tombstone if message/recipient deletion removes
the accepted result. Keep console operations/tombstones for 24 hours from creation,
following ADR 006, and keep client retry keys for at most ten minutes. Within that
window deletion cannot make an accepted key a fresh send. A reply parent originally
authorized at commit does not need to exist forever to replay that result, but its
absence cannot authorize a new reply. A member now having only `ro` cannot replay
a send result through the mutation endpoint; it may use the scoped read/receipt
API. Other accounts never retrieve that person's cached result through their key.

Every protected HTTP request gets a typed account or machine principal. Recheck
session, disabled state, current authentication version, role, operation and object
scope inside the store transaction that reads or mutates the object. Browser input
never selects the acting account or its grants. Reject requests mixing bearer and
console credentials rather than accidentally choosing stronger authority. Account
cookies authorize only the console API; they never authorize agent APIs, token
management, repository contact/merge/forget or doctor report upload. Preserve the
machine-only doctor PUT route. Existing machine tokens remain broad D13 principals.

Member live updates must use only authorized repository changes and a per-account
console revision, not the global Event sequence or heartbeat. Otherwise even an
empty frame leaks other repository activity. Revalidate the account before each
frame and close active sockets on expiry, logout, disabling or an authentication
version change. Serialize authorization and frame admission with revocation; a
frame admitted before revocation may already be in transit. Account access changes
also wake that account's sockets to close them when no repository event occurs.

Repository merge refuses while either repository has any member `ro` or `rw` grants:
the owner must review and remove those grants before repair, then grant the merged
repository explicitly. Never silently union permissions and expose another history.
Preserve account authorship, message/delivery IDs and account retry namespaces
when moving shared mailboxes and messages; never coalesce people or retry keys.
Exact retries after repair may locate a saved operation via its original request,
including a retired repository ID, but authorization uses the moved message and
recipients' current repositories and, for members, explicit post-merge `rw` grants.
A retired ID cannot originate a new send. Reply addresses on refreshed messages reflect the
current repository; an old retired address requires refresh, not implicit regrant.
Forget removes affected grants and increments affected accounts' authentication
versions in the same transaction. The existing all-or-nothing repair/forget dry-run
and delivery guarantees remain requirements.

### 3. Persistent opaque sessions, with a fresh secret

Generate a new independent 256-bit console session key in a user-only hub file at
cutover. It is neither the owner/host token nor a source application key. Protect
it with Iris's mode 0600/owner-only Windows DACL handling and persist it across hub
restarts. Failure to load it fails console authentication closed without stopping
agent coordination. No source cookies, links, signing keys, session IDs or CSRF
secrets are imported or accepted.

On successful login, generate a fresh 256-bit random opaque cookie value. Persist
only a domain-separated HMAC-SHA-256 digest under the console key, account ID,
authentication version, creation and absolute expiry times in `ConsoleSession`.
The browser receives the raw token once. A signed, self-contained role cookie or
JWT is unnecessary: server-side records give immediate revocation. No raw token,
password hash or key is returned by account/session inspection. Session validity
is independent of the agent boot epoch and survives a restart without extending
the original twelve-hour expiry. Use server UTC time; client timestamps cannot
extend it. Wall-clock rollback is a remaining limit, requiring a trustworthy hub
clock. Bound active sessions per account and prune expired records.

Logout deletes the current session and clears its cookie, and is idempotent.
For an active session it requires the session's CSRF token. A same-origin retry
with an already absent/expired/revoked session succeeds by clearing the stale
cookie only, returning no account data and changing no server state.
Password change/reset, role/grant changes and disabling atomically increment the
account authentication version and delete all its sessions, including the caller's
when that account is the target. Changing `rw` to `ro` is a permission change:
after its transaction commits, the next request on the old session is
`unauthorized`, including an already prepared send/reply or cached retry. After
signing in again, `ro` reads work but sends/replies return `forbidden`. Check grants
inside the write transaction, so a write serialized before downgrade may commit,
but none serialized after downgrade can use old `rw` or a cached capability.
The next action requires login; close its established sockets as above. Cosmetic
profile/locale edits do not revoke sessions. Password verification/derivation runs
outside the serialized store lock; commit rechecks the password/authentication
version and disabled state before inserting a session or changing a password.
Concurrent reset/login and permission/write races must not restore old authority.

Keep an HttpOnly, host-only cookie with Path=/ and SameSite=Strict. Secure is
mandatory for production/public HTTPS; the existing explicit literal-loopback HTTP
development mode is the only exception. Keep ADR 002's exact public origin and
trusted loopback proxy rules; do not trust arbitrary forwarding headers or enable
CORS. Preserve Host/Origin validation, no-store, CSP and bounded request bodies.
Key rotation invalidates all console sessions. Backup/restore instructions must
distinguish normal restarts from restore: rotate the console key and clear sessions
after restoration. A database backup contains password hashes and old permissions;
restoring it can restore both, requiring owner review before reopening the console.

### 4. Password formats and bounded verification

Scrypt acceptance and upgrade are superseded by [ADR 020](020-named-login-only.md); PR 3 is not yet implemented.

The accepted imported format is exactly
`scrypt$n$r$p$salt_base64$hash_base64`, without a leading dollar sign. The supplied
profile is `n=16384`, `r=8`, `p=1`, a 16-byte salt and a 32-byte derived key, both
URL-safe base64 without padding. Parse all six fields from each record; do not
replace its parameters with new-password defaults or hash the encoded hash again.

Before invoking `golang.org/x/crypto/scrypt`, bound the encoded record to 256 bytes,
validate decimal integers without signs/overflow, require a power-of-two `n > 1`
and positive `r,p`, validate canonical unpadded URL-safe base64 and decoded lengths,
and check memory/work products with overflow-safe arithmetic. The initial importer
and verifier accept only the supplied `(16384,8,1)` profile: a 16 MiB principal
scrypt memory cost (`128*n*r`) and work product `n*r*p=131072`. Other profiles fail
closed pending an explicit reviewed extension; parameters are still parsed and
validated from the record. Bound implementation overhead as well as this principal
allocation. Compare exactly 32 derived bytes using `crypto/subtle.ConstantTimeCompare`.
Malformed/unsupported records produce no session and never a panic or large allocation.
These constraints supplement the [Go scrypt API's parameter validation](https://pkg.go.dev/golang.org/x/crypto/scrypt#Key)
and [constant-time comparison contract](https://pkg.go.dev/crypto/subtle#ConstantTimeCompare).

New and changed passwords use Argon2id version 19 in PHC encoding:
`$argon2id$v=19$m=65536,t=3,p=1$<salt>$<hash>`, with a fresh 16-byte random salt,
32-byte key and standard unpadded base64. This is Iris's initial profile,
not an imported scrypt default. Use `golang.org/x/crypto/argon2.IDKey`; its
[Go documentation](https://pkg.go.dev/golang.org/x/crypto/argon2#IDKey) describes
the memory/time/parallelism inputs. Accept only recognized, bounded profiles.
Pin/review the pure-Go crypto dependency in its implementation PR and verify it
with `CGO_ENABLED=0`; no C library, source backend or new runtime service is needed.
Benchmark the 64 MiB profile on the supported hub platforms before enablement.

Retain imported scrypt hashes byte-for-byte until a successful login can upgrade
one using the submitted password. Perform that replacement with an authentication
version recheck, invalidate prior sessions and issue only the new login's session
afterward in the same transaction. If an upgrade cannot complete, fail login and
leave the old hash unchanged. Explicit password change verifies the current
password and requires login again after success. New passwords require at least
15 Unicode code points and at most 1024 UTF-8 bytes; imported passwords are not
retroactively subjected to the new minimum. Do not trim, normalize or silently
truncate passwords. Apply the input maximum before KDF work.

### 5. Browser defenses and API boundaries

Require an exact same-origin Origin and `X-Iris-CSRF` header on every browser
mutation, including login, logout, password, profile, read/ack, send, access edits
and revoke. A session-bound synchronizer token is returned by a no-store session
GET and held in memory by the UI; derive it with a separate HMAC domain from the
session cookie and console key. It is not an authentication credential and never
goes in a URL or browser persistent storage. Before login, a short-lived, bounded,
single-use server challenge and HttpOnly pre-login cookie bind an equivalent token
to that browser. Consume it on a login attempt; the UI obtains another for retry.
GET does not sign in, acknowledge mail, send or revoke on the user's behalf;
existing hub expiry bookkeeping remains independent of those actions. SameSite and
Origin are additional defenses, not replacements for CSRF validation. WebSocket upgrades
require exact origin plus a valid account session.

Add password-attempt limits before any KDF: initially five attempts per normalized
login key per fifteen minutes, thirty aggregate attempts per minute, and at most
two concurrent KDF jobs across login/change/reset/upgrade, without an unbounded
queue. Use bounded expiring buckets, keyed by digests rather than raw attempted
names. Unknown names consume the same quotas. Return `rate_limited` with
Retry-After. These are initial tunable defaults; they can deny legitimate users
under attack and reset on restart. Do not derive identity from untrusted forwarded
addresses or put expensive KDF work on the hook/API coordination lock.

Return the same HTTP status and “Invalid username or password” for missing users,
bad passwords, disabled accounts, role `none` and unusable stored hashes. Perform
bounded dummy work for rejected identities after rate admission to reduce obvious
timing differences; heterogeneous hash profiles still prevent a promise of identical
end-to-end timing. No account-existence hints, source hash values or parse errors
reach the browser. Account/session/security logs contain actor/target IDs, action,
time and outcome, never passwords, hashes, salts, session/CSRF values, request bodies
or login URL queries. This also applies to import reports and tracing.

Intended Iris routes, separate from the exported wire contract:

| Route | Authority |
|---|---|
| `GET /v1/ui/login`, `POST /v1/ui/login` | Pre-login challenge; then bounded credentials + CSRF verification. |
| `GET /v1/ui/session`, `POST /v1/ui/logout` | Current account/session; logout requires CSRF. |
| `PATCH /v1/ui/profile`, `POST /v1/ui/password` | Enabled account, self only; revision/current password as applicable. |
| `GET /v1/ui/accounts`, `PUT /v1/ui/accounts/{id}/access` | Operator only; mode is no grant, `ro` or `rw`; update requires expected edit revision and CSRF and revokes target sessions. |
| Existing overview/threads/deliveries/watch; `GET /v1/ui/messages/{id}/status` | Operator globally, member under current `ro`/`rw` read policy; receipts include account attribution and scoped recipient state. |
| `POST /v1/ui/messages` | Account session only: operator globally; member with current `rw` on explicit sending repository and every recipient repository, and a readable reply parent there; CSRF, account authorship and per-account retries. Machine bearers cannot call this route. |
| Existing thread read/claim revoke | Operator only, with CSRF and object checks; denied to both member modes. |
| Agent, token and admin APIs | No console-cookie authority, including with `rw`; existing machine/owner checks remain. |
| Existing doctor GET / PUT | Operator for GET; machine bearer for PUT. |

Server responses expose current capabilities so the UI can hide unusable controls,
but the backend enforces every request. Keep Iris's existing error vocabulary such
as `unauthorized`, `forbidden`, `not_found`, `conflict` and `rate_limited`.

### 6. Replace the login link at cutover without weakening D13

The ordinary-origin `iris ui` command has been removed under
[ADR 020](020-named-login-only.md).

Stage schema and tests first. Before activating named authentication, the owner
provisions/imports and verifies at least one enabled named operator through an
owner-authenticated setup/recovery path. Then cut over the console atomically:
retire `POST /v1/ui/links` and `GET /ui/login?code=...`, discard the old maps/cookie
namespace, and make `iris ui` open the ordinary console sign-in page. Neither owner
nor host bearer tokens mint browser sessions. Old browser cookies and links cannot
enter the account console. Do not run a permanent anonymous-operator bypass beside
the new permission checks. Setup/recovery remains available to the owner through
the CLI; host tokens cannot create/reset accounts or invoke the importer.

Owner-only token administration, repository repair/deletion/contact and machine
API authority remain as D13 specifies. This adds a boundary for browser-only users
who do not possess a machine credential. Any process under a host's OS user can
still read that host's token and act broadly through machine APIs; a named account
cannot contain that process or a stolen bearer token. The owner token and hub OS
administrator remain trusted. This is unsuitable for mutually hostile tenants.
No MFA, SSO, defense against a compromised browser/hub, or stronger edit enforcement
is promised. Claims stay advisory outside mediated actions; account and message
text remains untrusted data and never chooses authority, execution or priority.

### 7. A separate, transactional import tool

The importer is superseded by [ADR 020](020-named-login-only.md); PR 2 is not yet implemented.

Implement later in Go as an owner-authenticated CLI tool, with a source SQLite
connection opened read-only and a consistent snapshot. The hub alone writes its
database, through a dedicated import store transaction; the importer never writes
the hub file behind `iris serve`. Do not run or connect the source backend. Accept
only the declared table columns: `id`, `username`, `password_hash`, `role`,
`disabled`, `display_name`, `preferred_ui_locale`,
`preferred_correspondence_locale`, `created_ts`, `last_login_ts`. Confirm the source
table name and timestamp encoding/unit/time zone when building the tool; do not
guess them from values. A nullable last login stays null. Source times are history,
not session expiry or authorization inputs.

Require a stable source identifier, bounded account batch and explicit manifest
for renames/locale resolutions and any separately supplied project mappings.
Validate rows, booleans, hash format/cost, roles, names, locales and timestamps.
Report unknown roles as disabled `none` accounts; malformed hashes/rows, unresolved
login collisions and unsupported assignments refuse the entire apply. Locale values
must map to the existing supported Iris catalog or explicitly to unset; never
silently reinterpret unsupported data. Import locale preferences as data, not
instructions to agents. The account-table import always creates members with no
`ro` or `rw` grants, including disabled members; importing `member` never implies
repository access. Any later assignment transfer is separate and follows section
2's explicit viewer-to-`ro` mapping/refusal policy, not an inferred `rw` conversion.

Dry-run follows a dedicated no-write validation path, without `Store.Call`'s expiry
bookkeeping, session creation, timestamp updates or ledger writes. It reports safe
row IDs, counts, collisions, quarantined roles and proposed mappings, never hashes
or hash-containing payloads. It is a snapshot; apply repeats all validation and
collision checks inside one `BEGIN IMMEDIATE` transaction. Accounts and import-ledger
entries commit together, or none do; a separately authorized assignment transfer
must likewise commit its explicit grants and ledger atomically. Database uniqueness constraints
remain the final race check. See [SQLite transaction semantics](https://www.sqlite.org/lang_transaction.html).

Never overwrite an existing Iris account, password, profile, role or grant. The
ledger's unique `(source identifier, source row ID)` records a versioned SHA-256
fingerprint of the canonical original validated source row and its manifest
mappings. The fingerprint is independent of the rotating console session key.
An exact re-run skips that row even if
the account subsequently changed its password/profile in Iris; it cannot restore
old data or re-enable it. Changed input for a recorded source row, a different
source with the same login key, or a mapping collision fails apply and requires
explicit review, not an upsert. Fingerprints remain internal and are not logged.
On failure report sanitized IDs/reasons, roll back the batch, and allow safe retry.
Import no existing sessions or secrets. Take an Iris backup before a real apply;
rollback of a committed import is an owner recovery operation, not blind deletion
of accounts that may already have been used.

## Alternatives and consequences

| Alternative | Why this decision chooses otherwise |
|---|---|
| Keep the single login link, adding profile controls | It cannot attribute people, persist logout/revocation or give members a distinct scope. Retaining it as a bypass defeats permissions. |
| Give every member hub-wide read access | Simpler queries, but exposes every team's bodies, paths and diagnostics. Explicit repository grants are more work and deny by default. |
| Give members repository administration with write access | `rw` is narrowly message send/reply. Shared ACKs, claims, grants and administration retain their separate operator/owner authority. |
| Give all shared-human sends the same retry namespace and author | Two people can collide or receive another person's cached result. Immutable account authorship and per-account retry records distinguish people while retaining shared replies. |
| Reuse source project roles and server contracts | Project `operator` means writes there, and projects have identities/lifecycle Iris does not have. Explicit viewer mapping and refusal avoid accidental promotion. |
| Signed role cookies/JWTs or an in-memory session cache as authority | They complicate immediate permission/password revocation or lose sessions on restart. Database sessions fit the existing single writer. |
| Retain scrypt for all new passwords, or force resets at import | A single format is simpler, but bounded compatibility plus Argon2id separates preservation from new-password policy without requiring every imported user to reset first. |
| External identity service/SSO | Useful later, but adds deployment/runtime scope beyond the single-binary small-team requirements. |
| Per-account personal mailboxes | Useful for private conversations, but changes addresses, delivery/ACK semantics and repository repair. Shared mail with immutable message authorship meets this decision without pretending to provide private correspondence. |

This work adds credential-bearing database records, a new protected key file,
scope-aware queries and revocation races to maintain. Hashing can consume scarce
memory/CPU; throttles trade availability for bounded cost. The browser boundary is
meaningful only while machine credentials stay with trusted users. These costs
are reasons to stage verification before moving the exported UI.

## Ordered PR and transfer plan

Each PR depends on its predecessors. None is implemented by this ADR. Implementing
PRs update D13, D16 and the API/deployment documentation to this accepted decision;
until then the verified starting behavior above remains authoritative.

1. **Storage, roles and password contracts.** Add account/grant/session/import/
   console-operation migrations and immutable message-authorship fields using the
   next schema number available then; add strict unique `ro`/`rw` grants,
   version/revision handling and bounded hash parsers/verification/creation.
   Keep routes inactive. Bind console retry keys to accounts, with 24-hour
   retention/tombstones and no attribution of legacy shared-cache entries.
   Prove scrypt compatibility with synthetic vectors for the exact supplied format,
   malformed/overflow/over-cost/base64 cases, fixed-length comparisons, role default
   denial, default no-grant access, malformed grant modes, uniqueness,
   transaction rollback and pure-Go builds. Benchmark KDF cost outside the store
   lock and set the final bounded profiles/concurrency.
2. **Authorization and lifecycle.** Add typed principals, the operation matrix and
   transactional repository predicates. Cover cross-repository envelopes, hidden
   search matches/titles/counts/participants/pagination, missing filters, guessed
   IDs, both member modes' ACK/admin/claim denial, mixed credentials and owner/host
   boundaries. Add send/reply with explicit origin `rw`, all-recipient `rw` checks,
   visible reply parents and atomic batch refusal. Test origin/recipient grants
   absent or `ro`, a cross-repository target lacking `rw`, hidden parents and
   shorthand-address/forged-author bypass attempts. Expose immutable person
   attribution in every console/agent/MCP message and receipt projection while
   retaining shared human reply routing. Test two accounts using the same retry
   key, one account changing its request, restart/relogin retries, per-account
   send quotas and no duplicate deliveries. Verify reply visibility under changing
   grants and no author-only access exemption. Cover profile changes, demotion,
   disable and merge retaining authorship; bearer-only console-send rejection;
   merge refusal for either grant mode, lost-response retries after removal/merge/
   regrant, changed recipients, deletion
   tombstones, and retired IDs never enabling a new send. Add scoped live revisions
   and prove no hidden activity leaks. Keep existing machine retry semantics.
3. **Sessions and authentication defenses.** Add persistent hashed sessions, fresh
   key handling, challenge/CSRF, password-attempt limits, logout and password change,
   plus owner provisioning/recovery. Keep account-mode routes gated until the full
   authorization path is ready. Prove restart persistence/absolute expiry, disabled
   refusal, generic login failure, key rotation, all-session revocation including
   live sockets, KDF admission and reset/login/write/frame races with real SQLite
   concurrency. Include `rw` to `ro` or no-grant downgrade versus send/reply/cached
   retries: old sessions fail on the next request, reauthenticated `ro` accounts
   can read but not send, and no post-downgrade write uses earlier authority.
   Cover literal-loopback and ADR 002 proxy-origin paths.
4. **Importer.** Add the read-only source adapter, dedicated hub apply/dry-run and
   reviewed mapping manifest. Test synthetic SQLite inputs, never a production
   source: collision after dry-run, duplicate source IDs, unknown roles, malformed
   hashes, unsupported assignments/locales/times, rollback at each persistence
   boundary, exact and changed re-runs, changed target passwords, concurrent apply,
   zero grants for imported members, viewer-to-`ro` mapping only in a separate
   reviewed transfer, source project operator refusal despite Iris `rw`, and
   absence of sensitive values in logs. Prove dry-run changes no Iris record,
   including coordination expiry state. Publish backup/apply/recovery instructions.
5. **Login, logout and account UI transfer into `web/`.** Add an Iris login view
   from the requirements: no login implementation exists inside the permitted
   export. Adapt the exported account form, logout control, profile/password
   feedback and language interactions listed below to the Iris routes/CSRF rules.
   Replace the export's JavaScript string-length/password input limits with the
   specified code-point minimum and UTF-8-byte maximum; test multibyte and non-BMP
   characters as well as ASCII.
   Preserve the existing console shell, recipient IDs, drafts, review, retry keys,
   safe Markdown, accessibility and responsive behavior. Member compose/reply
   appears only for selected `rw` repositories; recipients are restricted to
   current `rw` repositories and the reply parent must be visible in the chosen
   origin. Show the authenticated person on shared messages and explain shared
   replies. Both member modes omit revoke/diagnostics/admin controls and never
   call the read/ACK action. Test `ro`-only navigation, mixed `ro`/`rw` compose,
   cross-repository replies and permission changes while a draft is open.
   A 401 or revocation closes streams and clears prior-account data, drafts and
   cached locale; another account on the same browser sees none of them.
6. **Access UI transfer and cutover.** Adapt the account-list/conflict presentation
   to Iris operator/member/none roles, disabled state and repository no-grant/`ro`/
   `rw` controls; implement the missing controls against the tested backend, including revision
   conflicts and last-operator protection. Test two-account browsers, keyboard
   flows and mobile/desktop views. Provision and verify an operator, switch the
   console to accounts, retire login links and update `iris ui`. Rehearse the switch
   and recovery using temporary databases; a real deployment is a later owner
   operation. Rollback must not reopen anonymous link access beside named accounts.

For behavioral PRs run `make test`, `make race`, `make build`, `make fixtures` and
the applicable web typecheck/lint/coverage/Playwright checks from `web/package.json`.
Go and web coverage remain 100% with behavior assertions and no exclusions. Exercise
the existing section 5 invariants and hook latency/fail-open checks when shared
authentication/store paths change. Release builds remain `CGO_ENABLED=0`.
For this documentation PR, verify local references, the role/route matrix and the
transfer inventory against code, and run `git diff --check`; runtime checks belong
to the later implementation PRs.

### Export inventory and adaptation boundaries

Paths below are relative to `~/iris-ui-import/ui`; they describe the inspected
export, not target API names. Transfer code deliberately and retain its provenance.

| Export evidence | Worth adapting after the backend contract exists |
|---|---|
| `src/App.tsx`, `renderAccount` (line 3492), display-name handling (1585) | Username/role facts, display-name form, loading/error/status and conflict feedback. Adapt to Iris edit revisions. |
| Same file, password handling (1680), password form (3624) | Current/new/confirmation fields, autocomplete, validation and rate-limit feedback. Rewrite the text that says the current session remains signed in: all sessions must end after an explicit password change. |
| Same file, topbar logout (3925) | Visible sign-out control using an Iris fetch mutation with CSRF; the exported native form and action do not fit the existing CSP. |
| Same file, `renderAdmin` (3703) | Account list, selected/disabled states and stale-permission feedback. Replace the project assignment editor with Iris repository no-grant/`ro`/`rw` controls. The export does not supply create/disable/global-role controls. |
| `src/LocalePicker.tsx`, `src/preferences.ts`, `src/i18n.ts` | Account language interaction patterns, retaining Iris's existing locale catalog/picker. Signed-in account UI locale wins; browser preference is for signed-out use only. Preserve correspondence locale in storage but hide its editor until Iris has a defined use; no translation or injected instruction is implied. |
| `src/app.css`, account/settings/list rules (1535, 1562, 1993) | Extract selected responsive styles into existing Iris styles without replacing the console shell. |
| `src/App.test.tsx`, account/access cases (6133 onward); `e2e/hermes.spec.ts`, account/access cases (904 onward) | Adapt same-origin, loading/authorization failure, revision conflict, keyboard and mobile scenarios to Iris contracts; do not import obsolete API mocks wholesale. |

Drop the exported project cards/archive lifecycle, numeric project identities and
generations; agent name/tone/profile editing; glob/shared/exclusive reservation
modes and orphan/legacy scope states; attachments, CC, importance, delivery commit
SHAs, quarantine/busy/deferred statuses and their retry controls. Iris has no
matching backend capabilities. Keep its existing repositories, advisory claims,
messages and delivery states. Drop the exported EventSource/SSE transport in
`src/mail.ts`; retain Iris WebSocket refresh with the new authorization rules.
Do not transfer route names, wire types, errors or role checks from `src/account.ts`
and `src/mail.ts` unchanged. Drop the human-priority preamble constructed in
`src/App.tsx` (144) and prepended by `DeliveryConfirmation` (425): neither a named
account nor a message body can order an agent to prioritize or execute anything.
Do not follow export test references to login templates or server files outside
`ui/`; they were not inspected and are outside this transfer's scope.

## Owner decisions accepted

- Operators have global console/access-management authority; members default to
  no repository access and receive explicit `ro`/`rw` grants. `rw` adds send/reply
  only, with `rw` on the origin and every recipient repository. Human mail stays
  shared, with immutable person attribution, shared replies and account retry keys.
- Import `admin` as operator, `member` without inferred grants, unknown roles as
  disabled `none`; map viewers explicitly and refuse source project operators.
- Use Argon2id for new passwords, bounded scrypt compatibility/upgrade, twelve-hour
  database sessions and all-session revocation after password/access changes.
- Retire bearer-created console links at cutover, use owner CLI provisioning and
  recovery, and defer correspondence-language effects and unsupported export views.
