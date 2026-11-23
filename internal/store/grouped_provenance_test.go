package store

import "testing"

func listedPath(t *testing.T, s *Store, repo, path string) string {
	t.Helper()
	for _, c := range call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any) {
		if c["path"] == path {
			return c["worktree_path"].(string)
		}
	}
	t.Fatalf("no claim %s", path)
	return ""
}

// A repository-scoped grouped edit used to stamp every path with the first
// group's checkout. After opt-in, an edit in the second checkout was an
// exempt overlap and was granted. Each path keeps the checkout it was read
// from. The same path from two checkouts stays repository-wide, including
// when neither checkout has a key, so opt-in still blocks it.
func TestRepositoryGroupedEditKeepsProvenanceUntilOptIn(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	unkeyed := func(worktree, path string) ClaimGroup {
		return ClaimGroup{Worktree: worktree, RemoteURL: scopeRemote, RootCommit: "root", Branch: "side", Paths: []string{path}}
	}
	out := call(t, s, "touch_files", filesTouch(a, "mixed",
		wtGroup("/repo/wt1", "dev:1", "main", "a.go"),
		wtGroup("/repo/wt2", "dev:2", "other", "b.go"),
		wtGroup("/repo/wt1", "dev:1", "later", "a.go"),
		unkeyed("/repo/wt1", "c.go"),
		unkeyed("/repo/wt1", "c.go"),
		unkeyed("/repo/wt1", "d.go"),
		unkeyed("/repo/wt2", "d.go"),
		unkeyed("/repo/wt2", "e.go"),
		unkeyed("/repo/wt3", "e.go"),
	))
	if out["action"] != "allow" || len(out["granted"].([]map[string]any)) != 5 {
		t.Fatalf("grant: %v", out)
	}
	rows := map[string]liveClaim{}
	for _, row := range liveClaims(t, s, repo) {
		rows[row.path] = row
	}
	if len(rows) != 5 {
		t.Fatalf("claims: %+v", rows)
	}
	aRow, bRow := rows["a.go"], rows["b.go"]
	if !aRow.key.Valid || !bRow.key.Valid || aRow.key.String == bRow.key.String || aRow.wpath.String != "/repo/wt1" || bRow.wpath.String != "/repo/wt2" || aRow.branch.String != "main" {
		t.Fatalf("provenance: a=%+v b=%+v", aRow, bRow)
	}
	cRow := rows["c.go"]
	if cRow.key.Valid || cRow.wpath.String != "/repo/wt1" {
		t.Fatalf("same unidentified checkout: %+v", cRow)
	}
	dRow := rows["d.go"]
	if dRow.key.Valid || dRow.wpath.String != ambiguousWorktree || dRow.branch.Valid {
		t.Fatalf("ambiguous duplicate wore a checkout: %+v", dRow)
	}
	if listedPath(t, s, repo, "d.go") != "" {
		t.Fatal("ambiguous claim was shown as a checkout")
	}

	setScope(t, s, repo, "worktree")
	guard := call(t, s, "check", Request{Host: "host", Cwd: "/repo/wt1", Worktree: "/repo/wt1", WorktreeID: "dev:1", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"e.go"}})
	if messageOf(guard) == "" || guard["advisories"] != nil {
		t.Fatal("ambiguous claim allowed the holder's root", guard)
	}
	second, _ := scopeSession(t, s, "", "second", "/repo/wt2", "dev:2", "other")
	denied := call(t, s, "touch", touchRequest(second, "overlap", "b.go"))
	if denied["action"] != "deny" || denied["advisories"] != nil {
		t.Fatalf("second checkout was exempt: %v", denied)
	}
	wide := call(t, s, "touch", touchRequest(second, "wide", "d.go"))
	if wide["action"] != "deny" || wide["advisories"] != nil {
		t.Fatalf("ambiguous path was exempt: %v", wide)
	}
}

// Under worktree, one request can name a path once with a checkout key and
// once without. Those grants are not rows yet, so each was inserted. The
// null-key claim is repository-wide and the call grants nothing.
func TestPlannedGrantsConflictBeforeInsert(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	unkeyed := ClaimGroup{Worktree: "/repo/wt2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "other", Paths: []string{"a.go"}}
	got := call(t, s, "touch_files", filesTouch(a, "both", wtGroup("/repo/wt1", "dev:1", "main", "a.go"), unkeyed))
	if got["action"] != "deny" || len(got["granted"].([]map[string]any)) != 0 || got["advisories"] != nil {
		t.Fatalf("both grants won: %v", got)
	}
	if rows := liveClaims(t, s, repo); len(rows) != 0 {
		t.Fatalf("inserted %v", rows)
	}
	// The exempt pair is the one that may both win. Distinct paths are not a pair.
	both := call(t, s, "touch_files", filesTouch(a, "exempt", wtGroup("/repo/wt1", "dev:1", "main", "a.go"), wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	if both["action"] != "allow" || len(both["granted"].([]map[string]any)) != 2 || len(liveClaims(t, s, repo)) != 2 {
		t.Fatalf("exempt pair: %v claims %v", both, liveClaims(t, s, repo))
	}
	separate := ClaimGroup{Worktree: "/repo/wt2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "other", Paths: []string{"c.go"}}
	other := call(t, s, "touch_files", filesTouch(a, "other", wtGroup("/repo/wt1", "dev:1", "main", "b.go"), separate))
	if other["action"] != "allow" || len(other["granted"].([]map[string]any)) != 2 {
		t.Fatalf("distinct paths: %v", other)
	}
}

// A repository-scoped edit in a second checkout used to keep the first
// checkout's key. After opt-in that key made the second checkout advisory.
// The renewed row becomes repository-wide, and a later edit there conflicts.
func TestRepositoryRenewalForgetsACheckoutItNoLongerAgreesWith(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	if call(t, s, "touch", touchRequest(a, "first", "a.go"))["action"] != "allow" {
		t.Fatal("first")
	}
	again := call(t, s, "touch_files", filesTouch(a, "second", wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	rows := claimsOn(liveClaims(t, s, repo), "a.go")
	if again["action"] != "allow" || len(rows) != 1 || rows[0].key.Valid || rows[0].wpath.String != ambiguousWorktree || rows[0].branch.Valid {
		t.Fatalf("renewal kept a checkout: %v %+v", again, rows)
	}
	kept := call(t, s, "touch_files", filesTouch(a, "third", wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	rows = claimsOn(liveClaims(t, s, repo), "a.go")
	if kept["action"] != "allow" || len(rows) != 1 || rows[0].key.Valid || rows[0].wpath.String != ambiguousWorktree {
		t.Fatalf("second renewal moved the ambiguous row: %v %+v", kept, rows)
	}
	setScope(t, s, repo, "worktree")
	second, _ := scopeSession(t, s, "", "second", "/repo/wt2", "dev:2", "other")
	denied := call(t, s, "touch", touchRequest(second, "overlap", "a.go"))
	if denied["action"] != "deny" || denied["advisories"] != nil {
		t.Fatalf("opt-in treated the stale key as exempt: %v", denied)
	}
	a.Kind, a.Status = "before", map[string]string{"a.go": " M"}
	call(t, s, "reconcile", a)
	a.Kind, a.Status = "after", map[string]string{}
	call(t, s, "reconcile", a)
	if len(claimsOn(liveClaims(t, s, repo), "a.go")) != 1 {
		t.Fatal("the ambiguous row was released with the other checkout", liveClaims(t, s, repo))
	}
}

// An own null key recorded in one checkout stays there when the session edits
// from another. A legacy row that recorded no checkout is still adopted and
// released with this checkout.
func TestANullKeyRecordedElsewhereIsNotAdoptedOrReleased(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	elsewhere := ClaimGroup{Worktree: "/repo/wt2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "other", Paths: []string{"b.go"}}
	if call(t, s, "touch_files", filesTouch(a, "there", elsewhere))["action"] != "allow" {
		t.Fatal("there")
	}
	moved := call(t, s, "touch", touchRequest(a, "here", "b.go"))
	rows := claimsOn(liveClaims(t, s, repo), "b.go")
	if moved["action"] != "allow" || moved["advisories"] != nil || len(rows) != 1 || rows[0].key.Valid || rows[0].wpath.String != "/repo/wt2" {
		t.Fatalf("activity here adopted the other checkout: %v %+v", moved, rows)
	}
	if call(t, s, "touch", touchRequest(a, "legacy", "old.go"))["action"] != "allow" {
		t.Fatal("legacy")
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL,worktree_path=NULL,branch=NULL WHERE execution_id=? AND path='old.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	adopted := call(t, s, "touch", touchRequest(a, "adopt", "old.go"))
	old := claimsOn(liveClaims(t, s, repo), "old.go")
	key := executionKey(t, s, a.ExecutionID)
	if adopted["action"] != "allow" || len(old) != 1 || !old[0].key.Valid || old[0].key.String != key.String || old[0].wpath.String != "/repo/wt1" {
		t.Fatalf("legacy row was not adopted: %v %+v", adopted, old)
	}
	if call(t, s, "touch", touchRequest(a, "blank", "blank.go"))["action"] != "allow" {
		t.Fatal("blank")
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL,worktree_path=NULL,branch=NULL WHERE execution_id=? AND path='blank.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	a.Kind, a.Status = "before", map[string]string{"b.go": " M", "blank.go": " M", "old.go": " M"}
	call(t, s, "reconcile", a)
	a.Kind, a.Status = "after", map[string]string{}
	call(t, s, "reconcile", a)
	left := liveClaims(t, s, repo)
	if len(claimsOn(left, "b.go")) != 1 || len(claimsOn(left, "blank.go")) != 0 || len(claimsOn(left, "old.go")) != 0 {
		t.Fatalf("release: %+v", left)
	}
}
