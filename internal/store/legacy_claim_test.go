package store

import "testing"

// A pre-upgrade Execution has no worktree key, and its automatic claim has none
// either. After the repository opts in, the session's next grouped edit of its
// own checkout adopts that row and records the same key on the Execution. A
// shell commit then releases the keyed claim. Another checkout's claim stays,
// and another session's null-key claim still blocks.
func TestLegacyClaimIsAdoptedAndReleasedWithTheCheckout(t *testing.T) {
	s, _, _ := testStore(t)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "legacy", Cwd: "/repo/wt1", Worktree: "/repo/wt1", RemoteURL: scopeRemote, RootCommit: "root", Branch: "main", Source: "hook"})
	a := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	repo := out["repository_id"].(string)
	if executionKey(t, s, a.ExecutionID).Valid {
		t.Fatal("registration without a worktree id stored a key")
	}
	first := call(t, s, "touch", touchRequest(a, "legacy", "legacy.go"))
	if first["action"] != "allow" {
		t.Fatal(first)
	}
	fence := claimsOn(liveClaims(t, s, repo), "legacy.go")[0].fence
	setScope(t, s, repo, "worktree")

	other, _ := scopeSession(t, s, "", "other", "/repo/wt2", "dev:2", "other")
	call(t, s, "touch", touchRequest(other, "held", "held.go"))
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL WHERE execution_id=? AND path='held.go'", other.ExecutionID); err != nil {
		t.Fatal(err)
	}
	blocked := call(t, s, "touch_files", filesTouch(a, "blocked", wtGroup("/repo/wt1", "dev:1", "main", "held.go")))
	if blocked["action"] != "deny" || blocked["advisories"] != nil {
		t.Fatal("another session's null key was exempt", blocked)
	}

	got := call(t, s, "touch_files", filesTouch(a, "adopt", wtGroup("/repo/wt1", "dev:1", "main", "legacy.go")))
	rows := claimsOn(liveClaims(t, s, repo), "legacy.go")
	key := executionKey(t, s, a.ExecutionID)
	if got["action"] != "allow" || got["advisories"] != nil || len(rows) != 1 || rows[0].fence != fence || !rows[0].key.Valid || !key.Valid || rows[0].key.String != key.String {
		t.Fatalf("legacy row was not adopted: %v key %v rows %+v", got, key, rows)
	}
	if call(t, s, "touch_files", filesTouch(a, "away", wtGroup("/repo/wt2", "dev:2", "other", "away.go")))["action"] != "allow" {
		t.Fatal("other checkout")
	}
	call(t, s, "touch_files", filesTouch(a, "drop", wtGroup("/repo/wt1", "dev:1", "main", "drop.go")))
	call(t, s, "touch_files", filesTouch(a, "old", wtGroup("/repo/wt1", "dev:1", "main", "old.go")))
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL WHERE execution_id=? AND path='old.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if executionKey(t, s, a.ExecutionID).String != key.String {
		t.Fatal("another checkout replaced the execution key")
	}

	a.Kind, a.Status = "before", map[string]string{"drop.go": " M", "old.go": " M"}
	a.Worktree, a.WorktreeID = "/repo/wt1", "dev:1"
	call(t, s, "reconcile", a)
	a.Kind, a.Status = "after", map[string]string{}
	call(t, s, "reconcile", a)
	left := liveClaims(t, s, repo)
	if len(claimsOn(left, "drop.go")) != 0 || len(claimsOn(left, "old.go")) != 0 || len(claimsOn(left, "away.go")) != 1 || len(claimsOn(left, "legacy.go")) != 1 {
		t.Fatalf("release: %+v", left)
	}
}
