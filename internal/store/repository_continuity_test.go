package store

import (
	"context"
	"fmt"
	"testing"
)

// Two known histories leave the path ambiguous. The first unknown registration
// must get its own identity; later registrations can remember that explicit choice.
func unknownCheckout(t *testing.T, s *Store, remote string) (Request, map[string]any) {
	t.Helper()
	r := rewriteRegistration("continuity-host", "history-old", "old")
	r.RemoteURL = remote
	old := call(t, s, "sessions", r)
	r.SessionID, r.RootCommit = "history-current", "current"
	current := call(t, s, "sessions", r)
	r.SessionID, r.RootCommit = "unknown-first", ""
	unknown := call(t, s, "sessions", r)
	if unknown["repository_id"] == old["repository_id"] || unknown["repository_id"] == current["repository_id"] {
		t.Fatal("an unknown checkout chose an ambiguous historical identity")
	}
	return r, unknown
}

func TestRepositoryCheckoutContinuityScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote string
		change func(*Request)
		same   bool
	}{
		{"client", rewriteRemote, func(r *Request) { r.Client = "claude" }, true},
		{"remote spelling", rewriteRemote, func(r *Request) { r.RemoteURL = "git@github.com:mateusz-klatt/iris.git" }, true},
		{"path separators", rewriteRemote, func(r *Request) {
			r.Worktree, r.Cwd, r.CommonDir = `\src\iris`, `\src\iris`, `\src\iris\.git`
		}, true},
		{"host", rewriteRemote, func(r *Request) { r.Host = "another-host" }, false},
		{"worktree", rewriteRemote, func(r *Request) { r.Worktree, r.Cwd = "/another/iris", "/another/iris" }, false},
		{"git directory", rewriteRemote, func(r *Request) { r.CommonDir = "/another/iris.git" }, false},
		{"fork", rewriteRemote, func(r *Request) { r.RemoteURL = "https://github.com/another-owner/iris.git" }, false},
		{"relative remote", "../first.git", func(r *Request) { r.RemoteURL = "../second.git" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := testStore(t)
			r, first := unknownCheckout(t, s, tc.remote)
			tc.change(&r)
			r.SessionID = "changed"
			got := call(t, s, "sessions", r)
			if same := got["repository_id"] == first["repository_id"]; same != tc.same {
				t.Fatalf("same checkout identity = %v, want %v: %v -> %v", same, tc.same, first["repository_id"], got["repository_id"])
			}
			r.SessionID = "changed-again"
			if again := call(t, s, "sessions", r); again["repository_id"] != got["repository_id"] {
				t.Fatalf("a repeated checkout split again: %v -> %v", got["repository_id"], again["repository_id"])
			}
		})
	}
}

func TestConcurrentUnknownCheckoutSessionsShareOneNewRepository(t *testing.T) {
	s, _, _ := testStore(t)
	r := rewriteRegistration("concurrent", "old", "old")
	call(t, s, "sessions", r)
	r.SessionID, r.RootCommit = "current", "current"
	call(t, s, "sessions", r)
	r.RootCommit = ""
	start := make(chan struct{})
	type registrationResult struct {
		out map[string]any
		err error
	}
	results := make(chan registrationResult, 10)
	for i := range cap(results) {
		request := r
		request.SessionID = fmt.Sprintf("unknown-%d", i)
		go func() {
			<-start
			out, err := s.Call(context.Background(), "sessions", request)
			results <- registrationResult{out, err}
		}()
	}
	close(start)
	repositories, mailboxes := map[any]bool{}, map[any]bool{}
	for range cap(results) {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		repositories[result.out["repository_id"]] = true
		mailboxes[result.out["agent"].(map[string]any)["id"]] = true
	}
	if len(repositories) != 1 || len(mailboxes) != cap(results) || count(t, s, "SELECT COUNT(*) FROM Repository") != 3 {
		t.Fatalf("concurrent unknown sessions split identity or shared a mailbox: repositories=%v, mailboxes=%v", repositories, mailboxes)
	}
}

func TestRepositoryKnownRootInvalidatesDifferentCheckoutContinuity(t *testing.T) {
	s, _, _ := testStore(t)
	r, unknown := unknownCheckout(t, s, rewriteRemote)
	r.SessionID, r.RootCommit = "known-again", "current"
	known := call(t, s, "sessions", r)
	if known["repository_id"] == unknown["repository_id"] {
		t.Fatal("unknown checkout continuity displaced a known history")
	}
	r.SessionID, r.RootCommit = "unknown-after-known", ""
	fresh := call(t, s, "sessions", r)
	if fresh["repository_id"] == unknown["repository_id"] || fresh["repository_id"] == known["repository_id"] {
		t.Fatal("a changed known identity left stale continuity or guessed a history")
	}
	r.SessionID = "unknown-after-known-again"
	if again := call(t, s, "sessions", r); again["repository_id"] != fresh["repository_id"] {
		t.Fatal("the new unknown checkout did not retain its own identity")
	}
}

func TestRepositoryKnownRootLearnsExistingCheckoutContinuity(t *testing.T) {
	s, _, _ := testStore(t)
	r, first := unknownCheckout(t, s, rewriteRemote)
	// The running unknown session learns its own root without changing identity.
	r.RootCommit = "learned"
	if learned := call(t, s, "sessions", r); learned["repository_id"] != first["repository_id"] {
		t.Fatal("learning a root moved the existing Execution")
	}
	r.SessionID = "known-after-learning"
	if known := call(t, s, "sessions", r); known["repository_id"] != first["repository_id"] {
		t.Fatal("the learned root was not available to a later session")
	}
	r.SessionID, r.RootCommit = "unknown-after-learning", ""
	if unknown := call(t, s, "sessions", r); unknown["repository_id"] != first["repository_id"] {
		t.Fatal("confirming the same known root discarded checkout continuity")
	}
}

func TestRepositoryKnownDuplicateRootsIgnoreCheckoutContinuity(t *testing.T) {
	s, _, _ := testStore(t)
	r, first := unknownCheckout(t, s, rewriteRemote)
	// A refresh can reveal that the isolated checkout shares another row's root.
	// That does not authorize a later known-root registration to choose either one.
	r.RootCommit = "current"
	call(t, s, "sessions", r)
	r.SessionID = "known-with-duplicate-root"
	fresh := call(t, s, "sessions", r)
	if fresh["repository_id"] == first["repository_id"] || count(t, s, "SELECT COUNT(*) FROM Repository WHERE root_commit='current'") != 3 {
		t.Fatal("checkout continuity overrode ambiguous known-root identities")
	}
	r.SessionID, r.RootCommit = "unknown-after-duplicate-root", ""
	unknown := call(t, s, "sessions", r)
	if unknown["repository_id"] == first["repository_id"] || unknown["repository_id"] == fresh["repository_id"] {
		t.Fatal("ambiguous known-root resolution left stale checkout continuity")
	}
	r.SessionID = "unknown-after-duplicate-root-again"
	if again := call(t, s, "sessions", r); again["repository_id"] != unknown["repository_id"] {
		t.Fatal("isolating ambiguous known roots did not preserve later unknown continuity")
	}
}

func TestRepositoryOldActiveRefreshCannotRepointCheckoutContinuity(t *testing.T) {
	s, _, _ := testStore(t)
	r, first := unknownCheckout(t, s, rewriteRemote)
	old := r
	old.SessionID, old.RootCommit = "history-old", "old"
	call(t, s, "sessions", old)
	r.SessionID = "unknown-after-old-refresh"
	if got := call(t, s, "sessions", r); got["repository_id"] != first["repository_id"] {
		t.Fatal("an old active Execution replaced the newer checkout's continuity")
	}
}

func TestRepositoryDuplicateCheckoutContinuityDoesNotChooseByOrder(t *testing.T) {
	s, _, _ := testStore(t)
	r, first := unknownCheckout(t, s, rewriteRemote)
	if _, err := s.db.Exec("INSERT INTO Repository(id,root_commit) VALUES('rp_duplicate_checkout','')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,'rp_duplicate_checkout')", checkoutRepositoryAlias(r)); err != nil {
		t.Fatal(err)
	}
	r.SessionID = "duplicate-continuity"
	fresh := call(t, s, "sessions", r)
	if fresh["repository_id"] == first["repository_id"] || fresh["repository_id"] == "rp_duplicate_checkout" {
		t.Fatal("ambiguous checkout continuity selected an existing row by order")
	}
	r.SessionID = "after-duplicate-continuity"
	if got := call(t, s, "sessions", r); got["repository_id"] != fresh["repository_id"] {
		t.Fatal("isolating duplicate checkout continuity did not stop repeated splits")
	}
}

func TestRepositoryCheckoutContinuityFollowsOperatorRepair(t *testing.T) {
	s, _, _ := testStore(t)
	r, first := unknownCheckout(t, s, rewriteRemote)
	destination := call(t, s, "sessions", rewriteRegistration("destination-host", "destination", "current"))
	from, into := first["repository_id"].(string), destination["repository_id"].(string)
	call(t, s, "merge", mergeRequest(from, into, false))
	r.SessionID = "unknown-after-merge"
	if got := call(t, s, "sessions", r); got["repository_id"] != into {
		t.Fatal("operator repair lost the checkout's explicit continuity")
	}

	// Removing a separate, ended repository removes its checkout continuity too.
	_, gone := ended(t, s, "continuity-forget")
	call(t, s, "forget", Request{RepositoryID: gone["repository_id"].(string)})
	if count(t, s, "SELECT COUNT(*) FROM RepositoryAlias WHERE repository_id=?", gone["repository_id"]) != 0 {
		t.Fatal("forget retained checkout continuity for the removed repository")
	}
}
