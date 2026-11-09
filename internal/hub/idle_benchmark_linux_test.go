//go:build linux

package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

// BenchmarkIdleHubCPU is an opt-in, real-time measurement, not a correctness
// test. Run with -run '^$' -bench '^BenchmarkIdleHubCPU$' -benchtime=1x. Its
// process CPU includes the real loopback WebSocket clients and the benchmark
// harness; it does not claim to be a server-only process measurement.
//
// IRIS_IDLE_BENCH_DURATION defaults to one measured minute per case. Shorter
// exploratory intervals are printed as such, with CPU scaled to seconds/minute.
// IRIS_IDLE_BENCH_CONSUMER defaults to a named durable consumer; set iris-watch
// to measure the monitor path whose cursor is acknowledged separately.
func BenchmarkIdleHubCPU(b *testing.B) {
	benchmarkHubCPU(b, false)
}

// BenchmarkLiveHubCPU measures the 10x-history, 20-watch, two-console fixture
// with one authenticated heartbeat per watch every 20 seconds. Phases are
// separated by one second, so the 10ms observer coalescer cannot normally turn
// independent heartbeats into an artificial single burst. CPU includes clients,
// authentication, writes, observer wakeups and the bounded worker drain.
func BenchmarkLiveHubCPU(b *testing.B) {
	benchmarkHubCPU(b, true)
}

func benchmarkHubCPU(b *testing.B, live bool) {
	duration := time.Minute
	if value := os.Getenv("IRIS_IDLE_BENCH_DURATION"); value != "" {
		var err error
		duration, err = time.ParseDuration(value)
		if err != nil || duration <= 0 {
			b.Fatalf("invalid measurement duration %q", value)
		}
	}
	consumer := os.Getenv("IRIS_IDLE_BENCH_CONSUMER")
	if consumer == "" {
		consumer = "idle-benchmark"
	}
	scales, socketCounts := []int{1, 10}, []int{1, 5, 20}
	if live {
		scales, socketCounts = []int{10}, []int{20}
	}
	for _, scale := range scales {
		b.Run(fmt.Sprintf("history=%d", scale), func(b *testing.B) {
			for _, watches := range socketCounts {
				b.Run(fmt.Sprintf("watches=%d", watches), func(b *testing.B) {
					b.StopTimer()
					fixture := idleBenchmarkFixture(b, scale)
					server := httptest.NewUnstartedServer(nil)
					h := New(fixture.store, "idle-owner-secret", []string{server.Listener.Addr().String()})
					h.Log = log.New(io.Discard, "", 0)
					var calls idleBenchmarkCallCounts
					type readyEvent struct {
						once sync.Once
						done chan struct{}
					}
					initialEvents := make(map[string]*readyEvent, watches)
					for i := range watches {
						initialEvents[fmt.Sprintf("idle-ex-%d", i)] = &readyEvent{done: make(chan struct{})}
					}
					realCall := h.call
					h.call = func(ctx context.Context, operation string, request store.Request) (map[string]any, error) {
						out, err := realCall(ctx, operation, request)
						if live {
							calls.record(operation)
						}
						if err == nil && operation == "events" {
							if ready := initialEvents[request.ExecutionID]; ready != nil {
								ready.once.Do(func() { close(ready.done) })
							}
						}
						return out, err
					}
					server.Config.Handler = h
					server.Start()
					defer server.Close()
					token, err := fixture.store.AddToken(context.Background(), "idle-benchmark-host")
					if err != nil {
						b.Fatal(err)
					}
					var readers sync.WaitGroup
					var frames atomic.Uint64
					var connections []*websocket.Conn
					dial := func(endpoint, credential string, protocol []string, initialFrame bool) {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
							HTTPHeader: http.Header{"Authorization": {"Bearer " + credential}}, Subprotocols: protocol,
						})
						if err != nil {
							b.Fatal(err)
						}
						connections = append(connections, conn)
						first := make(chan struct{})
						failed := make(chan error, 1)
						var firstOnce sync.Once
						readers.Add(1)
						go func() {
							defer readers.Done()
							for {
								if _, _, err := conn.Read(context.Background()); err != nil {
									failed <- err
									return
								}
								frames.Add(1)
								firstOnce.Do(func() { close(first) })
							}
						}()
						if initialFrame {
							select {
							case <-first:
							case err := <-failed:
								b.Fatal("console closed before initial frame", err)
							case <-ctx.Done():
								b.Fatal("console initial frame timeout", ctx.Err())
							}
						}
					}
					defer func() {
						for _, conn := range connections {
							_ = conn.CloseNow()
						}
						readers.Wait()
						deadline := time.Now().Add(10 * time.Second)
						for {
							h.mu.Lock()
							remaining := len(h.socketCounts)
							h.mu.Unlock()
							if remaining == 0 {
								break
							}
							if time.Now().After(deadline) {
								b.Error("socket handlers did not finish during benchmark cleanup")
								break
							}
							time.Sleep(time.Millisecond)
						}
					}()
					for i := range watches {
						endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/watch?execution_id=" + url.QueryEscape(fmt.Sprintf("idle-ex-%d", i)) +
							fmt.Sprintf("&generation=1&after=%d&consumer_id=%s", fixture.events, url.QueryEscape(consumer))
						dial(endpoint, token, []string{"iris.v1"}, false)
					}
					for range 2 {
						dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ui/watch", "idle-owner-secret", nil, true)
					}
					for _, initial := range initialEvents {
						select {
						case <-initial.done:
						case <-time.After(10 * time.Second):
							b.Fatal("watch did not complete its initial observation")
						}
					}
					// Exclude authentication, initial snapshots and subscription setup.
					// This is a real benchmark warmup, never a test synchronisation assertion.
					time.Sleep(1500 * time.Millisecond)
					beforeCPU := idleBenchmarkCPU(b)
					beforeTransactions, beforeFrames := fixture.store.TransactionCount(), frames.Load()
					beforeCalls := calls.snapshot()
					start := time.Now()
					startedUTC := start.UTC().Format(time.RFC3339Nano)
					var heartbeats *idleBenchmarkHeartbeats
					if live {
						heartbeats = startIdleBenchmarkHeartbeats(server.Client(), server.URL, token, watches, start)
						defer heartbeats.stopAndWait()
					}
					b.StartTimer()
					for range b.N {
						time.Sleep(duration)
					}
					b.StopTimer()
					if heartbeats != nil {
						// Join before final metrics, registration RPCs and socket cleanup.
						// A request already in flight has a five-second deadline; its
						// bounded drain is included in the reported actual interval.
						heartbeats.stopAndWait()
						for _, err := range heartbeats.errors() {
							b.Error(err)
						}
						if b.Failed() {
							b.FailNow()
						}
					}
					elapsed := time.Since(start).Seconds()
					cpu := idleBenchmarkCPU(b) - beforeCPU
					transactions := fixture.store.TransactionCount() - beforeTransactions
					idleFrames := frames.Load() - beforeFrames
					measuredCalls := calls.snapshot().subtract(beforeCalls)
					// Sample the hook's server-side registration RPC after the idle
					// interval, under the same open sockets. This does not measure a
					// real agent process, full hook startup, DNS, TLS or a reverse proxy.
					latencies := make([]float64, 20)
					for i := range latencies {
						payload := map[string]any{"client": "claude", "host": "idle-hook-host", "session_id": fmt.Sprintf("hook-%d", i),
							"cwd": "/idle-hook", "worktree": "/idle-hook", "remote_url": "https://example.invalid/idle/iris.git", "root_commit": "idle-root", "source": "hook"}
						body, err := json.Marshal(payload)
						if err != nil {
							b.Fatal(err)
						}
						r, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
						if err != nil {
							b.Fatal(err)
						}
						r.Header.Set("Authorization", "Bearer "+token)
						start := time.Now()
						response, err := server.Client().Do(r)
						if err != nil {
							b.Fatal(err)
						}
						_, readErr := io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
						latencies[i] = float64(time.Since(start)) / float64(time.Millisecond)
						if readErr != nil || response.StatusCode != http.StatusOK {
							b.Fatalf("registration status %d, read error %v", response.StatusCode, readErr)
						}
					}
					sort.Float64s(latencies)
					b.ReportMetric(cpu*60/elapsed, "cpu_s/min")
					b.ReportMetric(float64(transactions)/elapsed, "transactions/s")
					b.ReportMetric(latencies[9], "registration_p50_ms")
					b.ReportMetric(latencies[18], "registration_p95_ms")
					result := map[string]any{"history_scale": scale, "watch_sockets": watches, "legacy_console_sockets": 2, "consumer_id": consumer,
						"started_utc": startedUTC, "idle_ended_utc": start.Add(time.Duration(elapsed * float64(time.Second))).UTC().Format(time.RFC3339Nano),
						"measured_seconds": elapsed, "cpu_seconds": cpu, "cpu_seconds_per_minute": cpu * 60 / elapsed,
						"store_transactions": transactions, "transactions_per_second": float64(transactions) / elapsed,
						"idle_frames": idleFrames, "registration_rpc_samples": len(latencies), "registration_rpc_p50_ms": latencies[9], "registration_rpc_p95_ms": latencies[18],
						"go_version": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "cpu_scope": "whole benchmark process, including loopback clients",
						"fixture": fixture.counts}
					prefix := "IDLE_CPU_RESULT"
					if heartbeats != nil {
						prefix = "LIVE_CPU_RESULT"
						delete(result, "idle_frames")
						result["frames_during_interval"] = idleFrames
						result["measurement_ended_utc"] = result["idle_ended_utc"]
						delete(result, "idle_ended_utc")
						result["workload"] = "20 watch clients, heartbeat every 20 seconds, staggered phases"
						result["heartbeat_clients"] = watches
						result["heartbeat_period_seconds"] = idleBenchmarkHeartbeatPeriod.Seconds()
						result["heartbeat_first_phase_seconds"] = idleBenchmarkHeartbeatPeriod.Seconds() / float64(2*watches)
						result["heartbeat_phase_spacing_seconds"] = idleBenchmarkHeartbeatPeriod.Seconds() / float64(watches)
						result["heartbeat_rpc_attempts"] = heartbeats.attempts.Load()
						result["heartbeat_rpc_successes"] = heartbeats.successes.Load()
						result["heartbeat_rpc_errors"] = 0
						result["heartbeat_samples"] = heartbeats.samples
						result["runtime_num_cpu"] = runtime.NumCPU()
						result["heartbeat_skipped_slots"] = heartbeats.skipped.Load()
						result["heartbeat_operation_calls"] = measuredCalls.heartbeats
						result["watch_observation_calls"] = measuredCalls.events
						result["legacy_console_observation_calls"] = measuredCalls.console
						result["maintenance_calls"] = measuredCalls.expire
						result["hub_operation_calls"] = measuredCalls.total
						result["store_transactions_outside_hub_calls"] = int64(transactions) - int64(measuredCalls.total)
						result["authentication_scope"] = "Every heartbeat authenticates a named host token in its own store transaction; included in total Store transactions. Outside-hub-call residual can include interval-boundary in-flight operations."
						result["measurement_scope"] = "CPU/TPS include heartbeat HTTP clients, authentication, writes, all broadcast observer reads and heartbeat worker stop/join. Registration RPCs follow worker join under the same sockets, without ongoing heartbeats; not full hook/process/proxy latency."
					}
					encoded, err := json.Marshal(result)
					if err != nil {
						b.Fatal(err)
					}
					b.Logf("%s %s", prefix, encoded)
				})
			}
		})
	}
}

const idleBenchmarkHeartbeatPeriod = 20 * time.Second

type idleBenchmarkCallCounts struct {
	total, events, console, expire, heartbeats atomic.Uint64
}

type idleBenchmarkCallSnapshot struct {
	total, events, console, expire, heartbeats uint64
}

func (c *idleBenchmarkCallCounts) record(operation string) {
	c.total.Add(1)
	switch operation {
	case "events":
		c.events.Add(1)
	case "ui_events":
		c.console.Add(1)
	case "expire":
		c.expire.Add(1)
	case "heartbeat":
		c.heartbeats.Add(1)
	}
}

func (c *idleBenchmarkCallCounts) snapshot() idleBenchmarkCallSnapshot {
	return idleBenchmarkCallSnapshot{c.total.Load(), c.events.Load(), c.console.Load(), c.expire.Load(), c.heartbeats.Load()}
}

func (s idleBenchmarkCallSnapshot) subtract(before idleBenchmarkCallSnapshot) idleBenchmarkCallSnapshot {
	return idleBenchmarkCallSnapshot{s.total - before.total, s.events - before.events, s.console - before.console, s.expire - before.expire, s.heartbeats - before.heartbeats}
}

type idleBenchmarkHeartbeatSample struct {
	ExecutionID string  `json:"execution_id"`
	StartedUTC  string  `json:"started_utc"`
	LatencyMS   float64 `json:"latency_ms"`
}

type idleBenchmarkHeartbeats struct {
	samplesMu                    sync.Mutex
	samples                      []idleBenchmarkHeartbeatSample
	stop                         chan struct{}
	stopOnce                     sync.Once
	workers                      sync.WaitGroup
	failures                     chan error
	attempts, successes, skipped atomic.Uint64
}

func startIdleBenchmarkHeartbeats(client *http.Client, base, token string, clients int, start time.Time) *idleBenchmarkHeartbeats {
	run := &idleBenchmarkHeartbeats{stop: make(chan struct{}), failures: make(chan error, clients)}
	for i := range clients {
		run.workers.Add(1)
		go func(i int) {
			defer run.workers.Done()
			execution := fmt.Sprintf("idle-ex-%d", i)
			phase := time.Duration(2*i+1) * idleBenchmarkHeartbeatPeriod / time.Duration(2*clients)
			next := start.Add(phase)
			timer := time.NewTimer(time.Until(next))
			defer timer.Stop()
			for {
				select {
				case <-run.stop:
					return
				case <-timer.C:
				}
				// Prefer stopping over starting another request at the boundary.
				select {
				case <-run.stop:
					return
				default:
				}
				run.attempts.Add(1)
				requestStart := time.Now()
				if err := idleBenchmarkHeartbeat(client, base, token, execution); err != nil {
					// At most one error per worker fits the buffer. The benchmark
					// goroutine reports errors after joining; workers never call b.Fatal.
					run.failures <- fmt.Errorf("heartbeat %s: %w", execution, err)
					return
				}
				run.successes.Add(1)
				run.samplesMu.Lock()
				run.samples = append(run.samples, idleBenchmarkHeartbeatSample{execution, requestStart.UTC().Format(time.RFC3339Nano), float64(time.Since(requestStart)) / float64(time.Millisecond)})
				run.samplesMu.Unlock()
				next = next.Add(idleBenchmarkHeartbeatPeriod)
				// Do not create catch-up bursts if this shared host was stalled.
				for !next.After(time.Now()) {
					run.skipped.Add(1)
					next = next.Add(idleBenchmarkHeartbeatPeriod)
				}
				timer.Reset(time.Until(next))
			}
		}(i)
	}
	return run
}

func (run *idleBenchmarkHeartbeats) stopAndWait() {
	run.stopOnce.Do(func() { close(run.stop) })
	run.workers.Wait()
}

func (run *idleBenchmarkHeartbeats) errors() []error {
	// Called only after stopAndWait, so no producer remains.
	var errors []error
	for {
		select {
		case err := <-run.failures:
			errors = append(errors, err)
		default:
			return errors
		}
	}
}

func idleBenchmarkHeartbeat(client *http.Client, base, token, execution string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/executions/"+url.PathEscape(execution)+"/heartbeat", strings.NewReader(`{"generation":1,"source":"watch"}`))
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(r)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	var out struct {
		ExecutionID string `json:"execution_id"`
		Generation  int64  `json:"generation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		return fmt.Errorf("status %d: %w", response.StatusCode, err)
	}
	if response.StatusCode != http.StatusOK || out.ExecutionID != execution || out.Generation != 1 {
		return fmt.Errorf("status %d, execution %q generation %d", response.StatusCode, out.ExecutionID, out.Generation)
	}
	return nil
}

func idleBenchmarkCPU(b *testing.B) float64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}

type idleFixture struct {
	store  *store.Store
	events int
	counts map[string]int
}

func idleBenchmarkFixture(b *testing.B, scale int) idleFixture {
	b.Helper()
	filename := filepath.Join(b.TempDir(), "idle-benchmark.db")
	s, err := store.Open(filename, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		_ = s.Close()
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	counts := map[string]int{"Agent": 148 * scale, "Execution": 468 * scale, "Claim": 3227 * scale, "Delivery": 2583 * scale, "Message": 1900 * scale, "Event": 1656 * scale,
		"active_executions": 20, "unrevoked_claims": 80, "unacked_deliveries": 200}
	now := time.Now().UnixMilli()
	var epoch int64
	if err := tx.QueryRow("SELECT boot_epoch FROM Meta WHERE id=1").Scan(&epoch); err != nil {
		b.Fatal(err)
	}
	exec := func(query string, args ...any) {
		b.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			b.Fatal(err)
		}
	}
	exec("INSERT INTO Repository(id,root_commit,name) VALUES('idle-repository','idle-root','Idle fixture')")
	exec("INSERT INTO RepositoryAlias(alias,repository_id) VALUES('remote:example.invalid/idle/iris','idle-repository')")
	const series = `WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x+1<?) `
	exec(series+`INSERT INTO Agent(id,name,repository_id,host,client,worktree,slot)
SELECT printf('idle-agent-%d',x),printf('idle-agent-%d',x),'idle-repository','idle-host','codex',printf('/idle/%d',x),1 FROM n`, counts["Agent"])
	exec(series+`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session,last_seen_ms)
	SELECT printf('idle-ex-%d',x),printf('idle-agent-%d',CASE WHEN x<20 THEN x ELSE 20+(x-20)%(?-20) END),'idle-repository','idle-host','codex',printf('idle-session-%d',x),1,printf('/idle/%d',CASE WHEN x<20 THEN x ELSE 20+(x-20)%(?-20) END),'main',?,3600000,CASE WHEN x<20 THEN 'active' ELSE 'ended' END,0,0,1,? FROM n`, counts["Execution"], counts["Agent"], counts["Agent"], epoch, now)
	exec(series+`INSERT INTO Claim(execution_id,repository_id,kind,path,origin,boot_epoch,expires_ms,revoked_ms,path_key)
SELECT printf('idle-ex-%d',CASE WHEN x<80 THEN x%20 ELSE 20+x%(?-20) END),'idle-repository','exact',printf('file-%d',x),'explicit',?,?,CASE WHEN x<80 THEN NULL ELSE ? END,printf('file-%d',x) FROM n`, counts["Claim"], counts["Execution"], epoch, now+3600000, now-3600000)
	exec(series+`INSERT INTO Message(id,sender_execution_id,repository_id,subject,body,created_ms,thread_id)
SELECT printf('idle-message-%d',x),printf('idle-ex-%d',20+x%(?-20)),'idle-repository','Synthetic fixture',?,?,printf('idle-message-%d',x) FROM n`, counts["Message"], counts["Execution"], strings.Repeat("synthetic ", 28), now-3600000)
	exec(series+`INSERT INTO Delivery(id,message_id,recipient_agent_id,execution_id,attempt,attention)
SELECT printf('idle-delivery-%d',x),printf('idle-message-%d',x%?),
	 printf('idle-agent-%d',CASE WHEN x<200 THEN x%20 ELSE 20+(x%(?-20))%(?-20) END),
 printf('idle-ex-%d',CASE WHEN x<200 THEN x%20 ELSE 20+x%(?-20) END),1,0 FROM n`, counts["Delivery"], counts["Message"], counts["Execution"], counts["Agent"], counts["Execution"])
	exec(series+`INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms)
SELECT printf('idle-delivery-%d',x),printf('idle-ex-%d',20+x%(?-20)),1,? FROM n WHERE x>=200`, counts["Delivery"], counts["Execution"], now-3600000)
	exec(series+`INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms)
	SELECT 'inbox_changed',d.recipient_agent_id,NULL,d.id,? FROM n JOIN Delivery d ON d.id=printf('idle-delivery-%d',x%?)`, counts["Event"], now-3600000, counts["Delivery"])
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	for table, want := range counts {
		if strings.Contains(table, "_") {
			continue
		}
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil || got != want {
			b.Fatalf("fixture %s: rows %d, want %d; error %v", table, got, want, err)
		}
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		b.Fatal(err)
	}
	if rows.Next() || rows.Err() != nil {
		b.Fatal("invalid synthetic fixture foreign keys", rows.Err())
	}
	_ = rows.Close()
	return idleFixture{s, counts["Event"], counts}
}
