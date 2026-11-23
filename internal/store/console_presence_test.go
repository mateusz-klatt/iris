package store

import (
	"testing"
	"time"
)

// An agent's presence time alone moves no console cursor: every hook call and heartbeat writes
// last_seen_ms, and with busy agents a cursor that followed it refetched the console without end.
// A change the console shows, here the session ending, still moves it.
func TestPresenceAloneMovesNoConsoleCursor(t *testing.T) {
	s, clock, _ := testStore(t)
	r, out := session(t, s, 1)
	repo := out["repository_id"].(string)
	operator := createTestConsoleAccount(t, s, "Operator", "operator")
	member := createTestConsoleAccount(t, s, "Reader", "member")
	member, err := s.ReplaceConsoleAccess(t.Context(), member.ID, 1, "member", false, []ConsoleGrant{{repo, "ro"}})
	if err != nil {
		t.Fatal(err)
	}
	operatorBefore, memberBefore := consoleRevision(t, s, operator), consoleRevision(t, s, member)
	unmoved := func(what string) {
		t.Helper()
		if consoleRevision(t, s, operator) != operatorBefore || consoleRevision(t, s, member) != memberBefore {
			t.Fatalf("%s moved a console cursor", what)
		}
	}
	clock.advance(time.Second)
	heartbeat := r
	heartbeat.Source = "watch"
	call(t, s, "heartbeat", heartbeat)
	unmoved("a heartbeat")
	clock.advance(time.Second)
	session(t, s, 1)
	unmoved("a hook registering its session again")
	if _, err := s.db.Exec("UPDATE Execution SET state=state,branch=branch,repository_id=repository_id WHERE id=?", r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	unmoved("an update that leaves repository, branch and state as they were")
	call(t, s, "end", r)
	if consoleRevision(t, s, operator) <= operatorBefore || consoleRevision(t, s, member) <= memberBefore {
		t.Fatal("a session that ended did not move the console cursors")
	}
}

// A schema-25 hub moved console cursors on last_seen_ms. Opening it replaces that trigger.
func TestMigration26StopsPresenceFromMovingConsoleCursors(t *testing.T) {
	s, clock, file := testStore(t)
	r, _ := session(t, s, 1)
	operator := createTestConsoleAccount(t, s, "Operator", "operator")
	if _, err := s.db.Exec(removeLeaseRenewedTickSchema + `DROP TRIGGER console_revision_execution_update;
CREATE TRIGGER console_revision_execution_update AFTER UPDATE OF repository_id,branch,state,last_seen_ms ON Execution BEGIN
 INSERT INTO ConsoleRepositoryChange SELECT OLD.repository_id UNION SELECT NEW.repository_id;
END;
PRAGMA user_version=25;`); err != nil {
		t.Fatal(err)
	}
	before := consoleRevision(t, s, operator)
	if _, err := s.db.Exec("UPDATE Execution SET last_seen_ms=last_seen_ms+1 WHERE id=?", r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) == before {
		t.Fatal("the schema-25 trigger was not in place")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != latestVersion {
		t.Fatalf("version %d, %v", version, err)
	}
	before = consoleRevision(t, s, operator)
	if _, err = s.db.Exec("UPDATE Execution SET last_seen_ms=last_seen_ms+1 WHERE id=?", r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if consoleRevision(t, s, operator) != before {
		t.Fatal("presence still moves the console cursor after migration 26")
	}
}
