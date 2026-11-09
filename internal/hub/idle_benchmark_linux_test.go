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
	for _, scale := range []int{1, 10} {
		b.Run(fmt.Sprintf("history=%d", scale), func(b *testing.B) {
			for _, watches := range []int{1, 5, 20} {
				b.Run(fmt.Sprintf("watches=%d", watches), func(b *testing.B) {
					b.StopTimer()
					fixture := idleBenchmarkFixture(b, scale)
					server := httptest.NewUnstartedServer(nil)
					h := New(fixture.store, "idle-owner-secret", []string{server.Listener.Addr().String()})
					h.Log = log.New(io.Discard, "", 0)
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
					start := time.Now()
					startedUTC := start.UTC().Format(time.RFC3339Nano)
					b.StartTimer()
					for range b.N {
						time.Sleep(duration)
					}
					b.StopTimer()
					elapsed := time.Since(start).Seconds()
					cpu := idleBenchmarkCPU(b) - beforeCPU
					transactions := fixture.store.TransactionCount() - beforeTransactions
					idleFrames := frames.Load() - beforeFrames
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
					encoded, err := json.Marshal(result)
					if err != nil {
						b.Fatal(err)
					}
					b.Logf("IDLE_CPU_RESULT %s", encoded)
				})
			}
		})
	}
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
	defer db.Close()
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
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
