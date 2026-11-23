package store

import "testing"

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
	))
	if out["action"] != "allow" || len(out["granted"].([]map[string]any)) != 4 {
		t.Fatalf("grant: %v", out)
	}
	rows := map[string]liveClaim{}
	for _, row := range liveClaims(t, s, repo) {
		rows[row.path] = row
	}
	if len(rows) != 4 {
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
	if dRow.key.Valid || dRow.wpath.Valid || dRow.branch.Valid {
		t.Fatalf("ambiguous duplicate wore a checkout: %+v", dRow)
	}

	setScope(t, s, repo, "worktree")
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
