package store

import (
	"context"
	"sort"
	"testing"
)

func holders(t *testing.T, s *Store, alias string) int {
	t.Helper()
	return count(t, s, "SELECT COUNT(*) FROM RepositoryAlias WHERE alias=?", alias)
}

func holds(t *testing.T, s *Store, repo, alias string) bool {
	t.Helper()
	return count(t, s, "SELECT COUNT(*) FROM RepositoryAlias WHERE alias=? AND repository_id=?", alias, repo) == 1
}

// A hook reports the checkout of the event it saw: a submodule, or another project the agent ran a
// command in. A repeated registration from there teaches the Execution's repository nothing and is
// not refused for that checkout's root (ADR 032). From its own worktree it still learns.
func TestARepeatedRegistrationFromAnotherCheckoutLearnsNothing(t *testing.T) {
	s, _, _ := testStore(t)
	r := Request{Client: "codex", Host: "host", SessionID: "super", Cwd: "/src/super", Worktree: "/src/super", CommonDir: "/src/super/.git", RemoteURL: "https://example.com/team/super.git", RootCommit: "super-root"}
	first := call(t, s, "sessions", r)
	repo := first["repository_id"].(string)
	sub := r
	sub.Cwd, sub.Worktree, sub.CommonDir = "/src/super/lib", "/src/super/lib", "/src/super/.git/modules/lib"
	sub.RemoteURL, sub.RootCommit = "https://example.com/team/lib.git", "lib-root"
	again := call(t, s, "sessions", sub)
	if again["execution_id"] != first["execution_id"] || again["repository_id"] != repo {
		t.Fatal("a hook from another checkout moved the Execution", again)
	}
	for _, alias := range []string{"local:host:/src/super/lib", "gitdir:host:/src/super/.git/modules/lib", "remote:example.com/team/lib", "root:lib-root"} {
		if holders(t, s, alias) != 0 {
			t.Fatalf("the repository learned %s from another checkout", alias)
		}
	}
	own := r
	own.RemoteURL = "git@example.com:team/super-renamed.git"
	call(t, s, "sessions", own)
	if !holds(t, s, repo, "remote:example.com/team/super-renamed") {
		t.Fatal("the Execution's own worktree no longer teaches its repository")
	}
}

// An unknown-root checkout whose shared aliases are ambiguous gets an isolated repository. It takes
// no alias another repository holds, at allocation or at a repeated registration, so the next
// checkout there finds no more candidates than before. A known root ends the isolation.
func TestAnAmbiguousUnknownRootAllocationIsIsolatedUntilItLearnsARoot(t *testing.T) {
	s, _, _ := testStore(t)
	call(t, s, "sessions", rewriteRegistration("host", "old", "old"))
	call(t, s, "sessions", rewriteRegistration("host", "current", "current"))
	gitdir, remote := "gitdir:host:/src/iris/.git", remoteAlias(rewriteRemote)
	if holders(t, s, gitdir) != 2 || holders(t, s, remote) != 2 {
		t.Fatal("fixture: the shared aliases must name both histories")
	}
	worktree := func(session, path string) Request {
		r := rewriteRegistration("host", session, "")
		r.Cwd, r.Worktree = path, path
		return r
	}
	first := worktree("wt-one", "/src/iris-wt-one")
	isolated := call(t, s, "sessions", first)["repository_id"].(string)
	if !holds(t, s, isolated, isolatedAlias(isolated)) || !holds(t, s, isolated, "local:host:/src/iris-wt-one") {
		t.Fatal("the ambiguous unknown-root allocation was not isolated with its own path")
	}
	call(t, s, "sessions", first)
	second := call(t, s, "sessions", worktree("wt-two", "/src/iris-wt-two"))["repository_id"].(string)
	if second == isolated || holders(t, s, gitdir) != 2 || holders(t, s, remote) != 2 {
		t.Fatal("an isolated repository became one more candidate for the shared aliases")
	}
	learned := first
	learned.RootCommit = "new-root"
	call(t, s, "sessions", learned)
	if holds(t, s, isolated, isolatedAlias(isolated)) || !holds(t, s, isolated, gitdir) || !holds(t, s, isolated, remote) || !holds(t, s, isolated, "root:new-root") {
		t.Fatal("learning a root did not end the isolation and publish the aliases")
	}
}

// Isolation is for unknown roots only. A known-root request among same-strength candidates is
// allocated as before and publishes its aliases, so duplicate roots stay visibly ambiguous.
// Ambiguity among the remote's candidates isolates as the local aliases' does.
func TestOnlyAnUnknownRootAllocationIsIsolatedAndRemoteAmbiguityCounts(t *testing.T) {
	s, _, _ := testStore(t)
	for _, q := range []string{
		"INSERT INTO Repository(id,case_policy,root_commit,name) VALUES('rp_a','folded','dup','dup'),('rp_b','folded','dup','dup')",
		"INSERT INTO RepositoryAlias VALUES('remote:example.com/team/dup','rp_a'),('remote:example.com/team/dup','rp_b')",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	known := Request{Client: "codex", Host: "host", SessionID: "known", Cwd: "/dup/known", Worktree: "/dup/known", RemoteURL: "https://example.com/team/dup.git", RootCommit: "dup"}
	repo := call(t, s, "sessions", known)["repository_id"].(string)
	if repo == "rp_a" || repo == "rp_b" || holds(t, s, repo, isolatedAlias(repo)) || !holds(t, s, repo, "remote:example.com/team/dup") {
		t.Fatal("a known-root ambiguous allocation was isolated or did not publish its remote")
	}
	unknown := Request{Client: "codex", Host: "host", SessionID: "unknown", Cwd: "/dup/unknown", Worktree: "/dup/unknown", RemoteURL: "https://example.com/team/dup.git"}
	repo = call(t, s, "sessions", unknown)["repository_id"].(string)
	if !holds(t, s, repo, isolatedAlias(repo)) || holds(t, s, repo, "remote:example.com/team/dup") {
		t.Fatal("an unknown-root request among the remote's candidates was not isolated")
	}
}

// The owner's unalias removes exact mappings from one repository. Another repository's mapping of
// the same alias stays, an alias the repository does not hold is reported, and a dry run reports
// the same and changes nothing.
func TestUnaliasRemovesOnlyTheNamedRepositorysMappings(t *testing.T) {
	s, _, _ := testStore(t)
	for _, q := range []string{
		"INSERT INTO Repository(id,case_policy,root_commit,name) VALUES('rp_a','folded','','a'),('rp_b','folded','','b')",
		"INSERT INTO RepositoryAlias VALUES('remote:example.com/team/b','rp_a'),('remote:example.com/team/b','rp_b'),('local:host:/b/sub','rp_a')",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := Request{RepositoryID: "rp_a", Aliases: []string{"remote:example.com/team/b", "local:host:/b/sub", "remote:example.com/team/b", "local:host:/never"}, DryRun: true}
	plan := call(t, s, "unalias", r)
	removed, absent := plan["removed"].([]string), plan["absent"].([]string)
	sort.Strings(removed)
	if len(removed) != 2 || removed[0] != "local:host:/b/sub" || len(absent) != 1 || absent[0] != "local:host:/never" || plan["dry_run"] != true {
		t.Fatalf("plan: %v", plan)
	}
	if !holds(t, s, "rp_a", "remote:example.com/team/b") || !holds(t, s, "rp_a", "local:host:/b/sub") {
		t.Fatal("a dry run removed an alias")
	}
	r.DryRun = false
	if out := call(t, s, "unalias", r); len(out["removed"].([]string)) != 2 {
		t.Fatalf("apply: %v", out)
	}
	if holds(t, s, "rp_a", "remote:example.com/team/b") || holds(t, s, "rp_a", "local:host:/b/sub") || !holds(t, s, "rp_b", "remote:example.com/team/b") {
		t.Fatal("unalias removed the wrong mappings")
	}
	expectError(t, s, "unalias", Request{Aliases: []string{"x"}}, "bad_request")
	expectError(t, s, "unalias", Request{RepositoryID: "rp_a"}, "bad_request")
	expectError(t, s, "unalias", Request{RepositoryID: "rp_missing", Aliases: []string{"x"}}, "not_found")
	_, err := s.CallAuthorized(context.Background(), Principal{Kind: HostPrincipal}, "unalias", r)
	consoleError(t, err, "forbidden")
}
