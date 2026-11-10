# Console activation on the candidate image, 2026-11-10

Research record for [ADR 019](../adr/019-console-activation.md) and
[the activation runbook](../console-activation.md). It follows the
[rehearsals of 2026-11-09](console-activation-rehearsal-2026-11-09.md), which ran
on a modified checkout. This one ran the image that CI built for the pull
request, on the public hub's machine, beside the public hub and without
touching it.

## What ran

| | |
|---|---|
| Image | CI artifact `iris-rehearsal-image-pr250-02f7dd951a3fde521b43fc48e8f535d1bbce7cbc`, run 42727307235 |
| Archive SHA-256 | `47089945312ee1560304012566978ec638bc739ae772964c5d0bd90965466b04` |
| Image ID | `sha256:6bb14c71c825b95a666d4cc969b7033a58051ead779b82c11f247572a0d388b7` |
| `iris version` | `dev (commit 22b2dec882d73c3239b34190e846534d6c140d57; source clean; console unverified)` |
| Where | `127.0.0.1:17878`, host network, a Docker volume of its own, an empty database |
| Browser | Headless Chrome 157 driven through Playwright, one window of 1440 by 900 |

Commit `22b2dec` is GitHub's merge of the pull request's head `02f7dd9` into
`main` at `7e27efb`. It is on no branch, and this image was never deployed.
The accounts and passwords were synthetic and were deleted with the volume.

## What was observed

| Step | Observed |
|---|---|
| Start with an empty volume | Log: `Iris console closed: console key unavailable or not independent`. `GET /v1/ui/session` answered 401 with `X-Iris-Console-Mode: closed`. `GET /v1/ui/links` and `GET /ui/login` answered 404, and `GET /v1/status` 200. The browser showed `Console sign-in is unavailable. Contact the hub owner.` and a `Try again` button |
| `iris admin console create`, twice, with the password on standard input | Two accounts with the role `operator` |
| `iris admin console activate` with a wrong password | `unauthorized: Invalid username or password`, exit status 1 |
| `activate` with the right password, sent without the final newline that `create` had received | The account, then `"restart_required": true`; exit status 0 |
| Before the restart | The session read still answered 401 with the closed header. The key file `iris.db.console.key` had mode 0600, the hub's user as its owner, and 44 bytes |
| `docker restart` | Log: `Iris console open`. A session read without a cookie answered 401 without the closed header |
| Sign-in over HTTP with a cookie jar | A wrong password answered 401 `Invalid username or password`, the right one 200. The session read then named the account and its role. The cookie `iris_console_session` was `HttpOnly` and, on loopback over HTTP, not `Secure` |
| A second `docker restart` | The same cookie still read its session |
| `activate` again | `conflict: console is already active`, exit status 1 |
| Sign-in in the browser as the second account | The console opened with the account's name, `Sign out`, `Live updates connected` and the `Access` entry. After a reload it showed the same, and no sign-in form. The browser logged no error from the sign-in until the scratch hub was removed |
| At the end | Docker's restart count for the container was 0. Its 11 log lines held no password, cookie or token value |

The same image was then started once on a copy of the newest backup of the
public hub's database (8,945,664 bytes, taken before the deploy of 2026-11-09
19:52 UTC), on `127.0.0.1:17879`. It logged `Iris console closed`, listened, and
its retention sweep removed 5171 events and 450 idempotency records from the
copy. `GET /v1/status` answered 200, the session read 401 with the closed
header, and the page 200. Nothing was created, imported or activated on the
copy, and the copy was deleted.

During both runs the public hub kept its image and its restart count of 0.

## What this does not show

- The access checks in
  [the earlier record](console-activation-rehearsal-2026-11-09.md#what-a-candidate-is-checked-for)
  were not repeated. No repository mail was created, only operators existed, and
  one browser session was used.
- `iris admin console import` was not run. The accounts were made with `create`.
- Apache in front of the hub, TLS, the `Secure` cookie on the public origin and
  the five-minute wait in the runbook's step 9 were not part of it.
- It is one run on one host, with one window size.
- A later `main` or a rebased head gives a different image. This record is about
  the image named above.

## Going back, and another attempt

The runbook's first section says what going back leaves in place and what
another attempt then finds. A second run on the same machine measured that, on a
later image of the same pull request and the version the public hub runs.

| | |
|---|---|
| Image | CI artifact `iris-rehearsal-image-pr250-653916ea2c150d880393bc91b9074a91ba4a11c0`, run 42796592117 |
| Archive SHA-256 | `a61ae3c479f275a70cb8a9331494f7a5bb774e7f842e1710788b87c2391bffe6` |
| Image ID | `sha256:1fa4ac32e02dabea5a336f1f85d64cea4fe7a2320e944a7af38385ab6527da1c` |
| `iris version` | `dev (commit 174c12ce0c4a6df897c3c8f49f6c5d7e963ecf06; source clean; console unverified)` |
| The version before it | The image the public hub runs, commit `ef67498`, started by its commit tag |
| Where | `127.0.0.1:17878`, host network, a Docker volume of its own, one synthetic operator |

Commit `174c12c` is GitHub's merge of head `653916e` into `main` at `1ffed57`.
There was no browser: the sign-ins were HTTP requests with a cookie jar.

| Step | Observed |
|---|---|
| `ef67498` on an empty volume | Status 200. The session read answered 401 with `X-Iris-Console-Mode: links`. `iris admin console create` answered `not_found: unknown API route`: that version serves the import, not account creation |
| The candidate on that volume, as in step 5 | Log: `Iris console closed: console key unavailable or not independent`. Session read 401 with the closed header. `create` made the operator |
| The line of step 6 as the runbook prints it, with the scratch address and token in place of the public hub's and the password on standard input | Wrong password: `unauthorized: Invalid username or password`, exit status 1. Right password: the account and `"restart_required": true`, exit status 0. The session read still answered 401 closed |
| `docker restart`, as in step 7 | Log: `Iris console open` |
| Sign-in over HTTP, as in step 8 | 200, and the session read 200 |
| Going back: `ef67498` started on the same volume, with the key and the account in place | Status 200. Session read 401 with the links header, with the cookie from that sign-in and without it. Its log said nothing about the console |
| Another attempt: the candidate started on the same volume again | Log: `Iris console open` at once. The cookie from before going back read its session again, 200. A new sign-in answered 200. `activate` answered `conflict: console is already active`, exit status 1 |
| At the end | The last container's restart count was 0. The public hub kept its image, its `latest` tag and its restart count of 0 |

So going back does not end a session that was made before it. The older version
does not accept it, and the next start of the activation version accepts it
again until its twelve-hour limit.

This run does not show: the import, because the account was created; a browser;
a login link used on `ef67498` after going back; Compose and the tag switch that
a real going back uses, because each start here named its image; or the
five-minute wait.
