package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestTransactionCountIncludesCommitAndRollbackWithoutTakingAStoreSnapshot(t *testing.T) {
	s, _, _ := testStore(t)
	before := s.TransactionCount()
	call(t, s, "status", Request{})
	expectError(t, s, "unknown-operation", Request{}, "not_found")
	if got := s.TransactionCount(); got != before+2 {
		t.Fatalf("transaction count %d, want %d after a commit and rollback", got, before+2)
	}
	if again := s.TransactionCount(); again != before+2 {
		t.Fatal("reading the diagnostic counter itself opened a transaction")
	}
	if err := s.importRead(context.Background(), func(*transaction) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := s.TransactionCount(); got != before+3 {
		t.Fatal("read-only import snapshots were missing from the transaction diagnostic")
	}
}

func TestTokenUseBookkeepingStaysQuietButRevocationNotifies(t *testing.T) {
	s, _, _ := testStore(t)
	token, err := s.AddToken(context.Background(), "quiet-use")
	if err != nil {
		t.Fatal(err)
	}
	changes := s.Changes()
	if name, err := s.AuthenticateToken(context.Background(), token); err != nil || name != "quiet-use" {
		t.Fatalf("token authentication: %q %v", name, err)
	}
	var used int
	if err := s.db.QueryRow("SELECT requests FROM HostToken WHERE name='quiet-use'").Scan(&used); err != nil || used != 1 {
		t.Fatalf("quiet use did not retain request accounting: %d %v", used, err)
	}
	select {
	case <-changes:
		t.Fatal("bearer-use bookkeeping woke every stream")
	default:
	}
	digest, err := s.RevokeToken(context.Background(), "quiet-use")
	if err != nil || s.TokenActive(context.Background(), digest) {
		t.Fatal("token revocation failed", err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("credential revocation did not notify streams")
	}
}

func TestHeartbeatNotifiesScopedRevisionObserversWithoutAGlobalEvent(t *testing.T) {
	s, clock, _ := testStore(t)
	r, _ := session(t, s, 1)
	a := newSessionAccount(t, s, "heartbeat-observer", "operator")
	var before, high int64
	if err := s.db.QueryRow("SELECT revision FROM ConsoleRevision WHERE account_id=?", a.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='Event'").Scan(&high); err != nil {
		t.Fatal(err)
	}
	changes := s.Changes()
	clock.advance(time.Second)
	r.Source = "shim"
	call(t, s, "heartbeat", r)
	var after, sameHigh int64
	if err := s.db.QueryRow("SELECT revision FROM ConsoleRevision WHERE account_id=?", a.ID).Scan(&after); err != nil || after <= before {
		t.Fatalf("heartbeat did not advance its visible scoped revision: %d then %d: %v", before, after, err)
	}
	if err := s.db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='Event'").Scan(&sameHigh); err != nil || sameHigh != high {
		t.Fatalf("heartbeat synthesized a global event: %d then %d: %v", high, sameHigh, err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("heartbeat revision changed without waking its observers")
	}
}

func TestAnUnchangedConsumerCursorDoesNotWriteOrWake(t *testing.T) {
	s, _, _ := testStore(t)
	r, _ := session(t, s, 1)
	r.ConsumerID = "idle-cursor"
	page := call(t, s, "events", r)
	var before, after int64
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	changes := s.Changes()
	call(t, s, "events", r)
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unchanged cursor wrote %d rows", after-before)
	}
	select {
	case <-changes:
		t.Fatal("an unchanged cursor woke streams")
	default:
	}
	r.After = page["high_water"].(int64)
	call(t, s, "events", r)
	var saved int64
	if err := s.db.QueryRow("SELECT seq FROM Cursor WHERE consumer_id=?", r.ExecutionID+":"+r.ConsumerID).Scan(&saved); err != nil || saved != r.After {
		t.Fatalf("advanced cursor %d, want %d: %v", saved, r.After, err)
	}
	r.After = 0
	call(t, s, "events", r)
	var retained int64
	if err := s.db.QueryRow("SELECT seq FROM Cursor WHERE consumer_id=?", r.ExecutionID+":"+r.ConsumerID).Scan(&retained); err != nil || retained != saved {
		t.Fatalf("a replay regressed the cursor to %d from %d: %v", retained, saved, err)
	}
}

func TestEventsValidateTheRequestedGenerationInTheirOwnTransaction(t *testing.T) {
	s, _, _ := testStore(t)
	r, _ := session(t, s, 1)
	r.Generation++
	expectError(t, s, "events", r, "lost")
	// The unqualified HTTP form remains compatible with the current generation.
	r.Generation = 0
	call(t, s, "events", r)
}

func TestAnIdleMaintenanceSweepDoesNotNotifyStreams(t *testing.T) {
	s, _, _ := testStore(t)
	session(t, s, 1)
	changes := s.Changes()
	before := s.TransactionCount()
	call(t, s, "expire", Request{})
	if s.TransactionCount() != before+1 {
		t.Fatal("maintenance did not use exactly one transaction")
	}
	select {
	case <-changes:
		t.Fatal("idle maintenance woke streams")
	default:
	}
}

func TestMaintenanceExpiresConsoleSessionsAndNotifiesOnce(t *testing.T) {
	s, clock, _ := testStore(t)
	a := newSessionAccount(t, s, "maintenance-session", "member")
	p := issueTestSession(t, s, a, 71)
	clock.advance(ConsoleSessionLifetime)
	changes := s.Changes()
	call(t, s, "expire", Request{})
	if n := sessionCount(t, s, a); n != 0 {
		t.Fatalf("expired console session remains: %d", n)
	}
	if _, err := s.ConsoleSessionAccount(context.Background(), p.SessionDigest); err == nil {
		t.Fatal("expired console session still authenticates")
	}
	select {
	case <-changes:
	default:
		t.Fatal("session expiry did not wake its revocation observer")
	}
	changes = s.Changes()
	call(t, s, "expire", Request{})
	select {
	case <-changes:
		t.Fatal("expired console session notified twice")
	default:
	}
}

func TestMaintenanceRequeuesLostMailOnceAndLeavesAcknowledgedHistory(t *testing.T) {
	for _, ending := range []string{"deadline", "end"} {
		t.Run(ending, func(t *testing.T) {
			s, clock, _ := testStore(t)
			a, _ := session(t, s, 1)
			b, br := session(t, s, 2)
			name := br["agent"].(map[string]any)["name"].(string)
			for _, key := range []string{"waiting", "settled"} {
				call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: key, To: []string{name}, Body: key})
			}
			mail := call(t, s, "inbox", b)["deliveries"].([]map[string]any)
			if len(mail) != 2 {
				t.Fatal(mail)
			}
			acked := b
			acked.Key, acked.Attempt = mail[1]["id"].(string), number(mail[1]["attempt"])
			call(t, s, "ack", acked)
			waiting := mail[0]["id"].(string)
			if ending == "deadline" {
				clock.advance(time.Hour)
				call(t, s, "expire", Request{})
			} else {
				call(t, s, "end", b)
			}
			var assigned sql.NullString
			var attention int
			if err := s.db.QueryRow("SELECT execution_id,attention FROM Delivery WHERE id=?", waiting).Scan(&assigned, &attention); err != nil || assigned.Valid || attention != 1 {
				t.Fatalf("lost mail not returned to attention: %+v %d %v", assigned, attention, err)
			}
			if err := s.db.QueryRow("SELECT execution_id FROM Delivery WHERE id=?", acked.Key).Scan(&assigned); err != nil || !assigned.Valid || assigned.String != b.ExecutionID {
				t.Fatalf("settled delivery history changed: %+v %v", assigned, err)
			}
			var before, after int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='inbox_changed' AND object_id=?", waiting).Scan(&before); err != nil {
				t.Fatal(err)
			}
			call(t, s, "expire", Request{})
			if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='inbox_changed' AND object_id=?", waiting).Scan(&after); err != nil || after != before {
				t.Fatalf("mail requeued twice: %d then %d: %v", before, after, err)
			}
		})
	}
}

func TestHandoverDropsReceiptsForMovedAndSettledMail(t *testing.T) {
	s, clock, _ := testStore(t)
	sender, _ := session(t, s, 9)
	same := Request{Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", ClientPID: 4242, ClientPIDStartMS: 77}
	strayRequest, hookRequest := same, same
	strayRequest.SessionID, strayRequest.Source = "receipt-stray", "mcp"
	hookRequest.SessionID, hookRequest.Source = "receipt-hook", "hook"
	stray := call(t, s, "sessions", strayRequest)
	hook := call(t, s, "sessions", hookRequest)
	strayName := stray["agent"].(map[string]any)["name"].(string)
	hookName := hook["agent"].(map[string]any)["name"].(string)
	for _, subject := range []string{"both", "moved"} {
		mail := sender
		mail.Key, mail.Subject, mail.Body, mail.To = subject, subject, "receipt handover", []string{strayName}
		if subject == "both" {
			mail.To = append(mail.To, hookName)
		}
		call(t, s, "messages", mail)
	}
	from := Request{ExecutionID: stray["execution_id"].(string), Generation: stray["generation"].(int64), Receipt: true}
	fetched := call(t, s, "inbox", from)["deliveries"].([]map[string]any)
	if len(fetched) != 2 {
		t.Fatal(fetched)
	}
	from.HandOver = hook["execution_id"].(string)
	call(t, s, "end", from)
	for _, delivery := range fetched {
		var due sql.NullInt64
		var assigned sql.NullString
		var attention int
		var acked bool
		if err := s.db.QueryRow("SELECT receipt_due_ms,execution_id,attention,EXISTS(SELECT 1 FROM Ack WHERE delivery_id=Delivery.id) FROM Delivery WHERE id=?", delivery["id"]).Scan(&due, &assigned, &attention, &acked); err != nil {
			t.Fatal(err)
		}
		if due.Valid {
			t.Fatalf("handover kept the old fetch receipt for %s", delivery["subject"])
		}
		if delivery["subject"] == "both" && (!acked || !assigned.Valid || assigned.String != from.ExecutionID || attention != 0) {
			t.Fatal("settled duplicate lost its acknowledgement history")
		}
		if delivery["subject"] == "moved" && (acked || assigned.Valid || attention != 1) {
			t.Fatal("moved delivery lost pending attention")
		}
	}
	var before, after int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='inbox_changed'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	clock.advance(receiptWindow + time.Second)
	call(t, s, "expire", Request{})
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='inbox_changed'").Scan(&after); err != nil || after != before {
		t.Fatalf("the old fetch deadline woke handed-over mail: %d then %d: %v", before, after, err)
	}
}

func TestIdleIndexUpgradeClearsSettledLegacyReceiptDeadlines(t *testing.T) {
	s, clock, file := testStore(t)
	_, recipient, delivery, _ := attentionSetup(t, s)
	acked := recipient
	acked.Key, acked.Attempt = delivery, 1
	call(t, s, "ack", acked)
	// Reconstruct the stale metadata a pre-upgrade handover could leave.
	if _, err := s.db.Exec("UPDATE Delivery SET receipt_due_ms=? WHERE id=?", clock.Now().Add(receiptWindow).UnixMilli(), delivery); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"execution_active_deadline", "claim_unrevoked_execution", "delivery_attention_recipient", "event_recipient_cursor", "console_session_expiry"} {
		if _, err := s.db.Exec("DROP INDEX " + index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("PRAGMA user_version=20"); err != nil {
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
	t.Cleanup(func() { _ = upgraded.Close() })
	var due sql.NullInt64
	var settled bool
	if err := upgraded.db.QueryRow("SELECT receipt_due_ms,EXISTS(SELECT 1 FROM Ack WHERE delivery_id=Delivery.id) FROM Delivery WHERE id=?", delivery).Scan(&due, &settled); err != nil || due.Valid || !settled {
		t.Fatalf("upgrade retained a settled fetch deadline or lost its ack: %v %t %v", due, settled, err)
	}
}
