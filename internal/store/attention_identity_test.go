package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// attentionOf is the attention block the hub gives r's Execution now.
func attentionOf(t *testing.T, s *Store, r Request) map[string]any {
	t.Helper()
	var out map[string]any
	if err := s.transact(context.Background(), func(tx *transaction) error {
		e, err := tx.live(r.ExecutionID, r.Generation)
		if err != nil {
			return err
		}
		out, err = tx.attention(e)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// identityCursor is the identity's cursor, or 0 when the hub says none is pending. The identity
// field is present either way, and any identity that is not null names this hub's stream.
func identityCursor(t *testing.T, s *Store, block map[string]any) int64 {
	t.Helper()
	identity, present := block["identity"]
	if !present {
		t.Fatalf("no identity field: %v", block)
	}
	if identity == nil {
		return 0
	}
	id := identity.(map[string]any)
	if id["stream"] != s.stream {
		t.Fatalf("an identity of another stream: %v", id)
	}
	return id["cursor"].(int64)
}

// newestEvent is the cursor of the newest event of kind.
func newestEvent(t *testing.T, s *Store, kind string) int64 {
	t.Helper()
	var seq int64
	if err := s.db.QueryRow("SELECT COALESCE(MAX(seq),0) FROM Event WHERE kind=?", kind).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// stampOf is a delivery's attention cursor.
func stampOf(t *testing.T, db *sql.DB, delivery string) sql.NullInt64 {
	t.Helper()
	var seq sql.NullInt64
	if err := db.QueryRow("SELECT attention_seq FROM Delivery WHERE id=?", delivery).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func mailTo(from Request, key, name string) Request {
	m := from
	m.Key, m.To, m.Subject, m.Body = key, []string{name}, key, "x"
	return m
}

// twoSessions is two sessions, a and b, and the name of b's agent.
func twoSessions(t *testing.T, s *Store) (a, b Request, name string) {
	t.Helper()
	a, _ = session(t, s, 1)
	b, joined := session(t, s, 2)
	return a, b, joined["agent"].(map[string]any)["name"].(string)
}

// The identity names the newest event still pending attention, and there is none once everything
// is handled. Handling one message and receiving another leave the count unchanged but give a new
// identity. The counts alone cannot show that change (ADR 017, decision 4).
func TestTheIdentityIsTheNewestPendingMail(t *testing.T) {
	s, _, _ := testStore(t)
	a, b, name := twoSessions(t, s)
	if got := identityCursor(t, s, attentionOf(t, s, b)); got != 0 {
		t.Fatalf("an identity with nothing pending: %d", got)
	}
	call(t, s, "messages", mailTo(a, "one", name))
	first := newestEvent(t, s, "inbox_changed")
	if got := attentionOf(t, s, b); identityCursor(t, s, got) != first || got["unread"] != 1 {
		t.Fatalf("%v, want cursor %d", got, first)
	}
	call(t, s, "messages", mailTo(a, "two", name))
	if got := identityCursor(t, s, attentionOf(t, s, b)); got != newestEvent(t, s, "inbox_changed") || got <= first {
		t.Fatalf("the newer mail is not the identity: %d", got)
	}
	for _, d := range call(t, s, "inbox", b)["deliveries"].([]map[string]any) {
		call(t, s, "ack", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Attempt: number(d["attempt"]), Key: d["id"].(string)})
	}
	if got := attentionOf(t, s, b); identityCursor(t, s, got) != 0 || got["unread"] != 0 {
		t.Fatalf("an identity after everything was handled: %v", got)
	}
	call(t, s, "messages", mailTo(a, "three", name))
	if got := attentionOf(t, s, b); identityCursor(t, s, got) != newestEvent(t, s, "inbox_changed") || got["unread"] != 1 {
		t.Fatalf("one handled and one new: %v", got)
	}
}

// Every committed inbox_changed puts its cursor on its delivery, so a missed receipt's recovery,
// a requeue and a handover each give a new identity; the first leaves the count and the
// delivery's attention as they were.
func TestEveryInboxChangedMovesTheIdentity(t *testing.T) {
	t.Run("a missed receipt", func(t *testing.T) {
		s, clock, _ := testStore(t)
		a, b, name := twoSessions(t, s)
		call(t, s, "messages", mailTo(a, "m", name))
		asked := b
		asked.Receipt = true
		delivery := call(t, s, "inbox", asked)["deliveries"].([]map[string]any)[0]["id"].(string)
		before := attentionOf(t, s, b)
		clock.advance(receiptWindow + time.Second)
		call(t, s, "status", Request{})
		after := attentionOf(t, s, b)
		var attention int
		if err := s.db.QueryRow("SELECT attention FROM Delivery WHERE id=?", delivery).Scan(&attention); err != nil {
			t.Fatal(err)
		}
		if identityCursor(t, s, after) != newestEvent(t, s, "inbox_changed") || identityCursor(t, s, after) <= identityCursor(t, s, before) || after["unread"] != before["unread"] || attention != 1 {
			t.Fatalf("before %v, after %v, attention %d", before, after, attention)
		}
	})
	t.Run("a requeue", func(t *testing.T) {
		s, c, _ := testStore(t)
		_, _, delivery, _ := attentionSetup(t, s)
		c.tick += 900001 // past the fetcher's lease
		b := replace(t, s)
		stamp := stampOf(t, s.db, delivery)
		if got := identityCursor(t, s, attentionOf(t, s, b)); got != stamp.Int64 || got != newestEvent(t, s, "inbox_changed") {
			t.Fatalf("identity %d, stamp %v", got, stamp)
		}
	})
	t.Run("a handover", func(t *testing.T) {
		s, _, _ := testStore(t)
		sender, _ := session(t, s, 9)
		at := Request{Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", ClientPID: 4242, ClientPIDStartMS: 77}
		strayAt, hookAt := at, at
		strayAt.SessionID, strayAt.Source = "session-b", "mcp"
		hookAt.SessionID, hookAt.Source = "session-a", "hook"
		stray, hook := call(t, s, "sessions", strayAt), call(t, s, "sessions", hookAt)
		call(t, s, "messages", mailTo(sender, "m", stray["agent"].(map[string]any)["name"].(string)))
		sent := newestEvent(t, s, "inbox_changed")
		call(t, s, "end", Request{ExecutionID: stray["execution_id"].(string), Generation: stray["generation"].(int64), HandOver: hook["execution_id"].(string)})
		hookReq := Request{ExecutionID: hook["execution_id"].(string), Generation: hook["generation"].(int64)}
		if got := identityCursor(t, s, attentionOf(t, s, hookReq)); got != newestEvent(t, s, "inbox_changed") || got <= sent {
			t.Fatalf("the handed-over mail's identity %d, sent at %d", got, sent)
		}
	})
}

// Retention deletes events; the identity of mail that still needs attention stays.
func TestTheIdentityOutlivesItsEvent(t *testing.T) {
	s, _, _ := testStore(t)
	a, b, name := twoSessions(t, s)
	call(t, s, "messages", mailTo(a, "m", name))
	before := identityCursor(t, s, attentionOf(t, s, b))
	if err := s.PruneEvents(context.Background(), newestEvent(t, s, "inbox_changed")); err != nil {
		t.Fatal(err)
	}
	if newestEvent(t, s, "inbox_changed") != 0 {
		t.Fatal("the event was not deleted")
	}
	if got := identityCursor(t, s, attentionOf(t, s, b)); got != before {
		t.Fatalf("identity %d after its event went, want %d", got, before)
	}
}

// A freed path stays in the identity until retention removes its event, even after the session
// took the path: availability is historical, as its count already was.
func TestAFreedPathStaysInTheIdentityUntilRetention(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(a, "held", "f.txt", "exact"))
	expectError(t, s, "claim", claimRequest(b, "want", "f.txt", "exact"), "conflict")
	call(t, s, "release", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, All: true})
	freed := newestEvent(t, s, "claim_available")
	if got := attentionOf(t, s, b); identityCursor(t, s, got) != freed || got["available"] != 1 {
		t.Fatalf("%v, want cursor %d", got, freed)
	}
	call(t, s, "claim", claimRequest(b, "took", "f.txt", "exact"))
	if got := identityCursor(t, s, attentionOf(t, s, b)); got != freed {
		t.Fatalf("taking the path changed the identity to %d", got)
	}
	if err := s.PruneEvents(context.Background(), freed); err != nil {
		t.Fatal(err)
	}
	if got := attentionOf(t, s, b); identityCursor(t, s, got) != 0 || got["available"] != 0 {
		t.Fatalf("after retention: %v", got)
	}
}

// Heartbeats, registrations and the general changed event never count.
func TestHeartbeatsRegistrationsAndChangesLeaveTheIdentity(t *testing.T) {
	s, _, _ := testStore(t)
	a, b, name := twoSessions(t, s)
	call(t, s, "messages", mailTo(a, "m", name))
	before := identityCursor(t, s, attentionOf(t, s, b))
	call(t, s, "heartbeat", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Source: "shim"})
	register2(t, s, b.Generation)
	call(t, s, "claim", claimRequest(a, "c", "g.txt", "exact"))
	if newestEvent(t, s, "changed") <= before {
		t.Fatal("no changed event came after the mail")
	}
	if got := identityCursor(t, s, attentionOf(t, s, b)); got != before {
		t.Fatalf("identity %d, want %d", got, before)
	}
}

// Migration 023 gives mail that still needs attention the cursor of its newest surviving event, or
// the event sequence's high-water mark when retention removed it; handled mail gets none.
func TestTheMigrationStampsMailThatNeedsAttention(t *testing.T) {
	s, clock, file := historicalStore(t, 23)
	a, b, name := twoSessions(t, s)
	deliveries := map[string]string{}
	for _, key := range []string{"kept", "pruned", "handled"} {
		call(t, s, "messages", mailTo(a, key, name))
		var id string
		if err := s.db.QueryRow("SELECT d.id FROM Delivery d JOIN Message m ON m.id=d.message_id WHERE m.subject=?", key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		deliveries[key] = id
	}
	var kept int64
	if err := s.db.QueryRow("SELECT MAX(seq) FROM Event WHERE object_id=?", deliveries["kept"]).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM Event WHERE object_id=?", []any{deliveries["pruned"]}},
		{"INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms) VALUES(?,?,1,1)", []any{deliveries["handled"], b.ExecutionID}},
		{removeAttentionIdentitySchema + "PRAGMA user_version=22", nil},
	} {
		if _, err := s.db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	var high int64
	if err := s.db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='Event'").Scan(&high); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	upgraded, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	if got := stampOf(t, upgraded.db, deliveries["kept"]); got.Int64 != kept {
		t.Fatalf("kept %v, want %d", got, kept)
	}
	if got := stampOf(t, upgraded.db, deliveries["pruned"]); got.Int64 != high || high <= kept {
		t.Fatalf("pruned %v, want the high-water mark %d", got, high)
	}
	if got := stampOf(t, upgraded.db, deliveries["handled"]); got.Valid {
		t.Fatalf("handled mail got a stamp: %v", got)
	}
}

// An older hub that runs on this database after a rollback writes deliveries and their events as
// it always did, and knows nothing of attention_seq. The database puts each event's cursor on its
// delivery as the event is inserted, so retention can delete the events before this hub starts
// again and the identity stays: a requeue and a new message alike.
func TestTheDatabaseStampsMailAnOlderHubWakes(t *testing.T) {
	s, clock, file := testStore(t)
	a, b, name := twoSessions(t, s)
	call(t, s, "messages", mailTo(a, "requeued", name))
	var requeued, agent, repo string
	if err := s.db.QueryRow("SELECT d.id,d.recipient_agent_id,m.repository_id FROM Delivery d JOIN Message m ON m.id=d.message_id WHERE m.subject='requeued'").Scan(&requeued, &agent, &repo); err != nil {
		t.Fatal(err)
	}
	// The older hub's own statements, as its requeue and its send run them.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE Delivery SET execution_id=NULL,attention=1,receipt_due_ms=NULL WHERE id=?", []any{requeued}},
		{"INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) VALUES('inbox_changed',?,NULL,?,1)", []any{agent, requeued}},
		{"INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id) VALUES('ms_older',?,'older','x',1,?,'ms_older')", []any{a.ExecutionID, repo}},
		{"INSERT INTO Delivery(id,message_id,recipient_agent_id,attention) VALUES('dl_older','ms_older',?,1)", []any{agent}},
		{"INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) VALUES('inbox_changed',?,NULL,'dl_older',1)", []any{agent}},
	} {
		if _, err := s.db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	var woken, created int64
	if err := s.db.QueryRow("SELECT (SELECT MAX(seq) FROM Event WHERE object_id=?),(SELECT MAX(seq) FROM Event WHERE object_id='dl_older')", requeued).Scan(&woken, &created); err != nil {
		t.Fatal(err)
	}
	if stampOf(t, s.db, requeued).Int64 != woken || stampOf(t, s.db, "dl_older").Int64 != created || created <= woken {
		t.Fatalf("not stamped as inserted: requeued %v want %d, new %v want %d", stampOf(t, s.db, requeued), woken, stampOf(t, s.db, "dl_older"), created)
	}
	if err := s.PruneEvents(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	if got := identityCursor(t, s, attentionOf(t, s, b)); got != created {
		t.Fatalf("identity %d after retention, want %d", got, created)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	again, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if got := stampOf(t, again.db, requeued); got.Int64 != woken {
		t.Fatalf("requeued %v after a start, want %d", got, woken)
	}
	if got := stampOf(t, again.db, "dl_older"); got.Int64 != created {
		t.Fatalf("new %v after a start, want %d", got, created)
	}
}

// A restore keeps the stream id, because a backup copies the database whole, and takes the event
// sequence back. The restored identity is then older than one a client was told after the
// backup, so it would look told; the told cursor above the block's high_water shows that the hub
// went back (ADR 006, decision 7). That holds until high_water reaches the told cursor.
func TestARestoreShowsAsACursorAboveTheHighWaterMark(t *testing.T) {
	s, clock, file := testStore(t)
	a, b, name := twoSessions(t, s)
	call(t, s, "messages", mailTo(a, "in the backup", name))
	copied := filepath.Join(t.TempDir(), "backup.db")
	if _, err := s.db.Exec("VACUUM INTO ?", copied); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"one", "two", "three", "four", "five"} {
		call(t, s, "messages", mailTo(a, key, name))
	}
	told := attentionOf(t, s, b)
	cursor := identityCursor(t, s, told)
	if high := told["high_water"].(int64); high < cursor {
		t.Fatalf("high_water %d below the cursor %d it covers", high, cursor)
	}
	stream := s.stream
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	_ = os.Remove(file + "-wal")
	_ = os.Remove(file + "-shm")
	if err := os.Rename(copied, file); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	block := attentionOf(t, restored, register2(t, restored, b.Generation))
	if got := identityCursor(t, restored, block); restored.stream != stream || got == 0 || got >= cursor || block["high_water"].(int64) >= cursor {
		t.Fatalf("stream %s (was %s), block %v, told cursor %d", restored.stream, stream, block, cursor)
	}
}

// Every successful reconcile answer carries the attention block, and an events read carries it
// only when it asks, so a watch socket's own reads pay nothing for it.
func TestReconcileAndAskingEventReadsCarryAttention(t *testing.T) {
	s, _, _ := testStore(t)
	a, b, name := twoSessions(t, s)
	call(t, s, "messages", mailTo(a, "m", name))
	want := newestEvent(t, s, "inbox_changed")
	snap := b
	snap.Kind = "before"
	snap.Status = map[string]string{}
	if got := call(t, s, "reconcile", snap)["attention"].(map[string]any); identityCursor(t, s, got) != want {
		t.Fatalf("reconcile: %v", got)
	}
	asked := b
	asked.Attention = true
	if got := call(t, s, "events", asked)["attention"].(map[string]any); identityCursor(t, s, got) != want || got["unread"] != 1 {
		t.Fatalf("events: %v", got)
	}
	if got, present := call(t, s, "events", b)["attention"]; present {
		t.Fatalf("an events read that did not ask got %v", got)
	}
}

// A bind answer says whether any hook has reached the hub for the Execution, by handle and by
// session alike.
func TestBindSaysWhetherAHookReachedTheExecution(t *testing.T) {
	s, _, _ := testStore(t)
	at := Request{Client: "codex", Host: "host", SessionID: "s", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "mcp"}
	ex := call(t, s, "sessions", at)["execution_id"].(string)
	binds := []Request{{Client: "codex", Host: "host", Execution: ex}, {Client: "codex", Host: "host", SessionID: "s"}}
	for _, r := range binds {
		if got := call(t, s, "bind", r); got["hook_seen"] != false || got["execution_id"] != ex {
			t.Fatalf("before a hook: %v", got)
		}
	}
	at.Source = "hook"
	call(t, s, "sessions", at)
	for _, r := range binds {
		if got := call(t, s, "bind", r); got["hook_seen"] != true {
			t.Fatalf("after a hook: %v", got)
		}
	}
}
