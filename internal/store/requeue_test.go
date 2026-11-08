package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// attentionSetup sends one message from a to b, has b fetch it, and returns what the tests
// need: both sessions, b's agent name, the delivery and the newest event cursor.
func attentionSetup(t *testing.T, s *Store) (a, b Request, delivery string, high int64) {
	t.Helper()
	a, _ = session(t, s, 1)
	b, br := session(t, s, 2)
	name := br["agent"].(map[string]any)["name"].(string)
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m", To: []string{name}, Body: "x"})
	fetched := call(t, s, "inbox", b)["deliveries"].([]map[string]any)
	if len(fetched) != 1 || number(fetched[0]["attempt"]) != 1 {
		t.Fatal(fetched)
	}
	return a, b, fetched[0]["id"].(string), number(call(t, s, "events", b)["high_water"])
}

// wakes counts the inbox_changed events after cursor that still need b's attention.
func wakes(t *testing.T, s *Store, b Request, after int64) int {
	t.Helper()
	r := b
	r.After = after
	n := 0
	for _, e := range call(t, s, "events", r)["events"].([]map[string]any) {
		if e["kind"] == "inbox_changed" && e["settled"] != true {
			n++
		}
	}
	return n
}

// reattach registers session 2 with its generation, as its client does after the hub
// restarts. replace registers it with none, as a new session of the same agent does once
// the old one is lost.
func reattach(t *testing.T, s *Store) Request { return register2(t, s, 1) }
func replace(t *testing.T, s *Store) Request  { return register2(t, s, 0) }
func register2(t *testing.T, s *Store, generation int64) Request {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "2", Source: "hook", Generation: generation})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
}

// A hub restart must not wake anyone for mail a live session already fetched: before, the
// restart freed every delivery, which would have made all read-but-unacked mail look new.
func TestARestartWakesNoOneForMailAlreadyFetched(t *testing.T) {
	s, c, file := testStore(t)
	_, _, _, high := attentionSetup(t, s)
	_ = s.Close()
	s.lock = nil
	n, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	b := reattach(t, n)
	if out := call(t, n, "events", b); out["pending"] != 0 {
		t.Fatalf("pending after restart: %v", out["pending"])
	}
	if got := wakes(t, n, b, high); got != 0 {
		t.Fatalf("%d wakes after a restart for mail already fetched", got)
	}
	if got := call(t, n, "monitor_cursor", b)["pending"]; got != 0 {
		t.Fatalf("a new monitor was told about fetched mail: %v", got)
	}
}

// A session that recovers after a restart keeps its delivery and settles it with the attempt it
// fetched, without fetching again.
func TestARecoveringSessionAcknowledgesWhatItFetchedBeforeTheRestart(t *testing.T) {
	s, c, file := testStore(t)
	_, _, delivery, _ := attentionSetup(t, s)
	_ = s.Close()
	s.lock = nil
	n, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	b := reattach(t, n)
	ack := b
	ack.Key, ack.Attempt = delivery, 1
	call(t, n, "ack", ack)
	if left := call(t, n, "inbox", b)["deliveries"].([]map[string]any); len(left) != 0 {
		t.Fatalf("acknowledged mail fetched again: %v", left)
	}
}

// The fetching session's lease runs out before it acknowledges. The mail is requeued: its
// recipient is woken once, and the agent's next session fetches it with a new attempt.
func TestALostFetcherRequeuesItsMailAndWakesTheRecipientOnce(t *testing.T) {
	s, c, _ := testStore(t)
	_, old, delivery, high := attentionSetup(t, s)
	c.tick += 900001 // past a hook session's lease
	b := replace(t, s)
	if b.ExecutionID == old.ExecutionID {
		t.Fatal("the lost session was not replaced")
	}
	if got := wakes(t, s, b, high); got != 1 {
		t.Fatalf("%d wakes for one requeued delivery, want 1", got)
	}
	if out := call(t, s, "events", b); out["pending"] != 1 {
		t.Fatalf("pending %v, want 1", out["pending"])
	}
	call(t, s, "validate", b) // expire runs again and must not announce it twice
	if got := wakes(t, s, b, high); got != 1 {
		t.Fatalf("%d wakes after a second expiry pass, want 1", got)
	}
	again := call(t, s, "inbox", b)["deliveries"].([]map[string]any)
	if len(again) != 1 || again[0]["id"] != delivery || number(again[0]["attempt"]) != 2 {
		t.Fatalf("the next session did not get the requeued mail: %v", again)
	}
	if got := wakes(t, s, b, high); got != 0 {
		t.Fatalf("fetched mail still wakes: %d", got)
	}
	stale := b
	stale.Key, stale.Attempt = delivery, 1
	expectError(t, s, "ack", stale, "conflict")
	stale.Attempt = 2
	call(t, s, "ack", stale)
}

// A session that never comes back after a restart loses its deliveries at the recovery
// deadline, and the mail is requeued then, not at the restart.
func TestMailOfASessionThatNeverRecoversIsRequeuedAtTheDeadline(t *testing.T) {
	s, c, file := testStore(t)
	_, _, delivery, high := attentionSetup(t, s)
	_ = s.Close()
	s.lock = nil
	n, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	c.advance(121 * time.Second)
	b := replace(t, n)
	if got := wakes(t, n, b, high); got != 1 {
		t.Fatalf("%d wakes at the recovery deadline, want 1", got)
	}
	again := call(t, n, "inbox", b)["deliveries"].([]map[string]any)
	if len(again) != 1 || again[0]["id"] != delivery || number(again[0]["attempt"]) != 2 {
		t.Fatalf("the agent's next session did not get the mail: %v", again)
	}
}

// A version 7 database gains Delivery.attention, set only on deliveries nobody has fetched.
// A fault at any step of that migration leaves the database at version 7 without the column.
func TestAttentionMigrationBackfillsAndRollsBackAtEveryStep(t *testing.T) {
	s, _, file := testStore(t)
	a, _ := session(t, s, 1)
	b, br := session(t, s, 2)
	cs, cr := session(t, s, 3)
	to := []string{br["agent"].(map[string]any)["name"].(string), cr["agent"].(map[string]any)["name"].(string)}
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m", To: to, Body: "x"})
	fetched := call(t, s, "inbox", b)["deliveries"].([]map[string]any)[0]["id"].(string)
	ack := cs
	read := call(t, s, "inbox", cs)["deliveries"].([]map[string]any)[0]
	ack.Key, ack.Attempt = read["id"].(string), 1
	call(t, s, "ack", ack)
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "n", To: to[:1], Body: "y"})
	_ = s.Close()
	s.lock = nil
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"ALTER TABLE Delivery DROP COLUMN attention", "ALTER TABLE Repository DROP COLUMN contact_agent_id", "ALTER TABLE Execution DROP COLUMN watch_seen_ms", "ALTER TABLE Execution DROP COLUMN hook_seen_ms", "ALTER TABLE Operation DROP COLUMN created_ms", "DROP VIEW DeliveryState", "ALTER TABLE Ack DROP COLUMN handled_ms", "PRAGMA user_version=7"} {
		if _, err = raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()

	f := &storageFault{discover: true}
	name := registerFaultDriver(f)
	if n, err := openDatabase(file, nil, name); !errors.Is(err, errStorageFault) {
		if n != nil {
			_ = n.Close()
		}
		t.Fatal("discovery", err)
	}
	trace := append([]string(nil), f.trace...)
	f.discover = false
	if !strings.Contains(strings.Join(trace, "\n"), "attention") {
		t.Fatalf("the sweep did not reach the migration: %v", trace)
	}
	for i, label := range trace {
		f.count, f.at, f.trace = 0, i+1, nil
		n, err := openDatabase(file, nil, name)
		f.at = 0
		if !errors.Is(err, errStorageFault) {
			if n != nil {
				_ = n.Close()
			}
			t.Fatalf("fault %d %s: %v", i+1, label, err)
		}
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		var version, column int
		if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 7 {
			t.Fatalf("fault %d %s left version %d: %v", i+1, label, version, err)
		}
		if err = raw.QueryRow("SELECT COUNT(*) FROM pragma_table_info('Delivery') WHERE name='attention'").Scan(&column); err != nil || column != 0 {
			t.Fatalf("fault %d %s left the column: %v", i+1, label, err)
		}
		_ = raw.Close()
	}
	n, err := openDatabase(file, nil, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	got := map[string]int{}
	rows, err := n.db.Query("SELECT id,attention FROM Delivery")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var attention int
		if err = rows.Scan(&id, &attention); err != nil {
			t.Fatal(err)
		}
		got[id] = attention
	}
	_ = rows.Close()
	if len(got) != 3 || got[fetched] != 0 || got[ack.Key] != 0 {
		t.Fatalf("backfill %v: fetched and acknowledged mail must need no attention", got)
	}
	waiting := 0
	for _, v := range got {
		waiting += v
	}
	if waiting != 1 {
		t.Fatalf("backfill %v: only the unfetched delivery needs attention", got)
	}
}
