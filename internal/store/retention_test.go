package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func rowCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sweep(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	out, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ADR 006: what is old enough goes, and what mail or a live session still needs stays.
func TestTheSweepKeepsWhatIsNeededAndDropsWhatIsOld(t *testing.T) {
	s, c, _ := testStore(t)
	sender, _ := session(t, s, 1)
	recipient, ro := session(t, s, 2)
	idle, _ := session(t, s, 3)
	// Mail ties the sender's and the recipient's Executions to its history.
	mail := sender
	mail.Key, mail.Subject, mail.Body = "kept", "s", "b"
	mail.To = []string{ro["agent"].(map[string]any)["name"].(string)}
	call(t, s, "messages", mail)
	// The idle session claims, releases and ends: nothing names it but its own rows.
	call(t, s, "claim", claimRequest(idle, "c", "done", "exact"))
	call(t, s, "release", Request{ExecutionID: idle.ExecutionID, Generation: idle.Generation, All: true})
	call(t, s, "end", idle)
	call(t, s, "end", sender)
	call(t, s, "end", recipient)

	// A day later only idempotency records are old enough.
	c.advance(25 * time.Hour)
	got := sweep(t, s)
	if got["operations"] == 0 || got["events"] != 0 || got["claims"] != 0 || got["executions"] != 0 {
		t.Fatalf("after a day: %v", got)
	}
	// After 31 days events, the finished claim and the idle Execution go; mail and its
	// Executions stay.
	c.advance(30 * 24 * time.Hour)
	got = sweep(t, s)
	// The recipient never fetched, so no delivery names its Execution: its mailbox is the
	// Agent, which stays with the mail.
	if got["events"] == 0 || got["claims"] != 1 || got["executions"] != 2 {
		t.Fatalf("after a month: %v", got)
	}
	if n := rowCount(t, s, "SELECT COUNT(*) FROM Execution WHERE id=?", idle.ExecutionID); n != 0 {
		t.Fatal("the idle Execution stayed")
	}
	if n := rowCount(t, s, "SELECT COUNT(*) FROM Execution WHERE id=?", sender.ExecutionID); n != 1 {
		t.Fatal("the Execution that sent the mail was deleted")
	}
	if n := rowCount(t, s, "SELECT COUNT(*) FROM Message m JOIN Delivery d ON d.message_id=m.id JOIN Agent a ON a.id=d.recipient_agent_id WHERE a.name=?", ro["agent"].(map[string]any)["name"]); n != 1 {
		t.Fatalf("mail or its mailbox was deleted: %d", n)
	}
	// A second sweep finds nothing more.
	if again := sweep(t, s); again["events"]+again["claims"]+again["executions"]+again["operations"] != 0 {
		t.Fatalf("a second sweep removed %v", again)
	}
}

// Nothing a live session needs is removed, however old: its Execution, its live claim, its
// waiter, its cursor, and the events its monitor has not passed.
func TestTheSweepNeverTouchesWhatALiveSessionNeeds(t *testing.T) {
	s, c, _ := testStore(t)
	holder, _ := session(t, s, 1)
	waiter, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "held", "exact"))
	expectError(t, s, "claim", claimRequest(waiter, "wait", "held", "exact"), "conflict")
	monitor := waiter
	monitor.ConsumerID, monitor.After = "iris-watch", 1
	call(t, s, "events", monitor)
	// Two hours on, with both sessions kept alive, events after the cursor are kept.
	for i := 0; i < 240; i++ {
		c.advance(30 * time.Second)
		for _, r := range []Request{holder, waiter} {
			hb := r
			hb.Source = "shim"
			call(t, s, "heartbeat", hb)
		}
	}
	after := rowCount(t, s, "SELECT COUNT(*) FROM Event WHERE seq>1")
	got := sweep(t, s)
	if got["executions"] != 0 || got["claims"] != 0 {
		t.Fatalf("a live session lost rows: %v", got)
	}
	if n := rowCount(t, s, "SELECT COUNT(*) FROM Event WHERE seq>1"); n != after {
		t.Fatalf("events the monitor has not passed were removed: %d of %d left", n, after)
	}
	if rowCount(t, s, "SELECT COUNT(*) FROM Event WHERE seq<=1") != 0 {
		t.Fatal("an old event every live monitor had passed stayed")
	}
	for _, q := range []string{"SELECT COUNT(*) FROM Claim WHERE revoked_ms IS NULL", "SELECT COUNT(*) FROM Waiter", "SELECT COUNT(*) FROM Cursor"} {
		if rowCount(t, s, q) != 1 {
			t.Fatalf("%s: a live session's row was removed", q)
		}
	}
}

// A retry inside its window still finds its record after a sweep.
func TestARetryAfterASweepStillReplays(t *testing.T) {
	s, c, _ := testStore(t)
	a, _ := session(t, s, 1)
	first := call(t, s, "claim", claimRequest(a, "same", "file", "exact"))
	c.advance(10 * time.Minute)
	hb := a
	hb.Source = "shim"
	call(t, s, "heartbeat", hb)
	sweep(t, s)
	if again := call(t, s, "claim", claimRequest(a, "same", "file", "exact")); number(again["fence"]) != number(first["fence"]) {
		t.Fatalf("the retry made a new claim: %v / %v", first, again)
	}
}

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *syncLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The hub sweeps at start and logs what the first sweep removed, which after a deploy can be a
// lot. Later sweeps that remove nothing stay quiet, and it stops with its context.
func TestRunSweepsLogsTheFirstSweepAndStops(t *testing.T) {
	s, c, _ := testStore(t)
	a, _ := session(t, s, 1)
	call(t, s, "claim", claimRequest(a, "c", "f", "exact"))
	c.advance(25 * time.Hour) // the clock is still before the sweeps start, so nothing races it
	log := &syncLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunSweeps(ctx, 5*time.Millisecond, log); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "1 idempotency records") {
		if time.Now().After(deadline) {
			t.Fatalf("the first sweep said %q", log.String())
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // several more ticks, with nothing left to remove
	cancel()
	<-done
	if lines := strings.Count(log.String(), "\n"); lines != 1 {
		t.Fatalf("sweeps that removed nothing spoke: %q", log.String())
	}
}

// A sweep that fails is reported, and the next tick tries again. One the stopping hub cuts
// short is not a failure, and says nothing.
func TestAFailedSweepIsReportedButAStoppedOneIsNot(t *testing.T) {
	s, _, _ := testStore(t)
	_ = s.db.Close()
	log := &syncLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunSweeps(ctx, time.Millisecond, log); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(log.String(), "retention sweep failed") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%q", log.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	stopped := &syncLog{}
	s.RunSweeps(ctx, time.Hour, stopped)
	if stopped.String() != "" {
		t.Fatalf("a stopped hub's sweep spoke: %q", stopped.String())
	}
}

// Mail is never pruned, so the size of the database is in the hub's status.
func TestStatusSaysHowLargeTheDatabaseIs(t *testing.T) {
	s, _, _ := testStore(t)
	if n := number(call(t, s, "status", Request{})["database_bytes"]); n <= 0 {
		t.Fatalf("database_bytes %d", n)
	}
}
