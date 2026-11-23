package store

import (
	"strings"
	"testing"
	"time"
)

// The default scope refuses the same overlaps v0.1.13 refused: an explicit
// claim, an automatic claim, a grouped edit that touches a held path in
// another linked worktree, a shell edit from outside the family, and a commit
// check from another checkout. A child in another checkout is still warned and
// still receives the automatic claim. The holder's own checkout is not a conflict.
func TestDefaultScopeConflictsMatchV0113(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := scopeSession(t, s, "", "holder", "/repo/wt1", "dev:holder", "main")
	other, _ := scopeSession(t, s, "", "other", "/repo/wt2", "dev:other", "side")

	call(t, s, "claim", claimRequest(holder, "explicit", "a.go", "exact"))
	expectError(t, s, "claim", claimRequest(other, "explicit-other", "a.go", "exact"), "conflict")
	call(t, s, "release", releaseAll(holder))

	if got := call(t, s, "touch", touchRequest(holder, "auto", "a.go")); got["action"] != "allow" || len(got["granted"].([]string)) != 1 {
		t.Fatal(got)
	}
	auto := call(t, s, "touch", touchRequest(other, "auto-other", "a.go"))
	if auto["action"] != "deny" || auto["advisories"] != nil || len(auto["granted"].([]string)) != 0 {
		t.Fatal(auto)
	}

	grouped := call(t, s, "touch_files", filesTouch(other, "grouped",
		wtGroup("/repo/wt2", "dev:other", "side", "a.go"),
		wtGroup("/repo/wt2", "dev:other", "side", "free.go")))
	if grouped["action"] != "deny" || grouped["advisories"] != nil || len(grouped["granted"].([]map[string]any)) != 0 || len(claimsOn(liveClaims(t, s, holderRepo(t, s, holder)), "free.go")) != 0 {
		t.Fatalf("grouped edit granted across a conflict: %v claims %v", grouped, liveClaims(t, s, holderRepo(t, s, holder)))
	}

	other.Kind, other.Status = "before", map[string]string{}
	call(t, s, "reconcile", other)
	other.Kind, other.Status = "after", map[string]string{"a.go": " M"}
	shell := call(t, s, "reconcile", other)
	if !strings.Contains(messageOf(shell), "Shell edit detected") || shell["advisories"] != nil || len(shell["claimed"].([]string)) != 0 {
		t.Fatal(shell)
	}

	own := call(t, s, "check", scopeCheck("host", "/repo/wt1", "dev:holder", "a.go"))
	away := call(t, s, "check", scopeCheck("host", "/repo/wt2", "dev:other", "a.go"))
	if messageOf(own) != "" || own["advisories"] != nil || len(own["conflicts"].([]map[string]any)) != 0 {
		t.Fatal("the holder's checkout was refused", own)
	}
	if messageOf(away) == "" || away["advisories"] != nil || len(away["conflicts"].([]map[string]any)) != 1 {
		t.Fatal("the other checkout was allowed", away)
	}

	childReg := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "child", ParentSessionID: "holder", Cwd: "/repo/wt3", Worktree: "/repo/wt3", WorktreeID: "dev:child", RemoteURL: scopeRemote, RootCommit: "root", Branch: "child", Source: "hook"})
	child := Request{ExecutionID: childReg["execution_id"].(string), Generation: childReg["generation"].(int64)}
	child.Kind, child.Status = "before", map[string]string{}
	call(t, s, "reconcile", child)
	child.Kind, child.Status = "after", map[string]string{"a.go": " M"}
	family := call(t, s, "reconcile", child)
	if !strings.Contains(messageOf(family), "Shell edit detected") || family["advisories"] != nil || len(family["claimed"].([]string)) != 1 || family["claimed"].([]string)[0] != "a.go" {
		t.Fatal(family)
	}
	if len(claimsOn(liveClaims(t, s, holderRepo(t, s, holder)), "a.go")) != 2 {
		t.Fatal(liveClaims(t, s, holderRepo(t, s, holder)))
	}
}

func holderRepo(t *testing.T, s *Store, holder Request) string {
	t.Helper()
	var repo string
	if err := s.db.QueryRow("SELECT repository_id FROM Execution WHERE id=?", holder.ExecutionID).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Two checkouts of different paths keep their own keys. The same path from two
// checkouts becomes one repository-wide claim with no checkout. A later renew
// does not put a checkout back. A stored worktree value still conflicts, and
// the check says the scope is not enabled.
func TestDisagreeingObservationsStayRepositoryWide(t *testing.T) {
	s, clock, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	split := call(t, s, "touch_files", filesTouch(a, "split",
		wtGroup("/repo/wt1", "dev:1", "main", "a.go"),
		wtGroup("/repo/wt2", "dev:2", "other", "b.go")))
	if split["action"] != "allow" || len(split["granted"].([]map[string]any)) != 2 {
		t.Fatal(split)
	}
	left := claimsOn(liveClaims(t, s, repo), "a.go")
	right := claimsOn(liveClaims(t, s, repo), "b.go")
	if len(left) != 1 || len(right) != 1 || !left[0].key.Valid || !right[0].key.Valid || left[0].key.String == right[0].key.String || left[0].wpath.String != "/repo/wt1" || right[0].wpath.String != "/repo/wt2" {
		t.Fatalf("distinct paths lost their checkouts: %+v %+v", left, right)
	}
	same := call(t, s, "touch_files", filesTouch(a, "same",
		wtGroup("/repo/wt1", "dev:1", "main", "c.go"),
		wtGroup("/repo/wt2", "dev:2", "other", "c.go")))
	if same["action"] != "allow" || len(same["granted"].([]map[string]any)) != 1 {
		t.Fatal(same)
	}
	shared := claimsOn(liveClaims(t, s, repo), "c.go")
	if len(shared) != 1 || shared[0].key.Valid || shared[0].wpath.String != ambiguousWorktree || shared[0].branch.Valid {
		t.Fatalf("one path kept a checkout: %+v", shared)
	}
	var shown string
	for _, c := range call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any) {
		if c["path"] == "c.go" {
			shown, _ = c["worktree_path"].(string)
		}
	}
	if shown != "" {
		t.Fatalf("ambiguous path was listed as %q", shown)
	}
	clock.advance(time.Minute)
	if sameCheckout := call(t, s, "touch", touchRequest(a, "same-checkout", "a.go")); sameCheckout["action"] != "allow" {
		t.Fatal(sameCheckout)
	}
	keptKey := claimsOn(liveClaims(t, s, repo), "a.go")
	if len(keptKey) != 1 || !keptKey[0].key.Valid || keptKey[0].key.String != left[0].key.String || keptKey[0].wpath.String != "/repo/wt1" || keptKey[0].branch.String != "main" || keptKey[0].expires <= left[0].expires {
		t.Fatalf("same-checkout renewal dropped the checkout: %+v", keptKey)
	}
	again := call(t, s, "touch_files", filesTouch(a, "again", wtGroup("/repo/wt2", "dev:2", "other", "a.go")))
	if again["action"] != "allow" {
		t.Fatal(again)
	}
	moved := claimsOn(liveClaims(t, s, repo), "a.go")
	if len(moved) != 1 || moved[0].key.Valid || moved[0].wpath.String != ambiguousWorktree || moved[0].branch.Valid || moved[0].fence != left[0].fence {
		t.Fatalf("a later checkout adopted the claim: %+v", moved)
	}
	clock.advance(time.Minute)
	if renew := call(t, s, "touch_files", filesTouch(a, "renew", wtGroup("/repo/wt1", "dev:1", "main", "a.go"))); renew["action"] != "allow" {
		t.Fatal(renew)
	}
	stayed := claimsOn(liveClaims(t, s, repo), "a.go")
	if len(stayed) != 1 || stayed[0].fence != moved[0].fence || stayed[0].key.Valid || stayed[0].wpath.String != ambiguousWorktree || stayed[0].expires <= moved[0].expires {
		t.Fatalf("renewal moved an ambiguous claim: %+v", stayed)
	}

	kept := call(t, s, "touch_files", filesTouch(a, "kept",
		wtGroup("/repo/wt1", "dev:1", "main", "d.go"),
		wtGroup("/repo/wt1", "dev:1", "feature", "d.go")))
	if kept["action"] != "allow" || len(claimsOn(liveClaims(t, s, repo), "d.go")) != 1 {
		t.Fatal(kept, liveClaims(t, s, repo))
	}
	one := claimsOn(liveClaims(t, s, repo), "d.go")
	if !one[0].key.Valid || one[0].wpath.String != "/repo/wt1" || one[0].branch.String != "main" {
		t.Fatalf("the same checkout was treated as a disagreement: %+v", one)
	}
	plain := ClaimGroup{Worktree: "/repo/plain", RemoteURL: scopeRemote, RootCommit: "root", Paths: []string{"e.go"}}
	if out := call(t, s, "touch_files", filesTouch(a, "plain", plain, plain)); out["action"] != "allow" {
		t.Fatal(out)
	}
	unkeyed := claimsOn(liveClaims(t, s, repo), "e.go")
	if len(unkeyed) != 1 || unkeyed[0].key.Valid || unkeyed[0].wpath.String != "/repo/plain" {
		t.Fatalf("equal unidentified checkouts became ambiguous: %+v", unkeyed)
	}
	otherPlain := plain
	otherPlain.Worktree = "/repo/other"
	if out := call(t, s, "touch_files", filesTouch(a, "differ", plain, otherPlain)); out["action"] != "allow" {
		t.Fatal(out)
	}
	// The second call names e.go again, from two display paths and no key.
	// That renews the existing row into ambiguity rather than inserting another.
	differed := claimsOn(liveClaims(t, s, repo), "e.go")
	if len(differed) != 1 || differed[0].key.Valid || differed[0].wpath.String != ambiguousWorktree {
		t.Fatalf("unidentified disagreement kept a checkout: %+v", differed)
	}

	setScope(t, s, repo, "worktree")
	other, _ := scopeSession(t, s, "", "other", "/repo/wt3", "dev:3", "third")
	if got := call(t, s, "touch", touchRequest(other, "blocked", "b.go")); got["action"] != "deny" || got["advisories"] != nil {
		t.Fatal(got)
	}
	checked := call(t, s, "check", scopeCheck("host", "/repo/wt3", "dev:3", "b.go"))
	if checked["claim_auto_scope"] != "repository" || checked["claim_scope_note"] != worktreeScopeDisabled || messageOf(checked) == "" {
		t.Fatal(checked)
	}
}
