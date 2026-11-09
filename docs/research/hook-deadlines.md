# Hook deadlines and retained execution history

Measured on WSL/ext4 on 2026-11-09, from `fix/hook-deadlines` based on
`c352df9b31bb371231f07cbf2b909b29065ba620`. All experiments used a loopback
hub, scratch database and scratch named host token. No production load was sent.

## Local failure evidence

The read-only failure log snapshot contained 303 valid entries, spanning
2026-11-08 21:07:08 UTC to 2026-11-09 15:04:59 UTC. November 9 contained 258:

| Client | Stage | Count |
| --- | --- | ---: |
| Codex | shell session | 237 |
| Codex | shell status | 5 |
| Codex | shell reconcile | 3 |
| Codex | session | 2 |
| Codex | touch | 6 |
| Claude | shell session | 3 |
| Claude | shell status | 1 |
| Claude | session | 1 |

Shell entries reported the reconciliation budget; session and touch entries
reported the hook deadline. Codex shell-session counts at the reported UTC peaks
were 07h: 5, 08h: 15, 10h: 71, 11h: 22, 13h: 33. No Antigravity failure remained
among the November 9 entries. Rotation and best-effort logging limit what the
counts prove.

The entries contain no session, host or worktree identity. They cannot establish
which checkouts failed or distinguish ext4 from `/mnt/d`. With the current
combined protocol, git status runs before the stage named `shell session`;
that stage covers registration and reconciliation together.

## Reproduction and timings

A temporary Go harness started the real store and HTTP hub on `127.0.0.1`,
created a named token, and set `IRIS_HUB_URL` and `IRIS_TOKEN_FILE` to those scratch
resources. Eight registered Codex sessions shared this ext4 worktree. The scratch
database also held 50 ended sessions in separate old mailboxes, followed by
10,000 ended executions in another mailbox. These are synthetic retained records,
not a copy or count of production history.

Fifteen synchronized rounds ran eight shell hooks each, with real repository
discovery, ancestor lookup and git status. Each hook opened a fresh HTTP
connection. Go execution was bounded with `GOMAXPROCS=2`; at most eight status
subprocesses ran concurrently. The existing 300 ms loopback shell budget and
1.5 s overall deadline were used. The paired measurements differed only in
whether the scratch database had the new agent index. Timed HTTP calls were
allowed to finish cancellation before collecting their results.

| Stage | Before p50 / p95, ms | After p50 / p95, ms |
| --- | ---: | ---: |
| Repository discovery | 0.055 / 0.101 | 0.058 / 0.129 |
| Git status | 9.173 / 24.439 | 7.605 / 14.514 |
| Ancestor lookup | 0.134 / 0.368 | 0.138 / 0.379 |
| Shell session HTTP request | 233.682 / 299.515 | 34.958 / 67.015 |
| Complete hook | 242.693 / 301.187 | 45.389 / 73.838 |

Before: 38 of 120 requests exhausted the shell budget. After: zero of 120.
The before HTTP tail is censored by cancellation, not the unconstrained service
time. An earlier empty-history control had HTTP p95 9.930 ms and zero failures.

A separate real-driver profile used 50 live sessions, 10,000 ended executions,
10,000 events and eight workers over 40 registration requests, also bounded to
two Go processors. Registration p50/p95 fell from 328.87/375.88 ms to
76.12/86.23 ms. The peer-expansion query itself fell from 33.15/41.76 ms to
0.227/0.416 ms. Commit time was tens of microseconds on this scratch filesystem.

## Defect and scope

Every registration expands its repository's peers, even when the combined shell
response ultimately discards that expansion. For each peer, the query selects
its latest execution by descending rowid. Without an index on `Execution.agent_id`,
SQLite scans execution history and sorts matching rows for each agent. That
repeated work runs under the store mutex and its single transaction, so concurrent
requests queue behind it. The regression first failed with `SCAN Execution` and
`USE TEMP B-TREE FOR ORDER BY` in the actual registration query plan.

Cached binding refresh adds `/v1/bind` traffic to that same serialized store, so
it can increase queueing under MCP activity. The hook-only reproduction already
fails without that traffic. Generation diagnostics run after commit and outside
the store lock; the adoption deadline is in the MCP maintenance path. Neither
was needed to reproduce this defect, so neither mechanism was changed.

One index on `Execution(agent_id)` lets SQLite seek an agent's last row directly;
ordinary SQLite indexes include rowid. The idempotent base schema runs on every
open, so existing current-version databases acquire the index transactionally as
well. Regression tests assert the latest peer's branch and state and the indexed
lookup on fresh and populated reopened databases, without timing thresholds.
The cost is index storage, maintenance when executions are inserted and a one-time
index build at startup. The protocol, budgets, durability and locking stay the same.

This reproduces and fixes a repository defect; it does not establish the cause of
every retained failure. Real DNS/TCP/TLS stalls, slow filesystem operations,
overloaded hosts and other expensive store work can still exhaust the budgets.
In particular the expiry requeue query still scans inactive execution history;
the profile measured it at p50/p95 7.06/12.11 ms after this fix. D3 still fails
open and records failures. No claim of protection during an outage follows.
