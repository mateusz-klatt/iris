package store

import (
	"context"
	"testing"
	"time"
)

func consoleRevision(t *testing.T, s *Store, account ConsoleAccount) int64 {
	t.Helper()
	var revision int64
	if err := s.transact(context.Background(), func(tx *transaction) error {
		tx.console = &account
		result, err := tx.consoleEvents()
		if err == nil {
			if result["kind"] != "changed" || len(result) != 2 {
				t.Fatal("a console revision exposed activity metadata")
			}
			revision = result["cursor"].(int64)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestConsoleRevisionRequestsRevalidateSessionsAndCurrentRepositoryScope(t *testing.T) {
	s, clock, _ := testStore(t)
	_, ar := ended(t, s, "authorized-revision-a")
	_, br := ended(t, s, "authorized-revision-b")
	repoA, repoB := ar["repository_id"].(string), br["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "Changing reader", "member")
	a, err := s.ReplaceConsoleAccess(t.Context(), a.ID, 1, "member", false, []ConsoleGrant{{repoA, "ro"}})
	if err != nil {
		t.Fatal(err)
	}
	p := readConsolePrincipal(t, s, a)
	before := accountRead(t, s, p, "ui_events", Request{})["cursor"]
	if _, err = s.db.Exec("INSERT INTO RepositoryAlias VALUES('hidden-before-revocation',?)", repoB); err != nil {
		t.Fatal(err)
	}
	if frame := accountRead(t, s, p, "ui_events", Request{}); frame["cursor"] != before || len(frame) != 2 {
		t.Fatal("authenticated refresh revealed hidden activity")
	}
	changes := s.Changes()
	a, err = s.ReplaceConsoleAccess(t.Context(), a.ID, a.EditRevision, "member", false, []ConsoleGrant{{repoB, "ro"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("access change did not wake sessions for revalidation")
	}
	_, err = s.CallConsole(t.Context(), p, "ui_events", Request{})
	consoleError(t, err, "unauthorized")
	p = readConsolePrincipal(t, s, a)
	before = accountRead(t, s, p, "ui_events", Request{})["cursor"]
	if _, err = s.db.Exec("INSERT INTO RepositoryAlias VALUES('hidden-after-revocation',?)", repoA); err != nil {
		t.Fatal(err)
	}
	if frame := accountRead(t, s, p, "ui_events", Request{}); frame["cursor"] != before {
		t.Fatal("a removed grant continued to affect refreshed cursors")
	}
	_, err = s.CallConsole(t.Context(), p, "ui_events", Request{RepositoryID: repoA})
	consoleError(t, err, "not_found")
	if _, err = s.db.Exec("INSERT INTO RepositoryAlias VALUES('new-current-grant',?)", repoB); err != nil {
		t.Fatal(err)
	}
	if frame := accountRead(t, s, p, "ui_events", Request{}); frame["cursor"].(int64) <= before.(int64) {
		t.Fatal("the new grant failed to supply authorized updates")
	}
	clock.advance(time.Hour)
	_, err = s.CallConsole(t.Context(), p, "ui_events", Request{})
	consoleError(t, err, "unauthorized")
}

func TestConsoleRevisionsExcludeHiddenActivityAndGlobalHeartbeats(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, b, br := mergeSplit(t, s)
	repoA, repoB := ar["repository_id"].(string), br["repository_id"].(string)
	member := createTestConsoleAccount(t, s, "Reader", "member")
	none := createTestConsoleAccount(t, s, "NoGrants", "member")
	operator := createTestConsoleAccount(t, s, "Operator", "operator")
	if _, err := s.ReplaceConsoleAccess(t.Context(), member.ID, 1, "member", false, []ConsoleGrant{{repoA, "ro"}}); err != nil {
		t.Fatal(err)
	}
	memberBefore, noneBefore, operatorBefore := consoleRevision(t, s, member), consoleRevision(t, s, none), consoleRevision(t, s, operator)
	assertHidden := func() {
		t.Helper()
		if consoleRevision(t, s, member) != memberBefore || consoleRevision(t, s, none) != noneBefore {
			t.Fatal("a hidden repository or deployment event moved a member cursor")
		}
	}
	call(t, s, "ui_doctor_put", Request{Host: "hidden-host", CheckedAt: "now", Clients: []map[string]any{{"status": "ok"}}})
	heartbeat := b
	heartbeat.Source = "watch"
	call(t, s, "heartbeat", heartbeat)
	call(t, s, "claim", claimRequest(b, "hidden-claim", "hidden.go", "exact"))
	historySend(t, s, b, br["agent"].(map[string]any)["id"].(string), "hidden-mail", "", "hidden title", "hidden body")
	if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES('hidden-alias',?)", repoB); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	if consoleRevision(t, s, operator) <= operatorBefore {
		t.Fatal("operator cursor did not include deployment-wide and repository changes")
	}
	// The global event sequence changes without any scoped console data changing.
	if err := s.transact(t.Context(), func(tx *transaction) error {
		return tx.event("", "", "changed", "")
	}); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	// A delivery makes this envelope visible in A, despite its hidden sender repo.
	historySend(t, s, b, ar["agent"].(map[string]any)["id"].(string), "visible-envelope", "", "received title", "received body")
	if consoleRevision(t, s, member) <= memberBefore || consoleRevision(t, s, none) != noneBefore {
		t.Fatal("newly visible mail did not move exactly the authorized cursor")
	}
	memberBefore = consoleRevision(t, s, member)
	call(t, s, "claim", claimRequest(a, "visible-claim", "visible.go", "exact"))
	if consoleRevision(t, s, member) <= memberBefore {
		t.Fatal("an authorized claim change did not move the cursor")
	}
	memberBefore = consoleRevision(t, s, member)
	// Watching and lease renewal without a visible timestamp change are internal.
	if _, err := s.db.Exec("UPDATE Execution SET watch_seen_ms=123,deadline_tick_ms=deadline_tick_ms+1 WHERE id=?", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	assertHidden()
}

func TestConsoleRevisionsAreDurablePerAccountAndTransactional(t *testing.T) {
	s, clock, file := testStore(t)
	_, ar := ended(t, s, "revision-repository")
	repo := ar["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "First", "member")
	b := createTestConsoleAccount(t, s, "Second", "member")
	operator := createTestConsoleAccount(t, s, "Access operator", "operator")
	if _, err := s.ReplaceConsoleAccess(t.Context(), a.ID, 1, "member", false, []ConsoleGrant{{repo, "rw"}}); err != nil {
		t.Fatal(err)
	}
	first, second := consoleRevision(t, s, a), consoleRevision(t, s, b)
	operatorBefore := consoleRevision(t, s, operator)
	if _, err := s.UpdateConsoleProfile(t.Context(), b.ID, 1, "Changed profile", "en", "pl"); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, a) != first || consoleRevision(t, s, b) <= second {
		t.Fatal("profile activity affected another account's revision")
	}
	if consoleRevision(t, s, operator) <= operatorBefore {
		t.Fatal("an operator could not refresh changed account administration data")
	}
	err := s.transact(t.Context(), func(tx *transaction) error {
		if _, err := tx.exec("INSERT INTO RepositoryAlias VALUES('rolled-back-alias',?)", repo); err != nil {
			return err
		}
		return fail("conflict", "rollback the visible change")
	})
	consoleError(t, err, "conflict")
	if consoleRevision(t, s, a) != first {
		t.Fatal("an uncommitted repository change advanced a revision")
	}
	if _, err = s.db.Exec("INSERT INTO RepositoryAlias VALUES('durable-alias',?)", repo); err != nil {
		t.Fatal(err)
	}
	first, second = consoleRevision(t, s, a), consoleRevision(t, s, b)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if consoleRevision(t, reopened, a) != first || consoleRevision(t, reopened, b) != second {
		t.Fatal("restart lost or globally advanced account revisions")
	}
}

func TestConsoleRevisionMigrationBackfillsExistingAccountsWithoutAttributingPastActivity(t *testing.T) {
	s, clock, file := testStore(t)
	_, registered := ended(t, s, "revision-migration")
	repo := registered["repository_id"].(string)
	a := createTestConsoleAccount(t, s, "Existing member", "member")
	if _, err := s.ReplaceConsoleAccess(t.Context(), a.ID, 1, "member", false, []ConsoleGrant{{repo, "ro"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(removeAttentionIdentitySchema + removeConsoleRevisionSchema + "PRAGMA user_version=17"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if revision := consoleRevision(t, reopened, a); revision != 0 {
		t.Fatal("migration adopted a global historical sequence")
	}
	if _, err = reopened.db.Exec("INSERT INTO RepositoryAlias VALUES('after-revision-upgrade',?)", repo); err != nil {
		t.Fatal(err)
	}
	if revision := consoleRevision(t, reopened, a); revision != 1 {
		t.Fatal("migration failed to initialize the existing account's scoped cursor")
	}
}
