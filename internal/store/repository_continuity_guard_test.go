package store

import "testing"

func TestRepositoryCheckoutContinuitySeparatesGlobalAndLiteralRemotes(t *testing.T) {
	s, _, _ := testStore(t)
	r, global := unknownCheckout(t, s, rewriteRemote)
	// This is an scp-style remote whose host is an SSH alias. Its literal text
	// happens to equal the normalized global alias for the original remote.
	r.RemoteURL = "remote:github.com/mateusz-klatt/iris"
	if remoteAlias(r.RemoteURL) != "" {
		t.Fatal("fixture must remain scoped to its literal remote spelling")
	}
	r.SessionID = "literal-remote"
	literal := call(t, s, "sessions", r)
	if literal["repository_id"] == global["repository_id"] {
		t.Fatal("a literal remote collided with a different global remote's checkout continuity")
	}
	r.SessionID = "literal-remote-again"
	if again := call(t, s, "sessions", r); again["repository_id"] != literal["repository_id"] {
		t.Fatal("the literal remote did not preserve its own checkout continuity")
	}
}

func TestRepositoryGuardReusesCheckoutContinuityAndClaims(t *testing.T) {
	s, _, _ := testStore(t)
	r, first := unknownCheckout(t, s, rewriteRemote)
	// Learning a root permits another worktree to register in this repository.
	// The guard at the original checkout may still be unable to read that root.
	r.RootCommit = "checkout-root"
	call(t, s, "sessions", r)
	peer := rewriteRegistration("guard-peer", "peer", r.RootCommit)
	peer.Worktree, peer.Cwd, peer.CommonDir = "/peer/iris", "/peer/iris", "/peer/iris/.git"
	holder := call(t, s, "sessions", peer)
	if holder["repository_id"] != first["repository_id"] {
		t.Fatal("the known-root peer did not join the checkout repository")
	}
	ex := Request{ExecutionID: holder["execution_id"].(string), Generation: holder["generation"].(int64)}
	call(t, s, "claim", claimRequest(ex, "guard-visible", "shared.go", "exact"))
	r.RootCommit, r.Paths = "", []string{"shared.go"}
	out := call(t, s, "check", r)
	conflicts := out["conflicts"].([]map[string]any)
	if len(conflicts) != 1 || conflicts[0]["path"] != "shared.go" || conflicts[0]["host"] != peer.Host {
		t.Fatalf("the unknown-root guard lost the checkout's peer claim: %v", out)
	}
	if count(t, s, "SELECT COUNT(*) FROM Repository") != 3 {
		t.Fatal("the unknown-root guard allocated another repository despite explicit continuity")
	}
}

func TestRepositoryGuardEstablishesAndInvalidatesCheckoutContinuity(t *testing.T) {
	s, _, _ := testStore(t)
	r := rewriteRegistration("guard-host", "old", "old")
	old := call(t, s, "sessions", r)
	r.SessionID, r.RootCommit = "current", "current"
	current := call(t, s, "sessions", r)
	r.RootCommit = ""
	call(t, s, "check", r)
	var recorded string
	alias := checkoutRepositoryAlias(r)
	if err := s.db.QueryRow("SELECT repository_id FROM RepositoryAlias WHERE alias=?", alias).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded == old["repository_id"] || recorded == current["repository_id"] {
		t.Fatal("an unknown-root guard chose an ambiguous historical identity")
	}
	r.SessionID = "after-guard"
	if got := call(t, s, "sessions", r); got["repository_id"] != recorded {
		t.Fatal("a session did not reuse the guard's explicit checkout resolution")
	}
	r.RootCommit = "current"
	call(t, s, "check", r)
	if count(t, s, "SELECT COUNT(*) FROM RepositoryAlias WHERE alias=?", alias) != 0 {
		t.Fatal("a known-root guard retained continuity pointing to another repository")
	}
	r.RootCommit, r.SessionID = "", "after-known-guard"
	fresh := call(t, s, "sessions", r)
	if fresh["repository_id"] == recorded || fresh["repository_id"] == current["repository_id"] || fresh["repository_id"] == old["repository_id"] {
		t.Fatal("a known-root guard left stale continuity or selected an ambiguous history")
	}
	r.SessionID = "after-known-guard-again"
	if again := call(t, s, "sessions", r); again["repository_id"] != fresh["repository_id"] {
		t.Fatal("the checkout failed to stabilize after its known-root guard resolution")
	}
}
