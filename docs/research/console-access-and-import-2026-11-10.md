# Console access and import on the candidate image, 2026-11-10

Research record for [ADR 019](../adr/019-console-activation.md),
[the activation runbook](../console-activation.md) and
[the import procedure](https://github.com/mateusz-klatt/iris/blob/658cfc81a0b5f2fe353ebd017d802c703ba3fa30/docs/accounts-import.md). The
[candidate run](console-activation-candidate-2026-11-10.md) left out two checks:
the access checks of
[the rehearsals of 2026-11-09](console-activation-rehearsal-2026-11-09.md#what-a-candidate-is-checked-for),
and `iris admin console import`. This run covers both on the final head's image.
The import ran on the version before the candidate, as the runbook's step 3 lets
the public hub's first cutover do it. The run then switched to the candidate,
activated, used the console in separate contexts of one Chromium browser and by
keyboard, went back to the version before and made another attempt.

## What ran

| | Candidate | Version before |
|---|---|---|
| CI artifact | `iris-rehearsal-image-pr250-653916ea2c150d880393bc91b9074a91ba4a11c0`, run 42796592117 | `iris-image-ef6749895645d9fe1007ec7e30610debecf8459a`, run 42736766565 |
| Archive SHA-256 | `a61ae3c479f275a70cb8a9331494f7a5bb774e7f842e1710788b87c2391bffe6` | `b35a351118cc146ba19249e3311432597a90788dd9688ad2b790bf1180a18864` |
| `image_id` in the artifact's `iris-image.txt` | `sha256:fd2a2e44319728929fa39b554f1426351f71edac8fa7e5a6142be1e647ed3c9b` | `sha256:c5089767e0013e3a37f4e6b9726af042cbc23f8b07f670db70729a42db7f6428` |
| `/iris` SHA-256 | `5a7456948bc26c0c986d9904a90c63f90c6699158a1e3bff265a6be572caa840` | `c2c901f224ff37d2794a1daf22394f1aa75f46bda85a74e0ecea9cea2a2af1f8` |
| `iris version` | `dev (commit 174c12ce0c4a6df897c3c8f49f6c5d7e963ecf06; source clean; console unverified)` | `dev (commit ef6749895645d9fe1007ec7e30610debecf8459a; source clean; console unverified)` |

Commit `174c12c` is GitHub's merge of the head `653916e` into `main` at
`1ffed57`. Rebased on `1ffed57`, that head is `dc6de3a`. `git rev-parse` gives the
same tree for both, `2934e91ca5030adff1e8052cccdd63fa198723d3`. The commits after
`dc6de3a` on the branch change only `docs/`.

| | |
|---|---|
| How | The image's only executable, `/iris`, taken out of each archive and run directly on WSL 2 (Linux 6.6, 16 cores) as the session's user. No container: Docker Desktop was not running |
| Where | `iris serve --listen 127.0.0.1:17930` with a database and token file in a new scratch directory. `HOME` and `TMPDIR` pointed into it, and every CLI call had `IRIS_HUB_URL` and `IRIS_TOKEN_FILE` set to the scratch hub |
| Accounts | A synthetic export made for the run: SQLite in DELETE journal mode, five rows with ADR 014 scrypt records (N 16384, r 8, p 1) of random passwords, and a version 1 manifest with one rename and one locale mapping. Two more accounts were created with `create` |
| Repositories | Two synthetic agent sessions, `alpha` and `beta`, registered with the owner bearer. Each sent one message to the operator mailbox |
| Browser | Chromium 153.0.8010.12, headless, through Playwright 1.63.0. Windows of 1440 by 900, and 390 by 844 as a phone (touch, scale 3) |
| When | 2026-11-10, 10:46:51 to 10:47:05 UTC |

The driver `rehearse.mjs` (SHA-256
`c41a136016e78b1064fa0cca471cfeaf3571ae9c57a9452a38360e48cf4bd5f5`), the source
generator `make_source.py` (`42c92d543c62e624b4abb6010b7cb7dcc676ed84b45fd8534314bc390b6a5bc5`)
and the archive reader `oci_extract.py`
(`ada8046de7694e8ad5f88aff9a735ed4fe6114a449d296d72395797dfcb5d01e`) lived in a
scratch directory of the session, with the report `report.json` (SHA-256
`9e6c2dad8969061ca6c4519b6739b29a47225ec5cee3b12e27443dd44c0d3e59`): 75 of 75
checks passed. They are local evidence, not part of the repository.

## What was observed

### The import, on the version before

| Step | Observed |
|---|---|
| `iris admin console import` dry run against `ef67498` | Five to create: rows 1 and 2 as operators, row 3 as a member, row 4 quarantined as a disabled `none` with its locale mapped, row 5 as a disabled member under its new name |
| `--apply` | Five created |
| The same dry run again | Five skipped, none created |
| `ef67498`'s console with those accounts in place | The session read answered 401 with `X-Iris-Console-Mode: links`. `GET` and `POST /v1/ui/login` answered 401 and set no cookie: an imported operator cannot sign in there |
| A login link from `ef67498`, made with `POST /v1/ui/links` and the owner bearer as `iris ui` does | Exchanged for an `iris_session` cookie that read `/v1/ui/overview`, 200 |

### The switch, owner setup and activation

| Step | Observed |
|---|---|
| The candidate on the same database | Log: `Iris console closed: console key unavailable or not independent`. Session read 401 with the closed header. `/v1/ui/links` and `/ui/login` answered 404, `/v1/status` 200. The legacy cookie from `ef67498` read nothing: 401 |
| `create` twice and `reset` once, the password on standard input | Exit status 0 each |
| `activate` for the imported operator with a wrong password, and for the quarantined account with its password | `unauthorized: Invalid username or password`, exit status 1, both |
| `activate` with the imported operator's password and one final LF | The account, then `"restart_required": true`, exit status 0. The session read still answered 401 closed. The key file had mode 0600, the hub's user as its owner, and 44 bytes |
| Restart | Log: `Iris console open`. A session read without a cookie answered 401 without the closed header |
| Sign-in over HTTP | A wrong password answered 401. The imported operator answered 200, and the session read named it as an operator. The cookie was `HttpOnly` and `SameSite=Strict`, and on loopback HTTP not `Secure`. The legacy cookie still read nothing |
| The other imported accounts | The 9-character imported password signed in: imported passwords keep no new-password minimum. The quarantined account and the disabled member, under its old name and its new one, answered 401 |
| `activate` again | `conflict: console is already active`, exit status 1 |
| A second restart | The same cookie read its session, with the same absolute expiry |

### The access checks

Two browser contexts, an operator's and a member's, in the 1440 by 900 window.

| Check | Observed |
|---|---|
| A member without grants | The page said `No repository access has been granted. You can manage your account.` and showed neither message. The overview listed no repository, `alpha`'s threads answered 404, and there was no Access entry |
| An operator grants `ro`, then `rw` | After each save the operator saw `Access saved. The account must sign in again.`, and the member's open console went back to the sign-in form by itself. With `ro` the member read `alpha`'s message, had no reply form, got 404 for `beta`'s threads, and 403 for a direct `POST /v1/ui/messages`. With `rw` the member sent a reply through Review and Reply, the console confirmed it, and the `alpha` agent's inbox held it once |
| An access edit made from stale data | A second operator's edit over HTTP answered 200. The first operator's save from the older form then showed the conflict notice, the grants stayed as the second operator had set them, and the form showed them |
| Removing the last operator | With the second operator demoted, the form said `The last enabled operator cannot be disabled or demoted.` and disabled the role and disabled controls. A direct `PUT /v1/ui/accounts/{id}/access` that demoted the operator answered 409, and the account stayed an enabled operator |
| A downgrade from `rw` to `ro` while the member is signed in | The member had a reply open in review. After the save the member's console went back to the sign-in form and the draft was gone. The earlier send, replayed with the same body and key, answered 401 with the old cookie and CSRF token, and 403 from a new `ro` session. The agent never received the draft |
| Signing in as another account in a second tab | Both tabs of one browser context, sharing its cookies, opened signed out. The first signed in as the member and opened `alpha`'s thread, then the second signed in as the operator. At the next agent message the first tab's overview read answered 401, and the tab showed the sign-in form, without the account's name or the thread. The second tab kept the operator |
| Narrow and wide windows, and the keyboard alone | In both windows, typing and with Tab, Enter, End and the arrow keys only: an operator signed in, opened Access, chose a member, set `alpha` to `rw` and saved; the member signed in, chose `alpha` in the repository filter, opened the thread, wrote a reply, reviewed and sent it, and signed out. No control was more than 15 presses of Tab from the one before it. The layout was measured at four points in each window: the operator signed in, access saved, the reply sent, the member signed out. At each, no page was wider than its window, and in the narrow one the only controls outside it were navigation buttons in the `.console-nav` strip, which scrolls sideways |
| The sign-in limit | A sixth sign-in for one account within 15 minutes answered 429 with `Retry-After` |
| Errors | No page raised an uncaught error |

### Going back, and another attempt

| Step | Observed |
|---|---|
| `ef67498` again on the same database | Session read with the named cookie: 401 with the links header. A new login link worked: the overview read 200. The import's dry run listed all five skipped |
| The candidate again | Log: `Iris console open` at once. The named session made before going back read again, with the same expiry. The cookie from the link used while going back read nothing: 401. The import's dry run listed all five skipped, and `activate` answered conflict |
| The log | 32 lines over the six starts. None held any of the 42 password, token, cookie, link-code or CSRF values the run used, and neither did the report |

Six development runs came before this one; their versions of the driver were not
kept. They changed the driver, never Iris:
expected statuses (an ungranted repository answers 404, not 403), selectors, and
the sign-in limit, which the narrow keyboard run first reached. In two of them
the last-operator check ran while the two accounts made with `create`, which
creates operators, were still operators, so it did not test what it named. The
run recorded here makes them members first and checks that only one enabled
operator is left before it tries the demotion.

## What this does not show

- The container. The binary ran as the session's user on WSL 2, not as user
  65532 with the `/data` volume, Compose and Docker's restart handling. The
  candidate record covers the container for activation and for going back.
- Apache, TLS, the `Secure` cookie on the public origin, HTTP/2 and the
  runbook's five-minute wait.
- The restore rehearsal with a fresh key, and the Apache boundary. On
  2026-11-09 the restore ran on the provisional binary built from a modified
  `0fe50af`, and the boundary with the older backend `bc1eae9`. Neither was
  repeated on the candidate.
- Sending the prepared draft. The console dropped it with the session, and its
  request was never submitted. The refused replays used the earlier send.
- A real export or real password records. The source was made for this run.
- Another browser engine or host. The e2e suite runs WebKit and Chromium
  against a mocked API, not this hub.
- A screen reader. The keyboard checks show that each named control is reached
  with Tab and works with the keys named, in Chromium on Linux.
- Every console action. New messages, the profile, a password change, a
  locale change, claims and diagnostics were not exercised.
- An interrupted or competing key publication, which only the tests exercise.
- The image CI builds after the merge, which is a different build.
