# Idle hub work and event-driven streams

The production observations below were reported by the Holzera operator on
2026-11-09. Local reproductions use synthetic WSL databases and loopback sockets;
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
per case and no idle frames. Every after case began 60 store transactions in its
minute, independent of the number of sockets and the history multiplier.

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

## What was not measured

This work does not measure a production redeploy, native Windows/macOS CPU,
reverse-proxy/TLS delays, a full agent hook under the benchmark load, or real
model-turn wake latency. Named-console correctness is tested with the gated
account path, but the minute benchmark uses the currently deployed legacy
console mode. Synthetic history is not a copy of production workload or a
capacity guarantee. Hook failures from network or filesystem stalls remain
possible, and claims remain advisory outside mediated actions.
