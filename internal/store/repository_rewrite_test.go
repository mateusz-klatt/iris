package store

import (
	"fmt"
	"testing"
)

const rewriteRemote = "https://github.com/mateusz-klatt/iris.git"

func rewriteRegistration(host, session, root string) Request {
	return Request{Client: "codex", Host: host, SessionID: session, Cwd: "/src/iris", Worktree: "/src/iris", CommonDir: "/src/iris/.git", RemoteURL: rewriteRemote, RootCommit: root}
}

func TestRepositoryHistoryRewriteJoinsCurrentRoot(t *testing.T) {
	s, _, _ := testStore(t)
	old := call(t, s, "sessions", rewriteRegistration("one", "old", "0f101bb"))
	call(t, s, "sessions", rewriteRegistration("one", "second-old", "e681a54"))
	current := call(t, s, "sessions", rewriteRegistration("one", "rewritten", "7788818"))
	if old["repository_id"] == current["repository_id"] {
		t.Fatal("a changed root reused the historical repository")
	}
	holder := Request{ExecutionID: current["execution_id"].(string), Generation: current["generation"].(int64)}
	call(t, s, "claim", claimRequest(holder, "shared", "shared.go", "exact"))
	for _, host := range []string{"two", "three", "four"} {
		got := call(t, s, "sessions", rewriteRegistration(host, "current", "7788818"))
		if got["repository_id"] != current["repository_id"] {
			t.Errorf("host %s split current history: got %v, want %v", host, got["repository_id"], current["repository_id"])
		}
		foundHolder := false
		for _, peer := range got["peers"].([]map[string]any) {
			if peer["id"] == current["agent"].(map[string]any)["id"] {
				foundHolder = true
			}
		}
		if !foundHolder {
			t.Errorf("host %s did not see the other host's peer", host)
		}
		ex := Request{ExecutionID: got["execution_id"].(string), Generation: got["generation"].(int64)}
		expectError(t, s, "claim", claimRequest(ex, "overlap", "shared.go", "exact"), "conflict")
		guard := rewriteRegistration(host, "", "7788818")
		guard.Paths = []string{"shared.go"}
		if conflicts := call(t, s, "check", guard)["conflicts"].([]map[string]any); len(conflicts) != 1 {
			t.Errorf("host %s guard missed the other host's claim: %v", host, conflicts)
		}
	}
	// The original path also has two aliases, each mapped to both histories.
	for n := range 3 {
		got := call(t, s, "sessions", rewriteRegistration("one", fmt.Sprintf("again-%d", n), "7788818"))
		if got["repository_id"] != current["repository_id"] {
			t.Errorf("duplicate local/gitdir aliases split registration: got %v, want %v", got["repository_id"], current["repository_id"])
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Repository").Scan(&count); err != nil || count != 3 {
		t.Fatalf("repositories = %d, want three histories: %v", count, err)
	}
}

func TestRepositoryCurrentRootBeatsUnknownAliases(t *testing.T) {
	for _, scope := range []string{"local", "remote"} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := testStore(t)
			current := call(t, s, "sessions", rewriteRegistration("one", "current", "current"))
			if _, err := s.db.Exec("INSERT INTO Repository(id,root_commit) VALUES('rp_unknown','')"); err != nil {
				t.Fatal(err)
			}
			alias := remoteAlias(rewriteRemote)
			host := "fresh"
			if scope == "local" {
				alias, host = "local:one:/src/iris", "one"
			}
			if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,'rp_unknown')", alias); err != nil {
				t.Fatal(err)
			}
			got := call(t, s, "sessions", rewriteRegistration(host, "again", "current"))
			if got["repository_id"] != current["repository_id"] {
				t.Fatal("an unknown root displaced the exact root match", got)
			}
			var root string
			if err := s.db.QueryRow("SELECT root_commit FROM Repository WHERE id='rp_unknown'").Scan(&root); err != nil || root != "" {
				t.Fatalf("unselected unknown identity changed: %q, %v", root, err)
			}
		})
	}
}

func TestRepositoryRemoteLearnsRoot(t *testing.T) {
	s, _, _ := testStore(t)
	first := call(t, s, "sessions", rewriteRegistration("one", "unknown", ""))
	learned := call(t, s, "sessions", rewriteRegistration("two", "learned", "current"))
	if learned["repository_id"] != first["repository_id"] {
		t.Fatal("a unique unknown remote could not learn its root")
	}
	var root string
	if err := s.db.QueryRow("SELECT root_commit FROM Repository WHERE id=?", first["repository_id"]).Scan(&root); err != nil || root != "current" {
		t.Fatalf("a root learned through the remote was lost: %q, %v", root, err)
	}
	other := call(t, s, "sessions", rewriteRegistration("three", "other", "other-root"))
	if other["repository_id"] == first["repository_id"] {
		t.Fatal("the learned remote root admitted a contradictory history")
	}
}

func TestRepositoryUnknownCandidateDoesNotBridgeHistories(t *testing.T) {
	s, _, _ := testStore(t)
	old := call(t, s, "sessions", rewriteRegistration("one", "old", "old"))
	if _, err := s.db.Exec("INSERT INTO Repository(id,root_commit) VALUES('rp_unknown','')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,'rp_unknown')", remoteAlias(rewriteRemote)); err != nil {
		t.Fatal(err)
	}
	got := call(t, s, "sessions", rewriteRegistration("fresh", "new", "current"))
	if got["repository_id"] == old["repository_id"] || got["repository_id"] == "rp_unknown" {
		t.Fatal("an unknown candidate guessed which history it represented", got)
	}
}

func TestRepositoryLocalHistoricalRootsDoNotHideCurrentRemote(t *testing.T) {
	s, _, _ := testStore(t)
	call(t, s, "sessions", rewriteRegistration("one", "old-one", "old-one"))
	call(t, s, "sessions", rewriteRegistration("one", "old-two", "old-two"))
	current := call(t, s, "sessions", rewriteRegistration("two", "current", "current"))
	got := call(t, s, "sessions", rewriteRegistration("one", "current", "current"))
	if got["repository_id"] != current["repository_id"] {
		t.Fatal("incompatible local histories hid the unambiguous current remote", got)
	}
}

func TestRepositoryUnknownRootDoesNotChooseAHistory(t *testing.T) {
	s, _, _ := testStore(t)
	old := call(t, s, "sessions", rewriteRegistration("one", "old", "old"))
	current := call(t, s, "sessions", rewriteRegistration("one", "new", "current"))
	for _, host := range []string{"one", "fresh"} {
		r := rewriteRegistration(host, "unknown", "")
		unknown := call(t, s, "sessions", r)
		if unknown["repository_id"] == old["repository_id"] || unknown["repository_id"] == current["repository_id"] {
			t.Errorf("host %s guessed an unknown root's history: %v", host, unknown["repository_id"])
		}
		again := call(t, s, "sessions", r)
		if again["execution_id"] != unknown["execution_id"] || again["repository_id"] != unknown["repository_id"] {
			t.Fatal("repeated unknown-root registration lost its explicit continuity")
		}
	}
}

func TestRepositoryRepeatedRegistrationLearnsIdentity(t *testing.T) {
	s, _, _ := testStore(t)
	r := rewriteRegistration("one", "running", "")
	r.RemoteURL = ""
	first := call(t, s, "sessions", r)
	ex := Request{ExecutionID: first["execution_id"].(string), Generation: first["generation"].(int64)}
	claim := call(t, s, "claim", claimRequest(ex, "keep", "keep.go", "exact"))
	r.RemoteURL, r.RootCommit = rewriteRemote, "7788818"
	again := call(t, s, "sessions", r)
	if again["execution_id"] != first["execution_id"] || again["agent"].(map[string]any)["id"] != first["agent"].(map[string]any)["id"] || again["generation"] != first["generation"] {
		t.Fatalf("identity refresh replaced an Execution or mailbox: %v -> %v", first, again)
	}
	var root string
	if err := s.db.QueryRow("SELECT root_commit FROM Repository WHERE id=?", first["repository_id"]).Scan(&root); err != nil || root != r.RootCommit {
		t.Fatalf("repeated registration did not learn its root: %q, %v", root, err)
	}
	other := call(t, s, "sessions", rewriteRegistration("two", "other", r.RootCommit))
	if other["repository_id"] != first["repository_id"] {
		t.Fatal("repeated registration did not publish its learned identity")
	}
	claims := call(t, s, "claims", Request{RepositoryID: first["repository_id"].(string)})["claims"].([]map[string]any)
	if len(claims) != 1 || claims[0]["fence"] != claim["fence"] {
		t.Fatalf("identity refresh changed claims: %v", claims)
	}
}

func TestRepositoryRepeatedRegistrationRefusesChangedRoot(t *testing.T) {
	s, _, _ := testStore(t)
	r := rewriteRegistration("one", "running", "old")
	first := call(t, s, "sessions", r)
	ex := Request{ExecutionID: first["execution_id"].(string), Generation: first["generation"].(int64)}
	call(t, s, "claim", claimRequest(ex, "keep", "keep.go", "exact"))
	before := databaseState(t, s)
	r.RootCommit = "current"
	expectError(t, s, "sessions", r, "conflict")
	if after := databaseState(t, s); after != before {
		t.Fatal("refused identity change moved records")
	}
}

func TestRepositoryDuplicateCurrentRootsRemainAmbiguous(t *testing.T) {
	s, _, _ := testStore(t)
	one := call(t, s, "sessions", rewriteRegistration("one", "one", "current"))
	// A split database has two different ids for the same remote and current root.
	if _, err := s.db.Exec("INSERT INTO Repository(id,root_commit) VALUES('rp_duplicate','current')"); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{remoteAlias(rewriteRemote), "root:current", "local:one:/src/iris", "gitdir:one:/src/iris/.git"} {
		if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,'rp_duplicate')", alias); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"one", "fresh"} {
		got := call(t, s, "sessions", rewriteRegistration(host, "ambiguous", "current"))
		if got["repository_id"] == one["repository_id"] || got["repository_id"] == "rp_duplicate" {
			t.Fatalf("host %s arbitrarily chose a duplicate identity: %v", host, got["repository_id"])
		}
	}
}
