# Idle hub work and event-driven streams

The production observations below were reported by the Holzera operator on
2026-11-09 and 2026-11-10. Local reproductions use synthetic WSL databases and loopback sockets;
no production database or account data was used by the implementer.

## Operator report

On production commit `4840765`, during 17:27–17:31 UTC, the operator reported
313 CPU ticks in about six seconds, Docker CPU readings of 35–44%, and five
runtime threads at 6–13% each. Fifteen connections to port 7878 arrived through
the reverse proxy. The WAL was rewritten about once a second, with roughly
48 KB/s of block writes and 26 MB in nine minutes while application logs were
quiet. The database was 8.2 MB, with 148 Agents, 468 Executions, 3,227 Claims,
2,583 Deliveries and 1,900 Messages. No production CPU profiler was attached.
These are reported observations, not measurements independently taken here.

## Reproduced mechanism

The previous Execution stream's 100 ms timer ran lease validation and event
lookup in separate transactions. Pings also returned to that observation loop.
Each domain call checked expiry, including a historical inactive-Execution scan
for delivery requeue. Generic named consumers rewrote their Cursor row even
when its sequence did not advance. Legacy consoles polled every second; named
consoles separately polled session validity and frame admission.

Deterministic regressions use Go's in-memory HTTP/WebSocket test network and
virtual time, counting actual begun store transactions. They deliberately use a
200 ms ping interval, rather than the default 25 seconds, to prove pings do not
cause observations. After initial snapshots, three virtual seconds produce:

| Watch sockets, plus two legacy consoles | Before transactions | After transactions |
| --- | ---: | ---: |
| 1 | 126 | 3 |
| 5 | 486 | 3 |
| 20 | 1,836 | 3 |

The remaining three transactions are shared maintenance, not per-socket polls.
These accelerated correctness-test counts are not production transaction rates.
Additional tests cover coalesced bursts, draining over 100 events without a
fresh commit, rotation during upgrade and while idle, lease/claim/receipt/session
expiry, observer shutdown/restart, and revocation during a stalled network write.
Slow UI writers retain the initial admitted frame and latest queued revision;
Execution event pages are never discarded by that coalescing.

## Minute benchmark

The reproducible Linux benchmark is `internal/hub/idle_benchmark_linux_test.go`:

```sh
CGO_ENABLED=0 go test ./internal/hub -run '^$' -bench '^BenchmarkIdleHubCPU$' -benchtime=1x -count=1 -timeout=15m
```

Each case measures an actual minute after initial-observation barriers and a
1.5 second warmup. One fixture has the reported table counts plus 1,656 Events;
the other multiplies that history by ten. Both keep 20 active Executions,
80 unrevoked Claims and 200 unacknowledged Deliveries. Bodies are 280 synthetic
bytes. Foreign keys and exact row counts are checked. There are 1, 5 or 20 watch
sockets plus two legacy console sockets, using real loopback HTTP/WebSockets and
the default ping interval. The named consumer `idle-benchmark` exercises Cursor
persistence; `IRIS_IDLE_BENCH_CONSUMER=iris-watch` selects the separately
acknowledged monitor path for an additional experiment.

CPU time covers the whole benchmark process, including loopback clients and Go
runtime work. CPU seconds per minute can exceed 60 when multiple runtime threads
run. It is not a server-only CPU measurement. Twenty registration RPC samples
are taken afterward with the same sockets open, outside the idle CPU interval.
They include named-host-token authentication and response reading, but not an
agent process, full hook startup, DNS, TLS or the production proxy. Their new
sessions do not contaminate the preceding idle interval.

Before is `08aa3fa` with only the process-local transaction counter added to
application code. The same frozen benchmark helper is used before and after.
The WSL host is shared rather than CPU-isolated; per-process CPU, fixture,
toolchain, affinity and observation intervals are recorded with the raw results.
Both six-case series completed successfully, with at least 60 measured seconds
per case and no idle frames. These intervals contain no application writes or
periodic heartbeats. Every after case began 60 store transactions in its minute,
independent of the number of sockets and the history multiplier. This is a
quiescent measurement, not a claim that active watch clients cost one transaction
per second. The current notifier broadcasts each semantic commit to all streams;
staggered heartbeats therefore add their own authentication/write transactions and
an observer-count multiplied by write-rate term. Scoped observer notifications
remain a separate follow-up.

| History | Watch sockets, plus two legacy consoles | CPU seconds/minute before | CPU seconds/minute after | Transactions/second before | Transactions/second after | Registration RPC p95 ms before | Registration RPC p95 ms after |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1×, quiet repeat | 1 | 5.077 | 0.052 | 22.13 | 1.00 | 30.90 | 7.11 |
| 1× | 5 | 26.964 | 0.057 | 102.40 | 1.00 | 68.73 | 6.12 |
| 1× | 20 | 65.930 | 0.051 | 323.16 | 1.00 | 133.60 | 15.68 |
| 10× | 1 | 47.190 | 0.046 | 21.70 | 1.00 | 276.43 | 15.35 |
| 10× | 5 | 60.561 | 0.047 | 27.23 | 1.00 | 390.31 | 38.88 |
| 10× | 20 | 60.810 | 0.056 | 31.33 | 1.00 | 2746.85 | 29.38 |

The first after interval, 18:20:46–18:21:46 UTC, may overlap another agent's heavy
checks, later reported as running from 18:18:51 until about 18:21 UTC. It was
therefore repeated alone for a full minute, 18:27:22–18:28:22 UTC, after those
checks stopped, using the same compiled binary and helper without source edits.
The table explicitly selects that quiet repeat. Both observations are retained:

| 1× history, one watch and two legacy consoles | Actual seconds | CPU seconds/minute | Transactions/second | Registration RPC p50 ms | Registration RPC p95 ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| Original, possible overlap | 60.000305 | 0.056488 | 0.999995 | 8.83 | 17.04 |
| Quiet repeat, selected above | 60.001342 | 0.051802 | 0.999978 | 5.35 | 7.11 |

These synthetic measurements show a 98.98–99.92% reduction in process CPU for
the selected comparisons. The low after values and twenty-sample RPC percentiles
remain subject to shared-host scheduling noise; they are not production latency
or capacity guarantees. Lower before transaction throughput with larger history
reflects serialized expensive work, rather than fewer requested observations.

### Measurement provenance

The base commit is `08aa3fab3f3fd11f024a6acbd07c58a63d08ce58`. The measured after
candidate is that commit plus the captured application patch, rather than a new
commit. Application Go/SQL fingerprints were unchanged through the single
build, six-case run and targeted repeat. The complete Go/SQL sources, including
tests, and module manifests are archived alongside path-to-content-SHA manifests.

| Artifact | SHA256 |
| --- | --- |
| Frozen benchmark helper, identical before and after | `5f4f2a41fc29c910277628892fa2392609ad855b3b078138375de32015c72203` |
| Before binary, with counter instrumentation only | `e3d49eb517ef80ccf219bb7ff48ac50a6b206cd31e04c174b3031748df3bdb28` |
| After binary, also used for the quiet repeat | `562452b09e5c91a437649d05e1d16030f95133a034c5eb8bf015857f98db7ecf` |
| After application Go/SQL manifest fingerprint, excluding tests | `6794cfee74f5966155ca5ca09b21447dc47d96bcdd9a181f53c42e3190c3e176` |
| After application patch, including new files | `cd8cf1664b5fa6e912ccd281a277e933e1966ea4c8e5e924411d542eb31a452b` |

The manifest fingerprint hashes compact, sorted JSON mapping repository-relative
paths to each file's SHA256. Both binaries use `go1.27.3`, `CGO_ENABLED=0`, default
consumer and duration settings, runtime CPU count 16 and `GOMAXPROCS=16`.
Before and after confirmed hostname `home`, kernel
`6.6.87.2-microsoft-standard-WSL2`, boot ID
`76eef03e-3e34-45a0-92a1-12dbdc63b66c`, and CPU affinity `0-15` on an Intel
Core i7-11700K. The before run was 18:04:19–18:11:34 UTC; the six-case after run
was 18:20:44–18:26:59 UTC, followed by the labelled repeat above, all on 2026-11-09.

Raw local artifacts are retained in `/tmp/iris-idle-before-p44f_0kw` and
`/tmp/iris-idle-after-2_7ygygt`. They include binaries, raw JSON/logs with exact
intervals and p50/p95 samples, host metadata, source manifests and patches, and
the frozen helper. The after directory also contains `source-go-sql.tar.gz`,
`comparison.json`, `summary.md`, and separate `quiet-repeat/` results preserving
the original first case. The binaries were built once with
`CGO_ENABLED=0 go test -c -o <binary> ./internal/hub`, then run with
`-test.run=^$ -test.bench=^BenchmarkIdleHubCPU$ -test.benchtime=1x -test.count=1`.
The repeat used the anchored selector
`-test.bench=^BenchmarkIdleHubCPU$/^history=1$/^watches=1$`.

## Live heartbeat experiment

The separate opt-in `BenchmarkLiveHubCPU` uses the 10× history fixture, 20 watch
sockets and two legacy consoles. Each watch client sends a real authenticated
heartbeat every 20 seconds. Initial phases are 0.5–19.5 seconds, spaced one
second apart, so the 10 ms coalescer cannot turn the workload into one artificial
burst. On 2026-11-09, the full minute ended at 18:53:40 UTC:

| Measured quantity | Result |
| --- | ---: |
| Actual interval | 60.001088 seconds |
| Process CPU seconds/minute | 0.768528 |
| Store transactions | 1,500, or 24.99955/second |
| Successful heartbeat RPCs | 60 of 60; no errors or skipped slots |
| Watch observations | 1,200 |
| Legacy console observations | 120 |
| Shared maintenance calls | 60 |
| Heartbeat operation transactions | 60 |
| Named-token authentication transactions | 60 |

Every client sent three heartbeats. Raw request timestamps show inter-client
spacing of 0.997545–1.000952 seconds and per-client periods of
19.996586–20.000466 seconds; the largest scheduled-phase error was 4.299 ms.
Thus the extra transactions are the expected broadcast observation cost, not
idle timer polls. With `H` writers, period `T` and `N` subscribed streams, separated
commits can add about `N*H/T` observation transactions per second, in addition
to maintenance and each heartbeat's own authentication/write transactions.
Here, `H=20`, `T=20` and `N=22` give 22 observations plus two request transactions
and one maintenance transaction per second. Watch hints scoped to their Agent's
events and Execution state, with console broadcasts retained, are follow-up work.

The heartbeat workers stop and join before registration RPC sampling. Its p50
19.08 ms and p95 24.57 ms therefore describe the same open sockets after that
write workload stops, not registration latency during ongoing heartbeats or a
full hook. No frames were sent: these legacy consoles use the global event
cursor, which heartbeats do not advance. Named-console scoped revisions can
advance on heartbeat writes; their correctness has separate tests.

The application fingerprint was
`5963ffb6afd30711b39e6c74abbd05186ec774324c80b56104c9c47955c7ca9d`:
a pre-merge checkpoint of the stream change (`df1d873`, a commit the repository no
longer keeps; the change itself is on `main` as #245) plus the follow-up moving
console-session deletion into the shared maintenance operation. The live binary SHA256 was
`b21a6087791fd7132dd28567de9ad715508da8c7b63713c7bdc37f917579790a`,
and the extended helper SHA256 was
`89223bd35dc026d2e1874c16363f07a3e0e9be2a12db1a04d8e27f5e96b8506e`.
The six-case quiescent table above retains its original frozen helper and
source provenance; it was not silently remeasured or relabelled. A later hub
admission I/O-boundary refactor for deterministic ordering tests preserves
behavior and is outside the captured measurement source.

Go 1.27.3, `CGO_ENABLED=0`, runtime CPU count 16, `GOMAXPROCS=16`, affinity 0–15,
host and boot identity match the earlier experiments. Iris heavy checks were
paused; residual external activity and historical load remained on this shared
WSL host. Load averages fell from 3.41 to 1.18; this is not CPU isolation. Raw
events, phase audit, source/module manifests, patches, binary, process/load
snapshots and report are in `/tmp/iris-live-cpu-jp4nn7ac`.

```sh
CGO_ENABLED=0 go test ./internal/hub -run '^$' -bench '^BenchmarkLiveHubCPU$' -benchtime=1x -count=1 -timeout=5m
```

## Query and migration limits

Schema 21 adds partial active-Execution/open-Claim/attention indexes, an event
recipient/cursor index and a console-session expiry index. Modernc query-plan
regressions check every maintenance statement and the attention/event read paths
against small and larger synthetic history. Requeue uses newly lost or explicitly
ended Execution IDs and the existing Delivery execution index, retaining the
per-delivery Ack check and transaction rollback guarantees.

`TestIdleCoordinationPlansAvoidScanningHistoricalRows` runs `EXPLAIN QUERY PLAN`
through the application's modernc driver with 200 and 2,000 historical rows. Both
fixtures select these access paths:

| Observation | Selected access path |
| --- | --- |
| Execution expiry | Covering scan of partial `execution_active_deadline`, containing active Executions only |
| Claim expiry | Scan of partial `claim_unrevoked_execution`, then Execution primary-key lookup |
| Newly lost/ended delivery requeue | `delivery_execution` lookup and correlated Ack primary-key lookup |
| Missing receipt | Range lookup on `DeliveryReceiptDue`, with Ack primary-key membership |
| Console-session expiry | Range lookup on covering `console_session_expiry` |
| Pending attention count | Recipient lookup on partial `delivery_attention_recipient`, with Ack primary-key membership |
| Event page | Recipient/sequence lookup on `event_recipient_cursor`; settlement subquery scans only the partial attention index |

Index scans of active Executions, open Claims or attention Deliveries still grow
with those live sets. The regression checks the actual shared SQL constants and
rejects full historical Execution, Delivery or Claim scans. It does not substitute
an alternate SQL implementation for the maintenance path.

The migration builds those indexes and clears acknowledged historical receipt
deadlines once. Handover settlement and movement now clear obsolete fetch
deadlines too. Startup therefore has a one-time history-dependent cost. Real
loss transitions can still visit acknowledged deliveries attached to that
Execution; claim-release fanout still visits durable repository Agents. No fault
injection boundary, state comparison, retry assertion or coverage gate is removed.
Console-session deletion runs only in the shared maintenance operation. With no
streams its worker is stopped, so expired rows can remain until another stream
or session cleanup. Authentication checks absolute expiry at use and never treats
an expired retained row as authority. Ordinary domain calls, including forget
dry-runs, do not perform this unrelated housekeeping.

## Public hub before and after the deploy

The Holzera operator read the hub process's CPU time, user plus system, from
`/proc/<pid>/stat` over 60-second windows, and counted the established TCP
connections to port 7878 with each window. No profiler was attached, and the
load was whatever the fleet was doing at the time.

| Commit | Windows began, UTC | CPU seconds in each window | Connections |
|---|---|---|---|
| `4840765`, before the change | 2026-11-09 19:27:40, 19:28:40, 19:29:41 | 31.26, 24.82, 30.73 | 15 |
| `cfcd86a`, before the change | 19:34:07, 19:35:08, 19:36:08 | 32.71, 34.02, 31.86 | 14 |
| `6d555c7`, with the change | 19:54:11, 19:55:11, 19:56:11, 19:57:11 | 0.34, 0.27, 0.32, 0.32 | 11 to 12 |
| `6d555c7`, with the change | 2026-11-10 01:32 | 0.14 | 10 at the start, 8 at the end |
| `ef67498`, with the change | 01:55:58, after its deploy at 01:55:37 | 0.15 | 9 |

The host was restarted on 2026-11-09 at 22:14 UTC. From the container's start at
22:15:02 until 01:32 the process used 18.5 CPU seconds in 3.29 hours: 0.09 CPU
seconds a minute, or 0.16 percent of one core.

What these readings do not show:

- They are not a controlled comparison. The WSL sessions ended seconds after the
  19:52 deploy of `6d555c7`, so fewer agent streams were attached afterwards,
  though the number of connections was close. A connection is a socket, most of
  them through the reverse proxy, not a session.
- They say nothing about hook latency, wake latency or many more sockets. The
  minute benchmark above compares equal numbers of sockets on synthetic data.
- Each reading is the process time of one machine. Disk writes were not read.

## What was not measured

This work does not measure the public hub under a controlled load, native Windows/macOS CPU,
reverse-proxy/TLS delays, a full agent hook under the benchmark load, or real
model-turn wake latency. Named-console correctness is tested with the gated
account path, but the minute benchmark uses the currently deployed legacy
console mode. Synthetic history is not a copy of production workload or a
capacity guarantee. Hook failures from network or filesystem stalls remain
possible, and claims remain advisory outside mediated actions.
