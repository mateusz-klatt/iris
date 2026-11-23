package store

import (
	"encoding/json"
	"testing"
)

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

// Two keyed groups can both select one own null-key fence. Adoption would
// narrow it to one checkout, so the call grants nothing and leaves the fence.
// The same fence already explicit covers a keyed request and an unkeyed one.
// A null fence neither group adopts still covers both. An existing keyed fence
// and a different checkout's new grant stay an exempt pair.
func TestPlannedGrantsFollowTheFenceTheyWillHold(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	if call(t, s, "touch", touchRequest(a, "seed", "a.go"))["action"] != "allow" {
		t.Fatal("seed")
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL,worktree_path=NULL,branch=NULL WHERE execution_id=? AND path='a.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	narrowed := call(t, s, "touch_files", filesTouch(a, "narrow", wtGroup("/repo/wt1", "dev:1", "main", "a.go"), wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	rows := claimsOn(liveClaims(t, s, repo), "a.go")
	if narrowed["action"] != "deny" || len(narrowed["granted"].([]map[string]any)) != 0 || len(rows) != 1 || rows[0].key.Valid || rows[0].wpath.Valid {
		t.Fatalf("both grants took the narrowed fence: %v %+v", narrowed, rows)
	}

	if _, err := s.db.Exec("UPDATE Claim SET worktree_path='/repo/wt1' WHERE execution_id=? AND path='a.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	samePath := wtGroup("/repo/wt1", "dev:1", "main", "a.go")
	otherID := samePath
	otherID.WorktreeID = "dev:9"
	split := call(t, s, "touch_files", filesTouch(a, "split-key", samePath, otherID))
	rows = claimsOn(liveClaims(t, s, repo), "a.go")
	if split["action"] != "deny" || len(rows) != 1 || rows[0].key.Valid {
		t.Fatalf("two adoptions shared one fence: %v %+v", split, rows)
	}

	call(t, s, "claim", claimRequest(a, "explicit", "held.go", "exact"))
	explicit := claimsOn(liveClaims(t, s, repo), "held.go")
	unkeyed := ClaimGroup{Worktree: "/repo/wt2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "other", Paths: []string{"held.go"}}
	shared := call(t, s, "touch_files", filesTouch(a, "shared", wtGroup("/repo/wt1", "dev:1", "main", "held.go"), unkeyed))
	got := shared["granted"].([]map[string]any)
	if shared["action"] != "allow" || len(got) != 2 || got[0]["fence"] != explicit[0].fence || got[1]["fence"] != explicit[0].fence || len(claimsOn(liveClaims(t, s, repo), "held.go")) != 1 {
		t.Fatalf("explicit fence was not reused: %v %+v", shared, liveClaims(t, s, repo))
	}

	if call(t, s, "touch_files", filesTouch(a, "recorded", wtGroup("/repo/wt2", "dev:2", "other", "b.go")))["action"] != "allow" {
		t.Fatal("recorded")
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL WHERE execution_id=? AND path='b.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	both := call(t, s, "touch_files", filesTouch(a, "stay", wtGroup("/repo/wt1", "dev:1", "main", "b.go"), wtGroup("/repo/wt3", "dev:3", "third", "b.go")))
	brows := claimsOn(liveClaims(t, s, repo), "b.go")
	bgot := both["granted"].([]map[string]any)
	if both["action"] != "allow" || len(bgot) != 2 || bgot[0]["fence"] != brows[0].fence || bgot[1]["fence"] != brows[0].fence || brows[0].key.Valid || brows[0].wpath.String != "/repo/wt2" {
		t.Fatalf("null fence was narrowed: %v %+v", both, brows)
	}

	if call(t, s, "touch", touchRequest(a, "keyed", "c.go"))["action"] != "allow" {
		t.Fatal("keyed")
	}
	exempt := call(t, s, "touch_files", filesTouch(a, "exempt-existing", wtGroup("/repo/wt1", "dev:1", "main", "c.go"), wtGroup("/repo/wt2", "dev:2", "other", "c.go")))
	if exempt["action"] != "allow" || len(exempt["granted"].([]map[string]any)) != 2 || len(claimsOn(liveClaims(t, s, repo), "c.go")) != 2 {
		t.Fatalf("existing key refused an exempt checkout: %v %+v", exempt, liveClaims(t, s, repo))
	}
	reversed := call(t, s, "touch_files", filesTouch(a, "exempt-reversed", wtGroup("/repo/wt2", "dev:2", "other", "c.go"), wtGroup("/repo/wt1", "dev:1", "main", "c.go")))
	if reversed["action"] != "allow" || len(reversed["granted"].([]map[string]any)) != 2 {
		t.Fatalf("reversed exempt pair: %v", reversed)
	}
}

// A replay matches the checkout the allow recorded. After that fence's key
// changes, the replay is lost. An allow cached before the field existed follows
// the fence's key at replay time.
func TestCachedAllowMatchesTheCheckoutItGranted(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	req := filesTouch(a, "once", wtGroup("/repo/wt1", "dev:1", "main", "a.go"))
	allowed := call(t, s, "touch_files", req)
	granted := allowed["granted"].([]map[string]any)
	key := executionKey(t, s, a.ExecutionID)
	if allowed["action"] != "allow" || len(granted) != 1 || granted[0]["worktree_key"] != key.String {
		t.Fatalf("grant omitted its checkout: %v", allowed)
	}
	if again := call(t, s, "touch_files", req); again["action"] != "allow" {
		t.Fatal(again)
	}
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key='wt_other' WHERE execution_id=? AND path='a.go'", a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, "touch_files", req, "lost")

	var raw string
	if err := s.db.QueryRow("SELECT result_json FROM Operation WHERE key=?", "touch_files:once").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	items := body["granted"].([]any)
	delete(items[0].(map[string]any), "worktree_key")
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE Operation SET result_json=? WHERE key=?", string(encoded), "touch_files:once"); err != nil {
		t.Fatal(err)
	}
	if replay := call(t, s, "touch_files", req); replay["action"] != "allow" {
		t.Fatal("a cached allow without a checkout followed the new fence key", replay)
	}
}
