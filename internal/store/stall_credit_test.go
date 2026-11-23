package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type executionRow struct {
	state       string
	deadline    int64
	renewed     int64
	recovery    sql.NullInt64
	heartbeatOn bool
}

func readExecutionRow(t *testing.T, s *Store, id string) executionRow {
	t.Helper()
	var row executionRow
	if err := s.db.QueryRow("SELECT state,deadline_tick_ms,lease_renewed_tick_ms,recovery_deadline,heartbeat_mode FROM Execution WHERE id=?", id).
		Scan(&row.state, &row.deadline, &row.renewed, &row.recovery, &row.heartbeatOn); err != nil {
		t.Fatal(err)
	}
	return row
}

func startHeartbeatSession(t *testing.T, s *Store, n int, claimPath string) (Request, map[string]any) {
	t.Helper()
	r, reg := session(t, s, n)
	hb := r
	hb.Source = "shim"
	call(t, s, "heartbeat", hb)
	if claimPath != "" {
		call(t, s, "claim", claimRequest(r, "claim-"+claimPath, claimPath, "exact"))
	}
	return r, reg
}

// 5a: After a 70 s jump with the sweep armed, the next heartbeat keeps its generation and claims.
func TestStallCreditKeepsGenerationAndClaimsAfter70sJump(t *testing.T) {
	s, clock, _ := testStore(t)
	a, reg := startHeartbeatSession(t, s, 1, "held.go")
	b, _ := session(t, s, 2)
	repo := reg["repository_id"].(string)

	s.SetStreamSweep(true)
	clock.advance(70 * time.Second)
	call(t, s, "expire", Request{})

	row := readExecutionRow(t, s, a.ExecutionID)
	if row.state != "active" || row.deadline != 90000 {
		t.Fatalf("after 70s stall: %+v, want active with deadline 90000", row)
	}
	claims := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	if len(claims) != 1 || claims[0]["path"] != "held.go" {
		t.Fatalf("claim lost during stall: %v", claims)
	}

	// One client heartbeat interval (20 s) was granted; renewing at +10 s succeeds.
	clock.advance(10 * time.Second)
	hb := a
	hb.Source = "shim"
	out := call(t, s, "heartbeat", hb)
	if number(out["generation"]) != a.Generation {
		t.Fatalf("generation changed: %v, want %d", out["generation"], a.Generation)
	}
	row = readExecutionRow(t, s, a.ExecutionID)
	if row.renewed != 80000 || row.deadline != 140000 {
		t.Fatalf("after renewal: %+v, want renewed=80000 deadline=140000", row)
	}
	if touch := call(t, s, "touch", touchRequest(b, "contested", "held.go")); touch["action"] != "deny" {
		t.Fatalf("claim did not continue blocking peer: %v", touch)
	}
}

// 5b: Without a heartbeat, the deadline is capped at renewed+lease+60s and stepping 1 s past that cap loses the session and revokes its claims.
func TestStallCreditCapsDeadlineAndExpiresPastCap(t *testing.T) {
	s, clock, _ := testStore(t)
	a, reg := startHeartbeatSession(t, s, 1, "capped.go")
	b, _ := session(t, s, 2)
	repo := reg["repository_id"].(string)

	s.SetStreamSweep(true)
	// Jump 110 s: now+20s = 130s, but cap = 0 + 60s + 60s = 120s.
	clock.advance(110 * time.Second)
	call(t, s, "expire", Request{})

	row := readExecutionRow(t, s, a.ExecutionID)
	if row.state != "active" || row.deadline != 120000 {
		t.Fatalf("after 110s stall: %+v, want active with capped deadline 120000", row)
	}

	// Stepping in 1 s sweep ticks stays active up to 119 s.
	for range 9 {
		clock.advance(time.Second)
		call(t, s, "expire", Request{})
	}
	if row = readExecutionRow(t, s, a.ExecutionID); row.state != "active" {
		t.Fatalf("expired before cap at 119s: %+v", row)
	}
	call(t, s, "validate", a)

	// Stepping past the 120 s cap marks the Execution lost and revokes its claims.
	clock.advance(2 * time.Second) // 121 s: 1 s past renewed+lease+60s
	call(t, s, "expire", Request{})

	if row = readExecutionRow(t, s, a.ExecutionID); row.state != "lost" || row.deadline != 120000 {
		t.Fatalf("past cap: %+v, want lost at deadline 120000", row)
	}
	hb := a
	hb.Source = "shim"
	expectError(t, s, "heartbeat", hb, "lost")
	if claims := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any); len(claims) != 0 {
		t.Fatalf("claims not revoked after cap expired: %v", claims)
	}
	call(t, s, "claim", claimRequest(b, "takeover", "capped.go", "exact"))
}

// 5c: A second jump without a heartbeat stops at the same cap, while a heartbeat in between restores a full 60 s.
func TestStallCreditSecondJumpStopsAtCapUnlessHeartbeatRenews(t *testing.T) {
	t.Run("without_heartbeat", func(t *testing.T) {
		s, clock, _ := testStore(t)
		a, reg := startHeartbeatSession(t, s, 1, "nocap.go")
		repo := reg["repository_id"].(string)

		s.SetStreamSweep(true)
		clock.advance(70 * time.Second)
		call(t, s, "expire", Request{})
		if row := readExecutionRow(t, s, a.ExecutionID); row.state != "active" || row.deadline != 90000 {
			t.Fatalf("after first jump: %+v", row)
		}

		// Second jump without heartbeat stops at renewed+lease+60s = 120000.
		clock.advance(40 * time.Second)
		call(t, s, "expire", Request{})
		if row := readExecutionRow(t, s, a.ExecutionID); row.state != "active" || row.deadline != 120000 {
			t.Fatalf("after second jump: %+v, want deadline 120000", row)
		}

		// Third jump past the same cap marks it lost instead of riding repeated stalls.
		clock.advance(11 * time.Second)
		call(t, s, "expire", Request{})
		if row := readExecutionRow(t, s, a.ExecutionID); row.state != "lost" || row.deadline != 120000 {
			t.Fatalf("third jump rode past cap: %+v", row)
		}
		if claims := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any); len(claims) != 0 {
			t.Fatalf("claims survived past cap: %v", claims)
		}
	})

	t.Run("with_heartbeat_in_between", func(t *testing.T) {
		s, clock, _ := testStore(t)
		a, reg := startHeartbeatSession(t, s, 1, "renewed.go")
		repo := reg["repository_id"].(string)

		s.SetStreamSweep(true)
		clock.advance(70 * time.Second)
		call(t, s, "expire", Request{})

		hb := a
		hb.Source = "shim"
		call(t, s, "heartbeat", hb)
		row := readExecutionRow(t, s, a.ExecutionID)
		if row.renewed != 70000 || row.deadline != 130000 {
			t.Fatalf("heartbeat did not restore renewal baseline: %+v", row)
		}

		// New cap is 70000 + 60000 + 60000 = 190000.
		clock.advance(70 * time.Second) // tick = 140000
		call(t, s, "expire", Request{})
		if row = readExecutionRow(t, s, a.ExecutionID); row.state != "active" || row.deadline != 160000 {
			t.Fatalf("after second jump: %+v, want active deadline 160000", row)
		}

		clock.advance(40 * time.Second) // tick = 180000
		call(t, s, "expire", Request{})
		if row = readExecutionRow(t, s, a.ExecutionID); row.state != "active" || row.deadline != 190000 {
			t.Fatalf("after third jump: %+v, want active capped at 190000", row)
		}

		// Still active 1 s before restored cap (189 s), then lost past 190 s.
		clock.advance(9 * time.Second)
		call(t, s, "expire", Request{})
		if row = readExecutionRow(t, s, a.ExecutionID); row.state != "active" {
			t.Fatalf("lost before restored 60s budget ended: %+v", row)
		}
		clock.advance(2 * time.Second) // 191 s
		call(t, s, "expire", Request{})
		if row = readExecutionRow(t, s, a.ExecutionID); row.state != "lost" {
			t.Fatalf("survived past restored cap: %+v", row)
		}
		if claims := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any); len(claims) != 0 {
			t.Fatalf("claims survived past restored cap: %v", claims)
		}
	})
}

// 5d: An execution already due before the gap stays lost.
func TestStallCreditDoesNotSaveExecutionAlreadyDueBeforeGap(t *testing.T) {
	s, clock, _ := testStore(t)
	dueBefore, _ := startHeartbeatSession(t, s, 1, "before.go")
	clock.advance(20 * time.Second)
	dueDuring, _ := startHeartbeatSession(t, s, 2, "during.go")
	clock.advance(20 * time.Second)
	atBoundary, _ := startHeartbeatSession(t, s, 3, "boundary.go")

	// Advance to tick=65000 with sweep disarmed and no calls:
	// dueBefore (deadline=60000) is already due; dueDuring (80000) and atBoundary (100000) are not.
	clock.advance(25 * time.Second)
	s.SetStreamSweep(true) // sets gap start baseline to tick=65000

	clock.advance(25 * time.Second) // tick=90000
	call(t, s, "expire", Request{})

	if row := readExecutionRow(t, s, dueBefore.ExecutionID); row.state != "lost" {
		t.Fatalf("execution already due before gap was credited: %+v", row)
	}
	if row := readExecutionRow(t, s, dueDuring.ExecutionID); row.state != "active" || row.deadline != 110000 {
		t.Fatalf("execution due during gap lost credit: %+v", row)
	}

	// Disarm, advance to exact deadline of atBoundary (100000), re-arm at 100000, then jump 10 s:
	// deadline == prevExpireTick is NOT after prevExpireTick and stays lost.
	s.SetStreamSweep(false)
	clock.advance(10 * time.Second) // tick=100000
	s.SetStreamSweep(true)          // gap baseline = 100000
	clock.advance(10 * time.Second) // tick=110000
	call(t, s, "expire", Request{})
	if row := readExecutionRow(t, s, atBoundary.ExecutionID); row.state != "lost" {
		t.Fatalf("execution due at gap boundary was credited: %+v", row)
	}
}

// 5e: The same jumps with the sweep disarmed are still losses.
func TestStallCreditDoesNotApplyWhenSweepDisarmed(t *testing.T) {
	s, clock, _ := testStore(t)
	a, reg := startHeartbeatSession(t, s, 1, "disarmed.go")
	repo := reg["repository_id"].(string)

	clock.advance(70 * time.Second)
	call(t, s, "expire", Request{})
	if row := readExecutionRow(t, s, a.ExecutionID); row.state != "lost" {
		t.Fatalf("disarmed store credited stall: %+v", row)
	}
	hb := a
	hb.Source = "shim"
	expectError(t, s, "heartbeat", hb, "lost")
	if claims := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any); len(claims) != 0 {
		t.Fatalf("claims not revoked when disarmed: %v", claims)
	}

	// Arming and then disarming also grants no stall credit.
	b, _ := startHeartbeatSession(t, s, 2, "rearmed.go")
	s.SetStreamSweep(true)
	s.SetStreamSweep(false)
	clock.advance(70 * time.Second)
	call(t, s, "expire", Request{})
	if row := readExecutionRow(t, s, b.ExecutionID); row.state != "lost" {
		t.Fatalf("re-disarmed store credited stall: %+v", row)
	}
}

// 5f: A stall jump leaves recovery_deadline unchanged; a recovering execution past 120 s of wall time is still lost, including across a reopen.
func TestStallCreditLeavesRecoveryDeadlineUnchanged(t *testing.T) {
	s, clock, file := testStore(t)
	a, reg := startHeartbeatSession(t, s, 1, "recover.go")
	repo := reg["repository_id"].(string)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil

	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	before := readExecutionRow(t, reopened, a.ExecutionID)
	if !before.recovery.Valid || before.recovery.Int64 != clock.Now().Add(120*time.Second).UnixMilli() {
		_ = reopened.Close()
		t.Fatalf("unexpected recovery deadline: %+v", before)
	}

	reopened.SetStreamSweep(true)
	clock.advance(70 * time.Second)
	call(t, reopened, "expire", Request{})
	mid := readExecutionRow(t, reopened, a.ExecutionID)
	if mid.state != "active" || !mid.recovery.Valid || mid.recovery.Int64 != before.recovery.Int64 {
		_ = reopened.Close()
		t.Fatalf("stall moved recovery_deadline: before=%+v after=%+v", before, mid)
	}

	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened.lock = nil
	reopenedAgain, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopenedAgain.Close() }()

	reopenedAgain.SetStreamSweep(true)
	clock.advance(51 * time.Second) // total 121 s of wall time since recovery began
	call(t, reopenedAgain, "expire", Request{})

	after := readExecutionRow(t, reopenedAgain, a.ExecutionID)
	if after.state != "lost" || !after.recovery.Valid || after.recovery.Int64 != before.recovery.Int64 {
		t.Fatalf("recovering execution past 120s not lost or recovery_deadline moved: %+v", after)
	}
	hb := a
	hb.Source = "shim"
	expectError(t, reopenedAgain, "heartbeat", hb, "lost")
	expectError(t, reopenedAgain, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w1", SessionID: "1", Generation: a.Generation}, "lost")
	if claims := call(t, reopenedAgain, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any); len(claims) != 0 {
		t.Fatalf("claims not revoked after recovery timeout: %v", claims)
	}
}

// 5g: Large wall-clock skew does not extend a lease or count as a stall.
func TestStallCreditIgnoresWallClockSkew(t *testing.T) {
	s, clock, _ := testStore(t)
	a, _ := startHeartbeatSession(t, s, 1, "skew.go")
	s.SetStreamSweep(true)

	// Huge forward wall jump with zero monotonic tick advance must not change deadline.
	clock.wall = clock.wall.Add(365 * 24 * time.Hour)
	call(t, s, "expire", Request{})
	if row := readExecutionRow(t, s, a.ExecutionID); row.state != "active" || row.deadline != 60000 {
		t.Fatalf("forward wall skew moved lease deadline: %+v", row)
	}

	// Huge backward wall jump during normal 1 s ticks must expire at 60 s as normal.
	clock.wall = clock.wall.Add(-730 * 24 * time.Hour)
	for range 59 {
		clock.advance(time.Second)
		call(t, s, "expire", Request{})
	}
	if row := readExecutionRow(t, s, a.ExecutionID); row.state != "active" {
		t.Fatalf("backward wall skew expired lease early: %+v", row)
	}
	clock.advance(time.Second) // tick = 60000
	call(t, s, "expire", Request{})
	if row := readExecutionRow(t, s, a.ExecutionID); row.state != "lost" {
		t.Fatalf("wall skew extended lease past 60s: %+v", row)
	}

	// Even with backward wall skew, a real monotonic stall past the 120 s cap still loses.
	b, _ := startHeartbeatSession(t, s, 2, "skew-cap.go") // renewed=60000, cap=180000
	clock.wall = clock.wall.Add(-24 * time.Hour)
	clock.tick += 130000 // tick = 190000 > cap 180000
	call(t, s, "expire", Request{})
	if row := readExecutionRow(t, s, b.ExecutionID); row.state != "lost" {
		t.Fatalf("negative wall skew bypassed monotonic cap: %+v", row)
	}
}

// 5h: A Call-triggered first expire after the stall gives the same credit as a sweep-triggered one.
func TestCallTriggeredExpireAfterStallMatchesSweepTriggered(t *testing.T) {
	s, clock, _ := testStore(t)
	a, reg := startHeartbeatSession(t, s, 1, "call-a.go")
	b, _ := startHeartbeatSession(t, s, 2, "call-b.go")
	repo := reg["repository_id"].(string)

	s.SetStreamSweep(true)
	clock.advance(70 * time.Second)

	// First expire after the stall comes from a queued heartbeat Call (not "expire").
	hb := a
	hb.Source = "shim"
	out := call(t, s, "heartbeat", hb)
	if number(out["generation"]) != a.Generation {
		t.Fatalf("heartbeat failed after stall: %v", out)
	}
	rowA := readExecutionRow(t, s, a.ExecutionID)
	if rowA.state != "active" || rowA.renewed != 70000 || rowA.deadline != 130000 {
		t.Fatalf("a after heartbeat Call: %+v", rowA)
	}
	// Peer b received identical stall credit from the Call's leading t.expire().
	rowB := readExecutionRow(t, s, b.ExecutionID)
	if rowB.state != "active" || rowB.deadline != 90000 {
		t.Fatalf("b after Call-triggered expire: %+v, want active deadline 90000", rowB)
	}

	// A non-heartbeat Call (validate) also triggers the stall credit for a on a second 70 s stall,
	// while b (whose cap 120000 <= 140000 has run out without a heartbeat) is marked lost.
	clock.advance(70 * time.Second) // tick = 140000
	call(t, s, "validate", a)
	if rowA = readExecutionRow(t, s, a.ExecutionID); rowA.state != "active" || rowA.deadline != 160000 {
		t.Fatalf("a after validate-triggered expire: %+v, want active deadline 160000", rowA)
	}
	if rowB = readExecutionRow(t, s, b.ExecutionID); rowB.state != "lost" {
		t.Fatalf("b should be lost past cap: %+v", rowB)
	}
	claims := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	if len(claims) != 1 || claims[0]["path"] != "call-a.go" {
		t.Fatalf("expected only call-a.go claim remaining: %v", claims)
	}
}

func TestLeaseRenewedTickUpdatedByRegisterResumeHeartbeatAndHookLeaseCap(t *testing.T) {
	s, clock, file := testStore(t)
	// Hook-only session (register sets lease_renewed_tick_ms=0, deadline=900000).
	hookReq, _ := session(t, s, 1)
	row := readExecutionRow(t, s, hookReq.ExecutionID)
	if row.heartbeatOn || row.renewed != 0 || row.deadline != 900000 {
		t.Fatalf("register hook-only: %+v", row)
	}

	// Unrelated Call (claim) does not touch lease_renewed_tick_ms.
	clock.advance(5 * time.Second)
	call(t, s, "claim", claimRequest(hookReq, "c1", "hook.go", "exact"))
	if row = readExecutionRow(t, s, hookReq.ExecutionID); row.renewed != 0 || row.deadline != 900000 {
		t.Fatalf("claim modified lease_renewed_tick_ms: %+v", row)
	}

	// Re-registering active session runs resume and updates lease_renewed_tick_ms.
	clock.advance(5 * time.Second) // tick = 10000
	call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "1", Source: "hook"})
	if row = readExecutionRow(t, s, hookReq.ExecutionID); row.renewed != 10000 || row.deadline != 910000 {
		t.Fatalf("active register/resume: %+v, want renewed=10000 deadline=910000", row)
	}

	// Hook-only session also receives stall credit bounded by 900s + 60s.
	s.SetStreamSweep(true)
	clock.advance(905 * time.Second) // tick = 915000 (past 910000, below cap 10000+900000+60000=970000)
	call(t, s, "expire", Request{})
	if row = readExecutionRow(t, s, hookReq.ExecutionID); row.state != "active" || row.deadline != 935000 {
		t.Fatalf("hook-only stall credit: %+v, want active deadline 935000", row)
	}
	s.SetStreamSweep(false)

	// Reopen puts hookReq in recovery; heartbeat reattachment calls resume and updates lease_renewed_tick_ms.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	clock.advance(5 * time.Second) // tick = 920000
	hb := hookReq
	hb.Source = "watch"
	call(t, reopened, "heartbeat", hb)
	if row = readExecutionRow(t, reopened, hookReq.ExecutionID); !row.heartbeatOn || row.renewed != 920000 || row.deadline != 980000 {
		t.Fatalf("resume on heartbeat reattach: %+v, want renewed=920000 deadline=980000", row)
	}
}

func TestMigration028AddsLeaseRenewedTickAndRollsBack(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := startHeartbeatSession(t, s, 1, "m28.go")
	if _, err := s.db.Exec("ALTER TABLE Execution DROP COLUMN lease_renewed_tick_ms; PRAGMA user_version=27"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil

	f := &storageFault{discover: true}
	name := registerFaultDriver(f)
	if failed, err := openDatabase(file, clock, name); !errors.Is(err, errStorageFault) {
		if failed != nil {
			_ = failed.Close()
		}
		t.Fatal("discovery", err)
	}
	trace := append([]string(nil), f.trace...)
	f.discover = false
	if !strings.Contains(strings.Join(trace, "\n"), "lease_renewed_tick_ms") {
		t.Fatalf("migration 028 missing from fault trace: %v", trace)
	}
	for i, label := range trace {
		f.count, f.at, f.trace = 0, i+1, nil
		failed, err := openDatabase(file, clock, name)
		f.at = 0
		if !errors.Is(err, errStorageFault) {
			if failed != nil {
				_ = failed.Close()
			}
			t.Fatalf("fault %d %s: %v", i+1, label, err)
		}
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 27 {
			_ = raw.Close()
			t.Fatalf("fault %d %s changed version to %d: %v", i+1, label, version, err)
		}
		_ = raw.Close()
	}

	retried, err := openDatabase(file, clock, name)
	if err != nil {
		t.Fatal("migration 028 retry failed", err)
	}
	defer func() { _ = retried.Close() }()
	var version int
	if err = retried.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatalf("version=%d want=%d err=%v", version, latestVersion, err)
	}
	row := readExecutionRow(t, retried, a.ExecutionID)
	if row.renewed != 0 {
		t.Fatalf("migrated default lease_renewed_tick_ms=%d want 0", row.renewed)
	}
}

func TestStallCreditStorageFaultRollbackPreservesLastExpireTick(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := startHeartbeatSession(t, s, 1, "fault.go")
	s.SetStreamSweep(true)
	clock.advance(70 * time.Second)
	before := databaseState(t, s)

	fault := installFaultDriver(t, s, file)
	invoke := func() error {
		_, err := s.Call(context.Background(), "expire", Request{})
		return err
	}
	fault.discover = true
	if err := invoke(); !errors.Is(err, errStorageFault) {
		t.Fatalf("discovery failed to interrupt expire: %v", err)
	}
	trace := append([]string(nil), fault.trace...)
	fault.discover = false
	sawCredit := false
	for _, label := range trace {
		sawCredit = sawCredit || label == creditStallSQL
	}
	if !sawCredit {
		t.Fatalf("fault discovery did not reach stall credit statement: %v", trace)
	}
	for i := range trace {
		fault.at, fault.count, fault.trace = i+1, 0, nil
		err := invoke()
		fault.at = 0
		if !errors.Is(err, errStorageFault) || databaseState(t, s) != before || s.lastExpireTick != 0 {
			t.Fatalf("boundary %d exposed partial state or moved lastExpireTick (%d): %v", i+1, s.lastExpireTick, err)
		}
	}
	if err := invoke(); err != nil {
		t.Fatalf("retry after fault failed: %v", err)
	}
	row := readExecutionRow(t, s, a.ExecutionID)
	if row.state != "active" || row.deadline != 90000 || s.lastExpireTick != 70000 {
		t.Fatalf("retry did not grant stall credit: %+v lastExpireTick=%d", row, s.lastExpireTick)
	}
}

func TestPre028ExecutionGetsNoCreditBeforeResumeAndResetsRenewedTickOnResume(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := startHeartbeatSession(t, s, 1, "pre028.go")
	if _, err := s.db.Exec("ALTER TABLE Execution DROP COLUMN lease_renewed_tick_ms; PRAGMA user_version=27"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil

	// Restart resets the hub's monotonic clock to a fresh origin while wall time keeps moving.
	clock.tick = 0
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	before := readExecutionRow(t, reopened, a.ExecutionID)
	if before.state != "active" || before.renewed != 0 || before.deadline != 60000 || !before.recovery.Valid {
		t.Fatalf("unexpected pre-resume row after Open: %+v", before)
	}

	// Arm the sweep and stall across the old boot's deadline (60000): stall credit must not apply
	// to the recovering pre-028 row before resume.
	reopened.SetStreamSweep(true)
	clock.advance(70 * time.Second) // tick = 70000, still inside 120 s wall recovery window
	call(t, reopened, "expire", Request{})

	mid := readExecutionRow(t, reopened, a.ExecutionID)
	if mid.state != "active" || mid.renewed != 0 || mid.deadline != 60000 || !mid.recovery.Valid || mid.recovery.Int64 != before.recovery.Int64 {
		t.Fatalf("stall credit touched pre-028 row before resume: %+v", mid)
	}

	// Resuming inside the recovery window clears recovery_deadline and writes lease_renewed_tick_ms
	// from the new boot's monotonic tick.
	clock.advance(5 * time.Second) // tick = 75000
	hb := a
	hb.Source = "shim"
	out := call(t, reopened, "heartbeat", hb)
	if number(out["generation"]) != a.Generation {
		t.Fatalf("resume changed generation: %v", out)
	}

	resumed := readExecutionRow(t, reopened, a.ExecutionID)
	if resumed.state != "active" || resumed.recovery.Valid || resumed.renewed != 75000 || resumed.deadline != 135000 {
		t.Fatalf("resume did not establish new-boot renewed tick: %+v, want renewed=75000 deadline=135000", resumed)
	}

	// Subsequent stalls in the new boot credit against the resumed lease_renewed_tick_ms=75000.
	clock.advance(70 * time.Second) // tick = 145000 (past 135000, below cap 75000+60000+60000=195000)
	call(t, reopened, "expire", Request{})
	credited := readExecutionRow(t, reopened, a.ExecutionID)
	if credited.state != "active" || credited.deadline != 165000 || credited.renewed != 75000 {
		t.Fatalf("post-resume stall credit used wrong baseline: %+v", credited)
	}
}
