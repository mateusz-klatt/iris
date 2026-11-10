package store

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

// reduced is a shell snapshot in the reduced scope that names these submodules.
func reduced(r Request, kind string, status map[string]string, submodules ...string) Request {
	r.Kind, r.Status, r.Scope = kind, status, &ShellScope{Mode: "dirty", Submodules: submodules}
	return r
}

// full is a shell snapshot in full scope.
func full(r Request, kind string, status map[string]string) Request {
	r.Kind, r.Status, r.Scope = kind, status, nil
	return r
}

// liveAuto is an Execution's live automatic claim on p, and its expiry.
func liveAuto(t *testing.T, s *Store, execution, p string) (int64, bool) {
	t.Helper()
	var expires int64
	err := s.db.QueryRow("SELECT expires_ms FROM Claim WHERE execution_id=? AND path=? AND origin='auto' AND revoked_ms IS NULL", execution, p).Scan(&expires)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return expires, true
}

// A snapshot is compared only with a baseline of its own scope. A change of scope, either way or
// to another set of submodules, replaces the baseline and attributes nothing, where the same
// change within one scope warns.
func TestAChangeOfScopeOnlySetsABaseline(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	for i, step := range []struct {
		route string
		r     Request
		warns bool
	}{
		{"reconcile", full(shell, "before", map[string]string{}), false},
		{"reconcile_scoped", reduced(shell, "after", map[string]string{"src/a": "??"}, "lib"), false},
		{"reconcile_scoped", reduced(shell, "after", map[string]string{"src/a": "??", "src/b": "??"}, "lib", "other"), false},
		{"reconcile", full(shell, "after", map[string]string{"src/a": "??", "src/b": "??", "src/c": "??"}), false},
		{"reconcile", full(shell, "after", map[string]string{"src/a": "??", "src/b": "??", "src/c": "??", "src/d": "??"}), true},
		{"reconcile_scoped", reduced(shell, "after", map[string]string{"src/e": "??"}, "lib"), false},
		{"reconcile_scoped", reduced(shell, "after", map[string]string{"src/e": "??", "src/f": "??"}, "lib"), true},
	} {
		if out := call(t, s, step.route, step.r); (out["message"] != "") != step.warns {
			t.Fatalf("step %d: %q", i, out["message"])
		}
	}
}

// A worktree without submodules sends an empty set, which JSON can carry as null or as []. The
// two compare as one scope in either order, so the command's change is still attributed.
func TestAnEmptySetIsOneScopeHoweverItIsSent(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	empty := reduced(shell, "before", map[string]string{})
	empty.Scope.Submodules = []string{}
	none := reduced(shell, "after", map[string]string{"src/a": "??"})
	none.Scope.Submodules = nil
	call(t, s, "reconcile_scoped", empty)
	if out := call(t, s, "reconcile_scoped", none); out["message"] == "" {
		t.Fatal("[] then null was taken for a change of scope")
	}
	none.Kind, none.Status = "before", map[string]string{"src/a": "??"}
	empty.Kind, empty.Status = "after", map[string]string{"src/a": "??", "src/b": "??"}
	call(t, s, "reconcile_scoped", none)
	if out := call(t, s, "reconcile_scoped", empty); out["message"] == "" {
		t.Fatal("null then [] was taken for a change of scope")
	}
}

// Under --ignore-submodules=dirty a submodule leaves the status when its commit returns to the
// recorded one, while edits inside it remain. Its path is in the scope, so it is unknown, not
// clean: the claim its earlier change took stays, and the unknown does not renew it. In full
// scope the same disappearance means the change was committed or restored, and the claim goes.
func TestHiddenDirtWithAReturningCommitReleasesNothing(t *testing.T) {
	s, clock, _ := testStore(t)
	shell, _ := session(t, s, 1)
	call(t, s, "reconcile_scoped", reduced(shell, "before", map[string]string{}, "lib"))
	out := call(t, s, "reconcile_scoped", reduced(shell, "after", map[string]string{"lib": " M"}, "lib"))
	if !reflect.DeepEqual(out["claimed"], []string{"lib"}) {
		t.Fatalf("the changed submodule was not claimed: %v", out["claimed"])
	}
	expiry, held := liveAuto(t, s, shell.ExecutionID, "lib")
	if !held {
		t.Fatal("no automatic claim on lib")
	}
	clock.advance(time.Minute)
	call(t, s, "reconcile_scoped", reduced(shell, "before", map[string]string{"lib": " M"}, "lib"))
	call(t, s, "reconcile_scoped", reduced(shell, "after", map[string]string{}, "lib"))
	if again, still := liveAuto(t, s, shell.ExecutionID, "lib"); !still || again != expiry {
		t.Fatalf("lib's claim: held %t, expiry %d, was %d", still, again, expiry)
	}
	call(t, s, "reconcile", full(shell, "before", map[string]string{"lib": " M"}))
	call(t, s, "reconcile", full(shell, "after", map[string]string{}))
	if _, still := liveAuto(t, s, shell.ExecutionID, "lib"); still {
		t.Fatal("a full status that no longer reports lib kept its claim")
	}
	var reduceds int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ReducedShellSnapshot WHERE execution_id=?", shell.ExecutionID).Scan(&reduceds); err != nil || reduceds != 0 {
		t.Fatalf("a full snapshot left %d reduced baselines: %v", reduceds, err)
	}
}

// Within one reduced scope a reported path compares as in full scope: a new one is claimed, or
// warned about where another session holds it; a changed code renews the session's own claim; an
// unchanged one does nothing; a path that is no submodule and left the status is released.
func TestReportedPathsCompareAsInFullScope(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "theirs.txt", "exact"))
	first := map[string]string{"mine.txt": " M", "same.txt": " M", "gone.txt": " M"}
	call(t, s, "reconcile_scoped", reduced(shell, "before", map[string]string{}, "lib"))
	call(t, s, "reconcile_scoped", reduced(shell, "after", first, "lib"))
	mine, _ := liveAuto(t, s, shell.ExecutionID, "mine.txt")
	same, _ := liveAuto(t, s, shell.ExecutionID, "same.txt")
	clock.advance(time.Minute)
	call(t, s, "reconcile_scoped", reduced(shell, "before", first, "lib"))
	out := call(t, s, "reconcile_scoped", reduced(shell, "after", map[string]string{"mine.txt": "M ", "same.txt": " M", "new.txt": "??", "theirs.txt": " M"}, "lib"))
	if out["message"] == "" {
		t.Fatal("a new path that another session holds was not warned about")
	}
	if renewed, _ := liveAuto(t, s, shell.ExecutionID, "mine.txt"); renewed <= mine {
		t.Fatal("a changed code did not renew the session's claim")
	}
	if kept, _ := liveAuto(t, s, shell.ExecutionID, "same.txt"); kept != same {
		t.Fatal("an unchanged code renewed the claim")
	}
	for p, want := range map[string]bool{"new.txt": true, "theirs.txt": false, "gone.txt": false} {
		if _, held := liveAuto(t, s, shell.ExecutionID, p); held != want {
			t.Errorf("%s: held %t", p, held)
		}
	}
}

// A start clears every reduced baseline and keeps the full ones: whichever hub ran in between,
// the first reduced snapshot after a start only sets a baseline (ADR 018).
func TestReducedBaselinesDoNotCrossAStart(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := session(t, s, 1)
	b, _ := session(t, s, 2)
	call(t, s, "reconcile", full(a, "before", map[string]string{"x": " M"}))
	call(t, s, "reconcile_scoped", reduced(b, "before", map[string]string{"y": " M"}, "lib"))
	reopened := restart(t, s, file, clock)
	var fulls, reduceds int
	if err := reopened.db.QueryRow("SELECT (SELECT COUNT(*) FROM ShellSnapshot),(SELECT COUNT(*) FROM ReducedShellSnapshot)").Scan(&fulls, &reduceds); err != nil || fulls != 1 || reduceds != 0 {
		t.Fatalf("after a start: %d full, %d reduced, %v", fulls, reduceds, err)
	}
}

// restart closes s and opens its file again, as a hub start does.
func restart(t *testing.T, s *Store, file string, clock *fakeClock) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

// What an older hub's store runs on the migrated schema, statement for statement (ef67498): it
// reads and writes ShellSnapshot only. After a reduced write its next reconcile finds no
// baseline, so it never reads a reduced map as full, and its deletes of snapshots and Executions
// still succeed, the cascade taking the reduced rows. The next new start clears what it left in
// the reduced table, and its full snapshot is the baseline.
func TestAnOlderHubNeverReadsAReducedMap(t *testing.T) {
	s, clock, file := testStore(t)
	shell, _ := session(t, s, 1)
	other, _ := session(t, s, 2)
	call(t, s, "reconcile", full(shell, "before", map[string]string{"lib": " M"}))
	call(t, s, "reconcile_scoped", reduced(shell, "after", map[string]string{"lib": " M"}, "lib"))
	call(t, s, "reconcile_scoped", reduced(other, "before", map[string]string{"lib": " M"}, "lib"))
	call(t, s, "reconcile_scoped", reduced(other, "after", map[string]string{"lib": " M", "x": "??"}, "lib"))
	var raw string
	if err := s.db.QueryRow("SELECT status_json FROM ShellSnapshot WHERE execution_id=?", shell.ExecutionID).Scan(&raw); err != sql.ErrNoRows {
		t.Fatalf("an older hub would read a baseline: %q %v", raw, err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO ShellSnapshot VALUES(?,?) ON CONFLICT(execution_id) DO UPDATE SET status_json=excluded.status_json", []any{shell.ExecutionID, `{"top":" M"}`}},
		{"DELETE FROM ShellSnapshot WHERE execution_id IN (SELECT id FROM Execution WHERE repository_id=? AND host_id=? AND worktree=?)", []any{"none", "host", "/repo/w9"}},
		{"DELETE FROM Claim WHERE execution_id=?", []any{other.ExecutionID}},
		{"DELETE FROM Waiter WHERE execution_id=?", []any{other.ExecutionID}},
		{"DELETE FROM Cursor WHERE execution_id=?", []any{other.ExecutionID}},
		{"DELETE FROM ShellSnapshot WHERE execution_id=?", []any{other.ExecutionID}},
		{"DELETE FROM Event WHERE execution_id=?", []any{other.ExecutionID}},
		{"DELETE FROM Execution WHERE id=?", []any{other.ExecutionID}},
	} {
		if _, err := s.db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	var left int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ReducedShellSnapshot WHERE execution_id=?", other.ExecutionID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the cascade left %d reduced rows: %v", left, err)
	}
	reopened := restart(t, s, file, clock)
	var reduceds int
	if err := reopened.db.QueryRow("SELECT (SELECT status_json FROM ShellSnapshot WHERE execution_id=?),(SELECT COUNT(*) FROM ReducedShellSnapshot)", shell.ExecutionID).Scan(&raw, &reduceds); err != nil || raw != `{"top":" M"}` || reduceds != 0 {
		t.Fatalf("after the new start: %q, %d reduced, %v", raw, reduceds, err)
	}
}

// A second live Execution in the worktree makes attribution impossible, in reduced scope as in
// full: nothing is warned about, and both kinds of baseline are cleared.
func TestASharedWorktreeClearsReducedBaselinesToo(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	call(t, s, "reconcile_scoped", reduced(shell, "before", map[string]string{}, "lib"))
	call(t, s, "sessions", Request{Client: "claude", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "shared"})
	if out := call(t, s, "reconcile_scoped", reduced(shell, "after", map[string]string{"src/new": "??"}, "lib")); out["message"] != "" {
		t.Fatalf("shared worktree warned: %v", out["message"])
	}
	var left int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ReducedShellSnapshot").Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d reduced baselines left: %v", left, err)
	}
}

// The scoped route takes the session form too: one call registers and reconciles, records the
// hook as seen, and answers with the session and its capabilities, as the plain route does.
func TestTheScopedRouteRegistersAndReconcilesInOneCall(t *testing.T) {
	s, clock, _ := testStore(t)
	clock.advance(time.Minute)
	register := Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "1", Source: "hook"}
	out := call(t, s, "reconcile_scoped", reduced(register, "before", map[string]string{}, "lib"))
	answer := out["session"].(map[string]any)
	if !reflect.DeepEqual(answer["capabilities"], []string{"shell_scope"}) {
		t.Fatalf("answer: %v", answer)
	}
	var seen int64
	if err := s.db.QueryRow("SELECT hook_seen_ms FROM Execution WHERE id=?", answer["execution_id"]).Scan(&seen); err != nil || seen != clock.Now().UnixMilli() {
		t.Fatalf("hook seen at %d: %v", seen, err)
	}
	var scope string
	if err := s.db.QueryRow("SELECT scope_json FROM ReducedShellSnapshot WHERE execution_id=?", answer["execution_id"]).Scan(&scope); err != nil || scope != `{"mode":"dirty","submodules":["lib"]}` {
		t.Fatalf("stored scope %q: %v", scope, err)
	}
}
