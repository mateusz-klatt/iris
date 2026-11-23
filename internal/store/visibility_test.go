package store

import (
	"testing"
)

// The console and GET /v1/claims name each claim's own checkout and whether that
// claim is worktree-scoped. A legacy row with no display path stays unknown, and
// a null key is never worktree-scoped just because the policy opted in (ADR 037).
func TestClaimListsNameTheCheckoutAndTheEffectiveScope(t *testing.T) {
	s, _, _ := testStore(t)
	held, registered := scopeSession(t, s, "host-a", "held", "/repo/wt1", "dev:1", "held-branch")
	other, _ := scopeSession(t, s, "host-b", "other", "/repo/wt1", "dev:2", "other-branch")
	repo := registered["repository_id"].(string)
	call(t, s, "touch", touchRequest(held, "auto-held", "src/api.go"))
	call(t, s, "claim", claimRequest(held, "explicit-held", "docs/keep.md", "exact"))

	before := listedClaim(t, call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any), "src/api.go", "host-a")
	if before["effective_scope"] != "repository" || before["worktree_path"] != "/repo/wt1" || before["branch"] != "held-branch" || before["host"] != "host-a" {
		t.Fatalf("default policy presented a keyed automatic claim as worktree-scoped: %v", before)
	}
	explicit := listedClaim(t, call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any), "docs/keep.md", "host-a")
	if explicit["effective_scope"] != "repository" || explicit["worktree_path"] != "/repo/wt1" || explicit["origin"] != "explicit" {
		t.Fatalf("explicit claim: %v", explicit)
	}

	setScope(t, s, repo, "worktree")
	call(t, s, "touch", touchRequest(other, "auto-other", "src/api.go"))
	opted := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	keyed := listedClaim(t, opted, "src/api.go", "host-a")
	samePath := listedClaim(t, opted, "src/api.go", "host-b")
	if keyed["effective_scope"] != "worktree" || samePath["effective_scope"] != "worktree" || keyed["host"] == samePath["host"] {
		t.Fatalf("opted-in automatic claims: %v and %v", keyed, samePath)
	}
	if listedClaim(t, opted, "docs/keep.md", "host-a")["effective_scope"] != "repository" {
		t.Fatal("an explicit claim became worktree-scoped")
	}
	var storedKey string
	if err := s.db.QueryRow("SELECT COALESCE(worktree_key,'') FROM Claim WHERE fence=?", keyed["fence"]).Scan(&storedKey); err != nil || storedKey == "" {
		t.Fatal("reading the claim back changed its key", err, storedKey)
	}

	plain, _ := session(t, s, 9)
	call(t, s, "touch", touchRequest(plain, "legacy-shaped", "src/old.go"))
	if _, err := s.db.Exec("UPDATE Claim SET worktree_key=NULL,worktree_path=NULL,branch=NULL WHERE path='src/old.go'"); err != nil {
		t.Fatal(err)
	}
	legacy := listedClaim(t, call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any), "src/old.go", "host")
	if legacy["worktree_path"] != "" || legacy["branch"] != "" || legacy["effective_scope"] != "repository" {
		t.Fatalf("legacy null row was guessed: %v", legacy)
	}

	setScope(t, s, repo, "repository")
	if listedClaim(t, call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any), "src/api.go", "host-a")["effective_scope"] != "repository" {
		t.Fatal("turning the policy back left a claim worktree-scoped")
	}
	if err := s.db.QueryRow("SELECT COALESCE(worktree_key,'') FROM Claim WHERE fence=?", keyed["fence"]).Scan(&storedKey); err != nil || storedKey == "" {
		t.Fatal("the policy change rewrote the key", err, storedKey)
	}
}

func listedClaim(t *testing.T, claims []map[string]any, path, host string) map[string]any {
	t.Helper()
	for _, c := range claims {
		if c["path"] == path && c["host"] == host {
			return c
		}
	}
	t.Fatalf("no claim %s on %s in %v", path, host, claims)
	return nil
}

// The overview names the policy of each project and omits it for a host's context
// of sessions outside git, which has no claim.auto_scope in force.
func TestTheOverviewReportsClaimAutoScopeForAProjectOnly(t *testing.T) {
	s, _, _ := testStore(t)
	_, registered := session(t, s, 1)
	repo := registered["repository_id"].(string)
	outsideSession(t, s, "home", "o", "/home/u/tmp")
	sawProject, sawOutside := false, false
	for _, row := range call(t, s, "ui_overview", Request{})["repositories"].([]map[string]any) {
		_, reported := row["claim_auto_scope"]
		switch row["kind"] {
		case "repository":
			sawProject = true
			if !reported || row["claim_auto_scope"] != "repository" {
				t.Fatalf("project scope: %v", row)
			}
		case "outside":
			sawOutside = true
			if reported {
				t.Fatalf("a context outside git reported a scope: %v", row)
			}
		}
	}
	if !sawProject || !sawOutside {
		t.Fatal("overview was missing a project or a context outside git")
	}
	setScope(t, s, repo, "worktree")
	checked := call(t, s, "check", Request{Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root"})
	if checked["claim_auto_scope"] != "worktree" {
		t.Fatalf("check did not report the opted-in scope: %v", checked["claim_auto_scope"])
	}
	for _, row := range call(t, s, "ui_overview", Request{})["repositories"].([]map[string]any) {
		if row["id"] == repo && row["claim_auto_scope"] != "worktree" {
			t.Fatalf("overview did not follow the new scope: %v", row)
		}
	}
	setScope(t, s, repo, "repository")
	if call(t, s, "check", Request{Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root"})["claim_auto_scope"] != "repository" {
		t.Fatal("check kept the old scope")
	}
}
