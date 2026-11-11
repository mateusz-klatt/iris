# Console activation rehearsals, 2026-11-09

Research record for [ADR 019](../adr/019-console-activation.md) and
[the activation runbook](../console-activation.md). It was first written inside
the runbook and moved here unchanged, apart from this introduction and the next
section, so that the runbook holds steps and this file holds evidence.

These rehearsals ran before the change was finished. The Iris binary was built
from a modified checkout, and the temporary directories named below were on a
host whose temporary files have since been removed, so the reports can no longer
be read. Treat the results as what was observed then, not as validation of a
released binary. The image CI built for the pull request was run on 2026-11-10:
see [the candidate record](console-activation-candidate-2026-11-10.md). That run
repeated the activation steps, not the access checks below. The
[access and import run](console-access-and-import-2026-11-10.md) repeated them on
the final head's image.

## What a candidate is checked for

Before a version with console sign-in is released, run it with a scratch
database on a loopback address and check, with two separate browser sessions:

| Check | What it must show |
|---|---|
| A member without grants | Sees no repository's mail |
| An operator grants `ro`, then `rw` | The member can read after the first and send after the second |
| An access edit made from stale data | Is refused as a conflict and changes nothing |
| Removing the last operator | Is refused |
| A downgrade from `rw` to `ro` while the member is signed in | Closes the member's session; a draft prepared before it cannot be sent, and a retry of an earlier send is refused |
| Signing in as another account in a second tab of the same browser | The first tab's next read is refused and it drops what it showed |
| Narrow and wide windows, and the keyboard alone | Every action stays reachable |

## What was observed

Both rehearsals ran on 2026-11-09 with temporary synthetic accounts/databases and
literal-loopback listeners. Reports contain safe checks and identities, not
credentials. Paths below are local evidence locations, not shipped release assets.

| Rehearsal | Observed result | Limits |
|---|---|---|
| Rebuilt Iris and real browser contexts | 44/44 checks passed | Modified source `0fe50af`; final clean candidate and atomic key-publication correction remain unvalidated |
| Actual Apache 2.4.69 and an older Iris build (`bc1eae9`, no longer kept in the repository) | 73/73 checks passed with the corrected raw doctor condition and Cookie stripping | Rendered local vhost, self-signed TLS, HTTP/1.1; no installed production configuration changes |

The real-binary report is `/tmp/iris-real-console-bdGopY/report.json`:

| Identity | SHA-256 |
|---|---|
| Report | `7e09a427327606b87ae934bc70b438f63fee50cd007a95bd18a1c20eaaba3cef` |
| Executed Iris binary | `f407697c4bf93c99759c34b60d75c1dfc3ef947372166ac2fbe1125e2b793211` |
| Embedded UI manifest | `488a2c470e170b9204a4c360e515b0dca83e7a7cf9094d12ee886a42a23238b8` |
| UI patch, `/tmp/iris-console-activation-ui.patch` | `c4037d8f08c389b9a1fa4c5c388c0397cc49cd98a67559807172a215b24a4c8e` |
| Browser driver, `v4-observed-read-restored-cache-authorization` | `78d81a7a79f5e62c7063b53c40223806bf27922edc1a5318893985ab62cde47d` |

The binary reports source `0fe50af` with `source_modified: true`; the embedded UI
includes uncommitted changes. Its observed workflows included actual owner
create/reset/import CLI calls, real KDF verification and upgrade of a short
synthetic imported scrypt password, activation without hot opening, restart and
unchanged absolute session expiry. Two browser contexts exercised operator-to-member
access editing, a live member stream and a prepared draft. `rw` to `ro` downgrade
closed the session and cleared the draft; stale prepared sends and cached retries
returned 401, and a newly authenticated `ro` send returned 403. An actual next read
with the old mounted CSRF and a replacement shared cookie returned 401.

The actual backup CLI captured named sessions. With the old key withheld, the
restored hub stayed closed; fresh-key activation cleared all restored sessions,
and tested old cookies were rejected after restart. Through the private UI the
owner reviewed a resurrected `rw` grant and reduced it to `ro` before reopening.
A fresh restored `ro` session could read the message but received 403 replaying a
previously successful cached `rw` send. These are observed synthetic flows, not
proof about a production backup or every historical credential.

Initial 50-case Apache evidence is `/tmp/iris-apache-rollback-w578u1xj/`: `report.md`,
`metadata.json`, rendered `httpd.conf`, `rollback-block.conf`, `configtest.txt`,
sanitized logs and independent cleanup verification. The old binary SHA-256 was
`b0cb7dd39e7d99f612edbca06a73ca1446972e159f62a5857a458a045076f58f`.
Apache used pinned image
`httpd@sha256:f6a86043744215e4be90cf8804375d90022a711d892c9aa2d7085c089ee2cb54`.
The follow-up `/tmp/iris-apache-rollback-paths-5qq8yavj/metadata.json` replayed
those checks and added raw-path cases, passing 73/73. Its recorded outbound
request-line bytes match the supplied raw targets, so the client did not normalize
away the double slashes or dot segments. The documented Apache block SHA-256 is
`21a003def78e3012537af069fdbed56feb04344273d60c44bb21ecd3f40ec0d3`,
matching both rehearsals exactly.

| Actual request/control | Observed status |
|---|---|
| Root, assets, login links, plain/encoded UI paths and actual UI WebSocket Upgrade | 503 |
| Doctor GET, HEAD, POST, PATCH, DELETE, OPTIONS and TRACE; PUT with a trailing slash or encoded doctor spelling | 503 |
| Literal doctor PUT, including an optional query, with valid owner/host bearer | 200 |
| Doctor PUT with no bearer or invalid bearer, including a valid old cookie | 401 |
| Agent status and actual registration | 200 |
| HTTP and HTTPS local ACME proof | 200 |
| Normal HTTP redirect to fixed HTTPS origin | 308 |
| Status over separately negotiated TLS 1.2 and TLS 1.3 | 200 |
| Encoded separators `/v1%2fui/overview` and `/v1/ui%2fdoctor` | 404 before the backend |
| Raw UI GET `//v1/ui/overview`, `/v1//ui/overview`, `/v1/./ui/overview`, `/v1/x/../ui/overview`, each with cookie-only and machine+cookie | 503 in all eight cases |
| Raw doctor PUT `//v1/ui/doctor`, `/v1//ui/doctor`, `/v1/./ui/doctor`, `/v1/x/../ui/doctor`, `/v1/ui/doctor/`, `/v1/ui/doctor/.`, each with cookie-only and machine+cookie | 503 in all twelve cases |

All twenty noncanonical UI/doctor cases were denied without redirects or
successful authentication. A normal literal doctor query returned 200 with a
valid machine bearer and 401 with the same valid legacy cookie alone.
Production certificate trust, issuance and
renewal, external DNS/networking, HTTP/2, rejection of older TLS versions and
draining established WebSockets across reload were not tested. The new Upgrade
was blocked, but an existing connection still requires explicit closure. Owned
temporary processes and containers were stopped/removed; no other service was
restarted. Record final clean-candidate local gates in the associated PR and verify
target-deployment readiness before owner activation.

A separate nine-request named-console control is recorded in
`/tmp/iris-named-cookie-strip-ir2vzzqq/metadata.json`. With the preliminary
`f407697c` binary, normal proxy login returned 200 and `Set-Cookie`; the same
session survived a restart. Retaining only request `Cookie` removal caused the
next session read to return 401, while the same cookie worked directly. A login
challenge still set a response cookie, but its submission returned 403 because
the request cookie was stripped. Removing that single directive restored the
original session to 200. This validates the leave-closed-mode check without
claiming anything about the final release binary or installed production vhost.
