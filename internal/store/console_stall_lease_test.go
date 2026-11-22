package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type lockTiming struct {
	Wait time.Duration
	Held time.Duration
}

type syncFakeClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func newSyncFakeClock(wall time.Time) *syncFakeClock {
	return &syncFakeClock{wall: wall}
}

func (c *syncFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *syncFakeClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}

func (c *syncFakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.tick += d.Milliseconds()
}

type sqlStatementRecord struct {
	Label      string  `json:"label"`
	SQL        string  `json:"sql"`
	WallMS     float64 `json:"wall_ms"`
	PrepExecMS float64 `json:"prep_exec_ms"`
	RowsNextMS float64 `json:"rows_next_ms,omitempty"`
	CloseMS    float64 `json:"close_ms,omitempty"`
	Rows       int     `json:"rows"`
	Args       []any   `json:"-"`
}

type sqlTracer struct {
	mu          sync.Mutex
	recording   bool
	records     []sqlStatementRecord
	onQueryHook func(q string)
}

func (tr *sqlTracer) reset(recording bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.recording = recording
	tr.records = nil
}

func (tr *sqlTracer) snapshot() []sqlStatementRecord {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]sqlStatementRecord, len(tr.records))
	copy(out, tr.records)
	return out
}

func (tr *sqlTracer) setQueryHook(fn func(q string)) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.onQueryHook = fn
}

func (tr *sqlTracer) record(rec sqlStatementRecord) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.recording {
		return
	}
	tr.records = append(tr.records, rec)
}

func (tr *sqlTracer) maybeHook(q string) {
	tr.mu.Lock()
	hook := tr.onQueryHook
	tr.mu.Unlock()
	if hook != nil {
		hook(q)
	}
}

var traceDriverSeq atomic.Uint64

func registerTraceDriver(tr *sqlTracer) string {
	name := fmt.Sprintf("iris-trace-%d", traceDriverSeq.Add(1))
	sql.Register(name, traceDriver{tracer: tr})
	return name
}

type traceDriver struct {
	tracer *sqlTracer
}

func (d traceDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	if _, err = c.(driver.ExecerContext).ExecContext(context.Background(), "PRAGMA foreign_keys=ON", nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	return traceConn{Conn: c, tracer: d.tracer}, nil
}

type traceConn struct {
	driver.Conn
	tracer *sqlTracer
}

func namedValuesToAny(args []driver.NamedValue) []any {
	if len(args) == 0 {
		return nil
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func (c traceConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	start := time.Now()
	res, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	wall := time.Since(start)
	c.tracer.record(sqlStatementRecord{
		Label:      classifyTraceSQL(q),
		SQL:        q,
		WallMS:     durationMS(wall),
		PrepExecMS: durationMS(wall),
		Args:       namedValuesToAny(args),
	})
	return res, err
}

func (c traceConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.tracer.maybeHook(q)
	start := time.Now()
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	prep := time.Since(start)
	if err != nil {
		c.tracer.record(sqlStatementRecord{
			Label:      classifyTraceSQL(q),
			SQL:        q,
			WallMS:     durationMS(time.Since(start)),
			PrepExecMS: durationMS(prep),
			Args:       namedValuesToAny(args),
		})
		return nil, err
	}
	return &traceRows{
		Rows:   rows,
		tracer: c.tracer,
		sql:    q,
		args:   namedValuesToAny(args),
		start:  start,
		prep:   prep,
	}, nil
}

type traceRows struct {
	driver.Rows
	tracer   *sqlTracer
	sql      string
	args     []any
	start    time.Time
	prep     time.Duration
	nextDur  time.Duration
	rowCount int
	once     sync.Once
}

func (r *traceRows) Next(dest []driver.Value) error {
	t0 := time.Now()
	err := r.Rows.Next(dest)
	r.nextDur += time.Since(t0)
	if err == nil {
		r.rowCount++
	}
	return err
}

func (r *traceRows) Close() error {
	t0 := time.Now()
	err := r.Rows.Close()
	closeDur := time.Since(t0)
	wall := time.Since(r.start)
	r.once.Do(func() {
		r.tracer.record(sqlStatementRecord{
			Label:      classifyTraceSQL(r.sql),
			SQL:        r.sql,
			WallMS:     durationMS(wall),
			PrepExecMS: durationMS(r.prep),
			RowsNextMS: durationMS(r.nextDur),
			CloseMS:    durationMS(closeDur),
			Rows:       r.rowCount,
			Args:       r.args,
		})
	})
	return err
}

func durationMS(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

func classifyTraceSQL(q string) string {
	switch q {
	case "BEGIN IMMEDIATE":
		return "begin_immediate"
	case "COMMIT":
		return "commit"
	case "ROLLBACK":
		return "rollback"
	case expireExecutionsSQL:
		return "expire_executions"
	case expiredClaimsSQL:
		return "expire_claims"
	case missedReceiptsSQL:
		return "expire_missed_receipts"
	case expireConsoleSessionsSQL:
		return "expire_console_sessions"
	}
	switch {
	case strings.HasPrefix(q, "SELECT m.thread_id,"):
		return "threads_main_query"
	case strings.HasPrefix(q, "SELECT DISTINCT CASE WHEN a.client='human'"):
		return "thread_participants_query"
	case strings.HasPrefix(q, "SELECT DISTINCT m.sender_account_id"):
		return "thread_authors_query"
	case strings.HasPrefix(q, "SELECT id FROM Repository"):
		return "overview_repositories"
	case strings.HasPrefix(q, "SELECT name,COALESCE("):
		return "overview_repo_access"
	case strings.HasPrefix(q, "SELECT alias FROM RepositoryAlias"):
		return "overview_repo_aliases"
	case strings.Contains(q, "FROM Claim c JOIN Execution e"):
		return "overview_repo_claims"
	case strings.HasPrefix(q, overviewAgentsSQL):
		return "overview_agents"
	case strings.Contains(q, "FROM Delivery d JOIN Agent a ON a.id=d.recipient_agent_id WHERE a.client='human'"):
		return "overview_human_unread"
	case strings.HasPrefix(q, deliveriesSQL):
		return "deliveries_query"
	case strings.HasPrefix(q, "SELECT host,report FROM HostReport"):
		return "doctor_get_query"
	case strings.HasPrefix(q, "SELECT d.id,d.attempt,"):
		return "inbox_query"
	case strings.HasPrefix(q, "UPDATE Delivery SET execution_id="):
		return "inbox_mark_inflight"
	case strings.HasPrefix(q, "SELECT COUNT(*) FROM Delivery WHERE recipient_agent_id="):
		return "inbox_more_count"
	case strings.HasPrefix(q, "UPDATE Execution SET deadline_tick_ms="):
		return "heartbeat_extend"
	case strings.HasPrefix(q, "UPDATE Execution SET last_seen_ms="):
		return "execution_last_seen"
	case strings.HasPrefix(q, executionSelect):
		return "execution_lookup"
	default:
		return "other_sql"
	}
}

type leaseConsequenceResult struct {
	SimulatedJumpSeconds int64   `json:"simulated_jump_s"`
	HeartbeatErrorCode   string  `json:"heartbeat_error_code"`
	HeartbeatMuWaitMS    float64 `json:"heartbeat_mu_wait_ms"`
	HeartbeatMuHeldMS    float64 `json:"heartbeat_mu_held_ms"`
	ThreadsMuHeldMS      float64 `json:"threads_mu_held_ms"`
	HeartbeatExecState   string  `json:"heartbeat_exec_state_after"`
	HookOnlyExecState    string  `json:"hook_only_exec_state_after"`
	HookOnlyValidateOK   bool    `json:"hook_only_validate_ok"`
}

func runLeaseConsequenceScenario(t *testing.T, dbPath string) leaseConsequenceResult {
	t.Helper()
	clock := newSyncFakeClock(time.Unix(1800000000, 0))
	tracer := &sqlTracer{}
	s, err := openDatabase(dbPath, clock, registerTraceDriver(tracer))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Session A enters heartbeat mode (60 s lease) via shim registration.
	regA := Request{
		Client: "codex", Host: "host", SessionID: "lease-60s",
		Cwd: "/repo/w1", Worktree: "/repo/w1",
		RemoteURL: "https://example.invalid/team/repo.git", RootCommit: "root",
		Source: "shim",
	}
	outA := call(t, s, "sessions", regA)
	execA := Request{ExecutionID: outA["execution_id"].(string), Generation: outA["generation"].(int64)}

	// Session B registers as hook-only (15-minute lease).
	regB := Request{
		Client: "claude", Host: "host", SessionID: "lease-15m",
		Cwd: "/repo/w2", Worktree: "/repo/w2",
		RemoteURL: "https://example.invalid/team/repo.git", RootCommit: "root",
		Source: "hook",
	}
	outB := call(t, s, "sessions", regB)
	execB := Request{ExecutionID: outB["execution_id"].(string), Generation: outB["generation"].(int64)}
	agentA := outA["agent"].(map[string]any)["name"].(string)

	// One message involving human so ui_threads executes its full query path.
	call(t, s, "ui_messages", Request{
		Key: "seed-thread", RepositoryID: outA["repository_id"].(string),
		To: []string{agentA}, Subject: "stall test", Body: "synthetic body",
	})

	// 5 s after registration, session A still has 55 s of its 60 s lease remaining.
	clock.advance(5 * time.Second)

	threadsInQuery := make(chan struct{})
	releaseThreads := make(chan struct{})
	var hookOnce sync.Once
	tracer.setQueryHook(func(q string) {
		if strings.HasPrefix(q, "SELECT m.thread_id,") {
			hookOnce.Do(func() {
				close(threadsInQuery)
				<-releaseThreads
			})
		}
	})

	var threadsTiming lockTiming
	threadsDone := make(chan error, 1)
	go func() {
		ctx := withLockObserver(context.Background(), lockObserver{
			afterUnlock: func(wait, held time.Duration) {
				threadsTiming = lockTiming{Wait: wait, Held: held}
			},
		})
		_, callErr := s.Call(ctx, "ui_threads", Request{RepositoryID: "", Agent: "human", Limit: 50})
		threadsDone <- callErr
	}()

	<-threadsInQuery

	hbQueuing := make(chan struct{})
	var hbTiming lockTiming
	hbDone := make(chan error, 1)
	go func() {
		ctx := withLockObserver(context.Background(), lockObserver{
			beforeLock: func() { close(hbQueuing) },
			afterUnlock: func(wait, held time.Duration) {
				hbTiming = lockTiming{Wait: wait, Held: held}
			},
		})
		_, callErr := s.Call(ctx, "heartbeat", Request{
			ExecutionID: execA.ExecutionID,
			Generation:  execA.Generation,
			Source:      "shim",
		})
		hbDone <- callErr
	}()

	<-hbQueuing
	// Give the goroutine a brief moment to block inside s.mu.Lock() so mu_wait is measurable,
	// then jump the fake clock 57 s while ui_threads holds Store.mu.
	time.Sleep(5 * time.Millisecond)
	tracer.reset(true)
	clock.advance(57 * time.Second)
	close(releaseThreads)
	tracer.setQueryHook(nil)

	if err := <-threadsDone; err != nil {
		t.Fatalf("ui_threads failed: %v", err)
	}
	hbErr := <-hbDone
	stmts := tracer.snapshot()
	tracer.reset(false)

	var domainErr *Error
	if !errors.As(hbErr, &domainErr) || domainErr.Code != "lost" {
		t.Fatalf("expected queued 60s heartbeat to fail with lost after 57s stall, got %v", hbErr)
	}

	expireMarkedInHeartbeat := false
	for _, st := range stmts {
		if st.Label == "expire_executions" && st.Rows == 1 {
			expireMarkedInHeartbeat = true
			break
		}
	}
	if !expireMarkedInHeartbeat {
		t.Fatalf("expected t.expire() inside queued heartbeat to mark 1 execution lost before renewal; statements=%+v", stmts)
	}

	// Because the failed heartbeat rolled back its own transaction, the next committing Call
	// (validating the 15-minute hook-only session) runs t.expire() and persists session A's loss
	// while session B survives and passes validation.
	if _, err := s.Call(context.Background(), "validate", execB); err != nil {
		t.Fatalf("15m hook-only session did not survive 57s stall: %v", err)
	}

	var stateA, stateB string
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", execA.ExecutionID).Scan(&stateA); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", execB.ExecutionID).Scan(&stateB); err != nil {
		t.Fatal(err)
	}
	if stateA != "lost" {
		t.Fatalf("expected 60s heartbeat session state 'lost', got %q", stateA)
	}
	if stateB != "active" {
		t.Fatalf("expected 15m hook-only session state 'active', got %q", stateB)
	}
	if hbTiming.Wait <= 0 || threadsTiming.Held <= 0 {
		t.Fatalf("expected positive lock wait/held timings, got hb.Wait=%v threads.Held=%v", hbTiming.Wait, threadsTiming.Held)
	}

	return leaseConsequenceResult{
		SimulatedJumpSeconds: 57,
		HeartbeatErrorCode:   domainErr.Code,
		HeartbeatMuWaitMS:    durationMS(hbTiming.Wait),
		HeartbeatMuHeldMS:    durationMS(hbTiming.Held),
		ThreadsMuHeldMS:      durationMS(threadsTiming.Held),
		HeartbeatExecState:   stateA,
		HookOnlyExecState:    stateB,
		HookOnlyValidateOK:   true,
	}
}

// TestConsoleStallLeaseConsequence proves with a fake clock that holding Store.mu in
// ui_threads across a 57 s tick jump causes a queued 60 s heartbeat to expire inside
// t.expire() before renewing, while a 15-minute hook-only lease survives.
func TestConsoleStallLeaseConsequence(t *testing.T) {
	res := runLeaseConsequenceScenario(t, t.TempDir()+"/lease-consequence.db")
	if res.HeartbeatErrorCode != "lost" || res.HeartbeatExecState != "lost" || res.HookOnlyExecState != "active" || !res.HookOnlyValidateOK {
		t.Fatalf("unexpected lease consequence result: %+v", res)
	}
}
