package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const schema26UnfilteredClaimAndDeliveryTriggers = removeLeaseRenewedTickSchema + `
DROP TRIGGER console_revision_claim_update;
CREATE TRIGGER console_revision_claim_update AFTER UPDATE ON Claim BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;
DROP TRIGGER console_revision_delivery_update;
CREATE TRIGGER console_revision_delivery_update AFTER UPDATE ON Delivery BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT repository_id FROM Agent WHERE id=OLD.recipient_agent_id UNION SELECT repository_id FROM Agent WHERE id=NEW.recipient_agent_id;
END;
PRAGMA user_version=26;
`

func TestClaimBookkeepingMovesNoConsoleCursor(t *testing.T) {
	s, clock, _ := testStore(t)
	a, ao := session(t, s, 1)
	b, _ := session(t, s, 2)
	repo1 := ao["repository_id"].(string)
	_, co := sessionInRepository(t, s, "other-repo", "https://example.com/team/other.git", "other-root")
	repo2 := co["repository_id"].(string)

	operator := createTestConsoleAccount(t, s, "Operator", "operator")
	member1 := createTestConsoleAccount(t, s, "Repo1Reader", "member")
	member2 := createTestConsoleAccount(t, s, "Repo2Reader", "member")
	unrelated := createTestConsoleAccount(t, s, "Unrelated", "member")
	var err error
	if member1, err = s.ReplaceConsoleAccess(t.Context(), member1.ID, 1, "member", false, []ConsoleGrant{{repo1, "ro"}}); err != nil {
		t.Fatal(err)
	}
	if member2, err = s.ReplaceConsoleAccess(t.Context(), member2.ID, 1, "member", false, []ConsoleGrant{{repo2, "ro"}}); err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(what string, opBefore, m1Before, m2Before, uBefore int64) {
		t.Helper()
		if consoleRevision(t, s, operator) != opBefore {
			t.Fatalf("%s moved operator cursor", what)
		}
		if consoleRevision(t, s, member1) != m1Before {
			t.Fatalf("%s moved repo1 member cursor", what)
		}
		if consoleRevision(t, s, member2) != m2Before {
			t.Fatalf("%s moved repo2 member cursor", what)
		}
		if consoleRevision(t, s, unrelated) != uBefore {
			t.Fatalf("%s moved unrelated member cursor", what)
		}
	}
	assertRepo1Moved := func(what string, opBefore, m1Before, m2Before, uBefore int64) (int64, int64, int64, int64) {
		t.Helper()
		opAfter := consoleRevision(t, s, operator)
		m1After := consoleRevision(t, s, member1)
		m2After := consoleRevision(t, s, member2)
		uAfter := consoleRevision(t, s, unrelated)
		if opAfter <= opBefore || m1After <= m1Before {
			t.Fatalf("%s did not move repo1/operator cursors: op %d->%d, m1 %d->%d", what, opBefore, opAfter, m1Before, m1After)
		}
		if m2After != m2Before || uAfter != uBefore {
			t.Fatalf("%s leaked to unrelated repos: m2 %d->%d, u %d->%d", what, m2Before, m2After, uBefore, uAfter)
		}
		return opAfter, m1After, m2After, uAfter
	}

	opRev, m1Rev, m2Rev, uRev := consoleRevision(t, s, operator), consoleRevision(t, s, member1), consoleRevision(t, s, member2), consoleRevision(t, s, unrelated)

	// New claim insert bumps.
	out := call(t, s, "touch", touchRequest(a, "t1", "held.go"))
	if out["action"] != "allow" {
		t.Fatal(out)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("initial auto claim insert", opRev, m1Rev, m2Rev, uRev)

	// Baseline reconcile before renewal does not alter claims.
	snap := a
	snap.Kind = "before"
	snap.Status = map[string]string{"held.go": " M"}
	call(t, s, "reconcile", snap)
	assertUnchanged("reconcile before snapshot", opRev, m1Rev, m2Rev, uRev)

	// Renewals of expires_ms via touch, touch_files, and reconcile must not bump.
	clock.advance(time.Second)
	call(t, s, "touch", touchRequest(a, "t2", "held.go"))
	assertUnchanged("touch expires_ms renewal", opRev, m1Rev, m2Rev, uRev)

	clock.advance(time.Second)
	call(t, s, "touch_files", Request{
		ExecutionID: a.ExecutionID,
		Generation:  a.Generation,
		ToolCallID:  "tf1",
		Groups: []ClaimGroup{{
			Worktree:   "/repo/w1",
			RemoteURL:  "https://example.com/team/project.git",
			RootCommit: "root",
			Paths:      []string{"held.go"},
		}},
	})
	assertUnchanged("touch_files expires_ms renewal", opRev, m1Rev, m2Rev, uRev)

	clock.advance(time.Second)
	snap.Kind = "after"
	call(t, s, "reconcile", snap)
	assertUnchanged("reconcile expires_ms renewal", opRev, m1Rev, m2Rev, uRev)

	// Bookkeeping path_key update and same-value updates on visible columns must not bump.
	var fence int64
	if err := s.db.QueryRow("SELECT fence FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", a.ExecutionID).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE Claim SET path_key=path_key||':folded' WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	assertUnchanged("path_key bookkeeping update", opRev, m1Rev, m2Rev, uRev)
	if _, err := s.db.Exec("UPDATE Claim SET execution_id=execution_id,repository_id=repository_id,kind=kind,path=path,origin=origin,revoked_ms=revoked_ms WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	assertUnchanged("no-op update of visible Claim columns", opRev, m1Rev, m2Rev, uRev)

	// Upgrading auto -> explicit changes origin and bumps.
	call(t, s, "claim", claimRequest(a, "upgrade-explicit", "held.go", "exact"))
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("auto to explicit upgrade", opRev, m1Rev, m2Rev, uRev)

	// Re-claiming an already-explicit claim with a TTL updates only expires_ms and must not bump.
	extend := claimRequest(a, "extend-explicit", "held.go", "exact")
	extend.TTLS = 600
	call(t, s, "claim", extend)
	assertUnchanged("explicit claim TTL update", opRev, m1Rev, m2Rev, uRev)

	// User-visible Claim column updates (path, kind, execution_id, repository_id) must bump.
	if _, err := s.db.Exec("UPDATE Claim SET path='renamed.go' WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("Claim.path update", opRev, m1Rev, m2Rev, uRev)

	if _, err := s.db.Exec("UPDATE Claim SET kind='prefix' WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("Claim.kind update", opRev, m1Rev, m2Rev, uRev)

	if _, err := s.db.Exec("UPDATE Claim SET execution_id=? WHERE fence=?", b.ExecutionID, fence); err != nil {
		t.Fatal(err)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("Claim.execution_id update", opRev, m1Rev, m2Rev, uRev)

	assertBothReposMoved := func(what string) {
		t.Helper()
		opAfter := consoleRevision(t, s, operator)
		m1After := consoleRevision(t, s, member1)
		m2After := consoleRevision(t, s, member2)
		uAfter := consoleRevision(t, s, unrelated)
		if opAfter <= opRev || m1After <= m1Rev || m2After <= m2Rev || uAfter != uRev {
			t.Fatalf("%s did not notify both repos: op %d->%d m1 %d->%d m2 %d->%d u %d->%d", what, opRev, opAfter, m1Rev, m1After, m2Rev, m2After, uRev, uAfter)
		}
		opRev, m1Rev, m2Rev, uRev = opAfter, m1After, m2After, uAfter
	}

	if _, err := s.db.Exec("UPDATE Claim SET repository_id=? WHERE fence=?", repo2, fence); err != nil {
		t.Fatal(err)
	}
	assertBothReposMoved("Claim.repository_id move to repo2")

	if _, err := s.db.Exec("UPDATE Claim SET repository_id=? WHERE fence=?", repo1, fence); err != nil {
		t.Fatal(err)
	}
	assertBothReposMoved("Claim.repository_id move back to repo1")

	// Revoke via ui_revoke updates revoked_ms and bumps.
	call(t, s, "ui_revoke", Request{Fence: fence})
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("ui_revoke", opRev, m1Rev, m2Rev, uRev)

	// Delete Claim bumps.
	if _, err := s.db.Exec("DELETE FROM Claim WHERE fence=?", fence); err != nil {
		t.Fatal(err)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("Claim delete", opRev, m1Rev, m2Rev, uRev)

	// Actual auto-claim expiry writes revoked_ms and bumps.
	call(t, s, "touch", touchRequest(a, "t-expire", "expiring.go"))
	opRev, m1Rev, m2Rev, uRev = assertRepo1Moved("expiring auto claim insert", opRev, m1Rev, m2Rev, uRev)
	clock.advance(11 * time.Minute)
	call(t, s, "expire", Request{})
	assertRepo1Moved("expired auto claim revoke", opRev, m1Rev, m2Rev, uRev)
}

func TestDeliveryBookkeepingMovesNoConsoleCursor(t *testing.T) {
	s, clock, _ := testStore(t)
	a, ao := session(t, s, 1)
	b, bo := session(t, s, 2)
	repo1 := ao["repository_id"].(string)
	bName := bo["agent"].(map[string]any)["name"].(string)
	bAgentID := bo["agent"].(map[string]any)["id"].(string)

	c, co := sessionInRepository(t, s, "other-repo", "https://example.com/team/other.git", "other-root")
	repo2 := co["repository_id"].(string)
	cAgentID := co["agent"].(map[string]any)["id"].(string)
	_ = c

	operator := createTestConsoleAccount(t, s, "Operator", "operator")
	member1 := createTestConsoleAccount(t, s, "Repo1Reader", "member")
	member2 := createTestConsoleAccount(t, s, "Repo2Reader", "member")
	unrelated := createTestConsoleAccount(t, s, "Unrelated", "member")
	var err error
	if member1, err = s.ReplaceConsoleAccess(t.Context(), member1.ID, 1, "member", false, []ConsoleGrant{{repo1, "ro"}}); err != nil {
		t.Fatal(err)
	}
	if member2, err = s.ReplaceConsoleAccess(t.Context(), member2.ID, 1, "member", false, []ConsoleGrant{{repo2, "ro"}}); err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(what string, opBefore, m1Before, m2Before, uBefore int64) {
		t.Helper()
		if consoleRevision(t, s, operator) != opBefore ||
			consoleRevision(t, s, member1) != m1Before ||
			consoleRevision(t, s, member2) != m2Before ||
			consoleRevision(t, s, unrelated) != uBefore {
			t.Fatalf("%s moved a console cursor: op %d->%d m1 %d->%d m2 %d->%d u %d->%d",
				what, opBefore, consoleRevision(t, s, operator), m1Before, consoleRevision(t, s, member1), m2Before, consoleRevision(t, s, member2), uBefore, consoleRevision(t, s, unrelated))
		}
	}
	assertRepo1Delta := func(what string, opBefore, m1Before, m2Before, uBefore, wantDelta int64) (int64, int64, int64, int64) {
		t.Helper()
		opAfter := consoleRevision(t, s, operator)
		m1After := consoleRevision(t, s, member1)
		m2After := consoleRevision(t, s, member2)
		uAfter := consoleRevision(t, s, unrelated)
		if opAfter-opBefore != wantDelta || m1After-m1Before != wantDelta || m2After != m2Before || uAfter != uBefore {
			t.Fatalf("%s delta mismatch (want %d on repo1/op): op %d->%d m1 %d->%d m2 %d->%d u %d->%d",
				what, wantDelta, opBefore, opAfter, m1Before, m1After, m2Before, m2After, uBefore, uAfter)
		}
		return opAfter, m1After, m2After, uAfter
	}

	opRev, m1Rev, m2Rev, uRev := consoleRevision(t, s, operator), consoleRevision(t, s, member1), consoleRevision(t, s, member2), consoleRevision(t, s, unrelated)

	// Send creates Message (+1) and Delivery (+1); delivery_attention_seq trigger on Event must not add a 3rd bump.
	sent := call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m1", To: []string{bName}, Body: "hello"})
	deliveryID := sent["deliveries"].([]map[string]any)[0]["id"].(string)
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("message send (Message+Delivery inserts, attention_seq ignored)", opRev, m1Rev, m2Rev, uRev, 2)

	// First inbox fetch changes execution_id NULL -> b.ExecutionID (pending -> inflight) and bumps once.
	fetched := call(t, s, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipt: true})["deliveries"].([]map[string]any)
	if len(fetched) != 1 {
		t.Fatal(fetched)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("first inbox fetch (pending -> inflight)", opRev, m1Rev, m2Rev, uRev, 1)

	// Fetch receipt clears attention and receipt_due_ms; must not bump.
	call(t, s, "inbox_receipt", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipts: []Receipt{{ID: deliveryID, Attempt: number(fetched[0]["attempt"])}}})
	assertUnchanged("inbox_receipt", opRev, m1Rev, m2Rev, uRev)

	// Re-fetch by same Execution raises attempt and sets receipt_due_ms; execution_id is unchanged so must not bump.
	refetched := call(t, s, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipt: true})["deliveries"].([]map[string]any)
	assertUnchanged("same-Execution inbox re-fetch", opRev, m1Rev, m2Rev, uRev)

	// Missed receipt sweep clears receipt_due_ms and fires inbox_changed -> delivery_attention_seq; must not bump.
	clock.advance(receiptWindow + time.Second)
	call(t, s, "expire", Request{})
	assertUnchanged("wakeMissedReceipts + attention_seq trigger", opRev, m1Rev, m2Rev, uRev)

	// Direct bookkeeping-only and same-value updates must not bump.
	if _, err := s.db.Exec("UPDATE Delivery SET attempt=attempt+1,attention=1,receipt_due_ms=123456,attention_seq=999 WHERE id=?", deliveryID); err != nil {
		t.Fatal(err)
	}
	assertUnchanged("attempt/attention/receipt_due_ms/attention_seq SQL update", opRev, m1Rev, m2Rev, uRev)
	if _, err := s.db.Exec("UPDATE Delivery SET recipient_agent_id=recipient_agent_id,execution_id=execution_id WHERE id=?", deliveryID); err != nil {
		t.Fatal(err)
	}
	assertUnchanged("no-op update of visible Delivery columns", opRev, m1Rev, m2Rev, uRev)

	// Restore attempt to match refetched attempt and ack: Ack insert bumps once; Delivery.receipt_due_ms=NULL must not add a second bump.
	if _, err := s.db.Exec("UPDATE Delivery SET attempt=? WHERE id=?", number(refetched[0]["attempt"]), deliveryID); err != nil {
		t.Fatal(err)
	}
	call(t, s, "ack", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Key: deliveryID, Attempt: number(refetched[0]["attempt"])})
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("ack (Ack insert only, receipt_due_ms clear ignored)", opRev, m1Rev, m2Rev, uRev, 1)

	// Marking handled updates Ack.handled_ms and bumps once.
	call(t, s, "ack", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Key: deliveryID, Attempt: number(refetched[0]["attempt"]), Handled: true})
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("ack handled", opRev, m1Rev, m2Rev, uRev, 1)

	// Send second message, fetch to make it inflight, then test execution_id -> NULL (inflight -> pending) and recipient_agent_id move.
	sent2 := call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m2", To: []string{bName}, Body: "second"})
	delivery2 := sent2["deliveries"].([]map[string]any)[0]["id"].(string)
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("second message send", opRev, m1Rev, m2Rev, uRev, 2)
	call(t, s, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation})
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("second inbox fetch (pending -> inflight)", opRev, m1Rev, m2Rev, uRev, 1)

	if _, err := s.db.Exec("UPDATE Delivery SET execution_id=NULL WHERE id=?", delivery2); err != nil {
		t.Fatal(err)
	}
	opRev, m1Rev, m2Rev, uRev = assertRepo1Delta("Delivery.execution_id cleared (inflight -> pending)", opRev, m1Rev, m2Rev, uRev, 1)

	assertBothReposDelta := func(what string) {
		t.Helper()
		opAfter := consoleRevision(t, s, operator)
		m1After := consoleRevision(t, s, member1)
		m2After := consoleRevision(t, s, member2)
		uAfter := consoleRevision(t, s, unrelated)
		if opAfter-opRev != 2 || m1After-m1Rev != 1 || m2After-m2Rev != 1 || uAfter != uRev {
			t.Fatalf("%s did not notify both repos: op %d->%d m1 %d->%d m2 %d->%d u %d->%d", what, opRev, opAfter, m1Rev, m1After, m2Rev, m2After, uRev, uAfter)
		}
		opRev, m1Rev, m2Rev, uRev = opAfter, m1After, m2After, uAfter
	}

	if _, err := s.db.Exec("UPDATE Delivery SET recipient_agent_id=? WHERE id=?", cAgentID, delivery2); err != nil {
		t.Fatal(err)
	}
	assertBothReposDelta("Delivery.recipient_agent_id move to repo2")

	// Verify handover/reassignment back to bAgentID also bumps both repos, then delete bumps.
	if _, err := s.db.Exec("UPDATE Delivery SET recipient_agent_id=? WHERE id=?", bAgentID, delivery2); err != nil {
		t.Fatal(err)
	}
	assertBothReposDelta("Delivery.recipient_agent_id move back to repo1")
	if _, err := s.db.Exec("DELETE FROM Delivery WHERE id=?", delivery2); err != nil {
		t.Fatal(err)
	}
	assertRepo1Delta("Delivery delete", opRev, m1Rev, m2Rev, uRev, 1)
}

func TestMigration27FiltersClaimAndDeliveryConsoleCursors(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := session(t, s, 1)
	b, bo := session(t, s, 2)
	bName := bo["agent"].(map[string]any)["name"].(string)
	operator := createTestConsoleAccount(t, s, "Operator", "operator")

	call(t, s, "touch", touchRequest(a, "t1", "held.go"))
	sent := call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m1", To: []string{bName}, Body: "hi"})
	deliveryID := sent["deliveries"].([]map[string]any)[0]["id"].(string)
	call(t, s, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipt: true})

	if _, err := s.db.Exec(schema26UnfilteredClaimAndDeliveryTriggers); err != nil {
		t.Fatal(err)
	}

	// Confirm the schema-26 triggers fire on bookkeeping-only updates.
	before := consoleRevision(t, s, operator)
	if _, err := s.db.Exec("UPDATE Claim SET expires_ms=expires_ms+1 WHERE execution_id=?", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) == before {
		t.Fatal("schema-26 claim trigger was not active")
	}
	before = consoleRevision(t, s, operator)
	if _, err := s.db.Exec("UPDATE Delivery SET receipt_due_ms=123 WHERE id=?", deliveryID); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) == before {
		t.Fatal("schema-26 delivery trigger was not active")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil

	// Reopen to apply migration 027.
	upgraded, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		_ = upgraded.Close()
		t.Fatalf("version %d, want %d: %v", version, latestVersion, err)
	}
	// Reopen an already-v27 store so version >= 27 path is also verified.
	if err = upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatalf("reopened version %d, want %d: %v", version, latestVersion, err)
	}

	before = consoleRevision(t, reopened, operator)
	if _, err = reopened.db.Exec("UPDATE Claim SET expires_ms=expires_ms+1 WHERE execution_id=?", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.db.Exec("UPDATE Delivery SET receipt_due_ms=456,attempt=attempt+1 WHERE id=?", deliveryID); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, reopened, operator) != before {
		t.Fatal("bookkeeping still moved console revision after migration 027")
	}

	if _, err = reopened.db.Exec("UPDATE Claim SET revoked_ms=? WHERE execution_id=?", clock.Now().UnixMilli(), a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	afterRevoke := consoleRevision(t, reopened, operator)
	if afterRevoke <= before {
		t.Fatal("claim revoke did not move console revision after migration 027")
	}
	if _, err = reopened.db.Exec("UPDATE Delivery SET execution_id=NULL WHERE id=?", deliveryID); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, reopened, operator) <= afterRevoke {
		t.Fatal("delivery execution_id change did not move console revision after migration 027")
	}
}

func TestMigration27RollsBackAtEveryStep(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := session(t, s, 1)
	b, bo := session(t, s, 2)
	bName := bo["agent"].(map[string]any)["name"].(string)
	call(t, s, "touch", touchRequest(a, "t1", "held.go"))
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m1", To: []string{bName}, Body: "hi"})
	call(t, s, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipt: true})
	if _, err := s.db.Exec(schema26UnfilteredClaimAndDeliveryTriggers); err != nil {
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
	if !strings.Contains(strings.Join(trace, "\n"), "console_revision_claim_update") {
		t.Fatalf("migration 027 statements missing from trace: %v", trace)
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
		var claimSQL, deliverySQL string
		if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 26 {
			_ = raw.Close()
			t.Fatalf("fault %d %s changed version to %d: %v", i+1, label, version, err)
		}
		if err = raw.QueryRow("SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='console_revision_claim_update'").Scan(&claimSQL); err != nil || strings.Contains(claimSQL, "WHEN") {
			_ = raw.Close()
			t.Fatalf("fault %d %s altered claim trigger: %q %v", i+1, label, claimSQL, err)
		}
		if err = raw.QueryRow("SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='console_revision_delivery_update'").Scan(&deliverySQL); err != nil || strings.Contains(deliverySQL, "WHEN") {
			_ = raw.Close()
			t.Fatalf("fault %d %s altered delivery trigger: %q %v", i+1, label, deliverySQL, err)
		}
		_ = raw.Close()
	}

	retried, err := openDatabase(file, clock, name)
	if err != nil {
		t.Fatal("migration 027 retry failed", err)
	}
	t.Cleanup(func() { _ = retried.Close() })
	var version int
	if err = retried.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatalf("retried version %d, want %d: %v", version, latestVersion, err)
	}
}

func TestConsoleRevisionBurstFrameRateBeforeAndAfterMigration27(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := session(t, s, 1)
	b, bo := session(t, s, 2)
	bName := bo["agent"].(map[string]any)["name"].(string)
	operator := createTestConsoleAccount(t, s, "Operator", "operator")

	const renewals = 50
	runBurst := func(store *Store, prefix string) (totalMoves, renewalMoves, deliveryBookkeepingMoves, realChangeMoves int64) {
		call(t, store, "touch", touchRequest(a, prefix+"-init", "burst.go"))
		var fence int64
		if err := store.db.QueryRow("SELECT fence FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", a.ExecutionID).Scan(&fence); err != nil {
			t.Fatal(err)
		}
		sent := call(t, store, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: prefix + "-msg", To: []string{bName}, Body: "burst"})
		deliveryID := sent["deliveries"].([]map[string]any)[0]["id"].(string)
		call(t, store, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipt: true})

		start := consoleRevision(t, store, operator)
		cur := start
		for i := range renewals {
			clock.advance(time.Second)
			call(t, store, "touch", touchRequest(a, fmt.Sprintf("%s-touch-%d", prefix, i), "burst.go"))
			next := consoleRevision(t, store, operator)
			renewalMoves += next - cur
			cur = next

			fetched := call(t, store, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipt: true})["deliveries"].([]map[string]any)
			next = consoleRevision(t, store, operator)
			deliveryBookkeepingMoves += next - cur
			cur = next

			call(t, store, "inbox_receipt", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Receipts: []Receipt{{ID: deliveryID, Attempt: number(fetched[0]["attempt"])}}})
			next = consoleRevision(t, store, operator)
			deliveryBookkeepingMoves += next - cur
			cur = next
		}
		finalFetch := call(t, store, "inbox", Request{ExecutionID: b.ExecutionID, Generation: b.Generation})["deliveries"].([]map[string]any)
		// Account for one final fetch before ack separately or fold into deliveryBookkeepingMoves:
		next := consoleRevision(t, store, operator)
		deliveryBookkeepingMoves += next - cur
		cur = next

		call(t, store, "ui_revoke", Request{Fence: fence})
		call(t, store, "ack", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Key: deliveryID, Attempt: number(finalFetch[0]["attempt"])})
		after := consoleRevision(t, store, operator)
		realChangeMoves = after - cur
		totalMoves = after - start
		return totalMoves, renewalMoves, deliveryBookkeepingMoves, realChangeMoves
	}

	if _, err := s.db.Exec(schema26UnfilteredClaimAndDeliveryTriggers); err != nil {
		t.Fatal(err)
	}
	beforeTotal, beforeClaim, beforeDelivery, beforeReal := runBurst(s, "before")
	// Before migration 027:
	//   50 claim renewals = 50 moves
	//   50*(inbox re-fetch + inbox_receipt) + 1 final inbox re-fetch = 101 moves
	//   1 ui_revoke (1) + 1 ack (Ack insert + Delivery.receipt_due_ms=NULL = 2) = 3 moves
	//   Total = 154 moves for 2 real state changes.
	if beforeClaim != renewals || beforeDelivery != renewals*2+1 || beforeReal != 3 || beforeTotal != 154 {
		t.Fatalf("unexpected schema-26 burst counts: total=%d claim=%d delivery=%d real=%d", beforeTotal, beforeClaim, beforeDelivery, beforeReal)
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

	// Re-establish live claim and delivery with fresh sessions (Open put old ones in recovery).
	a = Request{ExecutionID: a.ExecutionID, Generation: a.Generation}
	call(t, upgraded, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "1", Generation: a.Generation, Source: "hook"})
	call(t, upgraded, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "2", Generation: b.Generation, Source: "hook"})

	afterTotal, afterClaim, afterDelivery, afterReal := runBurst(upgraded, "after")
	// After migration 027:
	//   50 claim renewals = 0 moves
	//   50*(inbox re-fetch + inbox_receipt) + 1 final inbox re-fetch = 0 moves
	//   1 ui_revoke (1) + 1 ack (Ack insert only = 1) = 2 moves
	//   Total = 2 moves for 2 real state changes.
	if afterClaim != 0 || afterDelivery != 0 || afterReal != 2 || afterTotal != 2 {
		t.Fatalf("unexpected schema-27 burst counts: total=%d claim=%d delivery=%d real=%d", afterTotal, afterClaim, afterDelivery, afterReal)
	}
	t.Logf("burst(%d renewals + %d delivery bookkeeping steps + 2 real changes): before=%d (claim=%d, delivery=%d, real=%d) -> after=%d (claim=%d, delivery=%d, real=%d)",
		renewals, renewals*2+1, beforeTotal, beforeClaim, beforeDelivery, beforeReal, afterTotal, afterClaim, afterDelivery, afterReal)
}

func sessionInRepository(t *testing.T, s *Store, worktreeSuffix, remote, root string) (Request, map[string]any) {
	t.Helper()
	wt := "/repo/" + worktreeSuffix
	r := Request{Client: "codex", Host: "host", Cwd: wt, Worktree: wt, RemoteURL: remote, RootCommit: root, SessionID: "sess-" + worktreeSuffix, Source: "hook"}
	out := call(t, s, "sessions", r)
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out
}
