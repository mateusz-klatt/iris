package store

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	superRemote    = "https://github.com/example/snapper.git"
	frontendRemote = "https://github.com/example/snapper-frontend.git"
)

// register starts a session in a checkout and returns its proof and repository.
func register(t *testing.T, s *Store, session, worktree, remote, root string) (Request, string) {
	t.Helper()
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: session, Cwd: worktree, Worktree: worktree, CommonDir: worktree + "/.git", RemoteURL: remote, RootCommit: root, Source: "hook"})
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out["repository_id"].(string)
}

// filesTouch is a grouped touch of paths in one checkout.
func filesTouch(r Request, call string, groups ...ClaimGroup) Request {
	r.ToolCallID = call
	r.Groups = groups
	return r
}

// submodule is the frontend submodule of the superproject at /src/snapper.
func submodule(paths ...string) ClaimGroup {
	return ClaimGroup{Worktree: "/src/snapper/frontend", CommonDir: "/src/snapper/.git/modules/frontend", RemoteURL: frontendRemote, RootCommit: "frontend-root", Paths: paths}
}

func grantedIn(out map[string]any) map[string]string {
	got := map[string]string{}
	for _, g := range out["granted"].([]map[string]any) {
		got[g["path"].(string)] = g["repository_id"].(string)
	}
	return got
}

// A file in a submodule is claimed in the submodule's repository, whether the session that edits
// it was started in the superproject or in the submodule's own checkout, so the two meet.
func TestASubmoduleFileIsClaimedInItsOwnRepositoryFromEitherCheckout(t *testing.T) {
	s, _, _ := testStore(t)
	parent, parentRepo := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	if parentRepo == childRepo {
		t.Fatal("the two projects share a repository")
	}
	own := ClaimGroup{Worktree: "/src/snapper", CommonDir: "/src/snapper/.git", RemoteURL: superRemote, RootCommit: "snapper-root", Paths: []string{"README.md"}}
	out := call(t, s, "touch_files", filesTouch(parent, "edit", own, submodule("src/x.ts")))
	if out["action"] != "allow" {
		t.Fatalf("parent touch: %v", out)
	}
	if got := grantedIn(out); got["src/x.ts"] != childRepo || got["README.md"] != parentRepo {
		t.Fatalf("granted %v, want src/x.ts in %s and README.md in %s", got, childRepo, parentRepo)
	}
	refused := call(t, s, "touch", touchRequest(child, "child-edit", "src/x.ts"))
	if refused["action"] != "deny" || !strings.Contains(refused["message"].(string), "src/x.ts is held by") {
		t.Fatalf("child touch of the same file: %v", refused)
	}
	call(t, s, "release", Request{ExecutionID: parent.ExecutionID, Generation: parent.Generation, All: true})
	if out := call(t, s, "touch", touchRequest(child, "child-again", "src/x.ts")); out["action"] != "allow" {
		t.Fatalf("after the parent released: %v", out)
	}
	back := call(t, s, "touch_files", filesTouch(parent, "parent-again", submodule("src/x.ts")))
	if back["action"] != "deny" || !strings.Contains(back["message"].(string), "src/x.ts is held by codex-host-frontend in repository snapper-frontend until") {
		t.Fatalf("parent touch while the child holds it: %v", back)
	}
	if len(back["granted"].([]map[string]any)) != 0 || back["conflicts"].([]map[string]any)[0]["repository_id"] != childRepo {
		t.Fatalf("refused touch: %v", back)
	}
}

// A checkout no repository knows yet gets one, once, and a session started there later meets
// the claim. The session's own repository learns nothing from it.
func TestAnUnknownCheckoutIsAllocatedOnceAndLaterSessionsMeetItsClaims(t *testing.T) {
	s, _, _ := testStore(t)
	parent, parentRepo := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	mcp := ClaimGroup{Worktree: "/src/snapper/integrations/mcp", CommonDir: "/src/snapper/.git/modules/mcp", RemoteURL: "https://github.com/example/snapper-mcp.git", RootCommit: "mcp-root", Paths: []string{"main.go"}}
	first := grantedIn(call(t, s, "touch_files", filesTouch(parent, "one", mcp)))["main.go"]
	mcp.Paths = []string{"go.mod"}
	if second := grantedIn(call(t, s, "touch_files", filesTouch(parent, "two", mcp)))["go.mod"]; first == "" || second != first || first == parentRepo {
		t.Fatalf("allocations %q and %q, parent %q", first, second, parentRepo)
	}
	var aliases int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM RepositoryAlias WHERE repository_id=? AND alias LIKE '%mcp%'", parentRepo).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatalf("the parent learned %d mcp aliases, %v", aliases, err)
	}
	later, laterRepo := register(t, s, "later", "/work/mcp", "https://github.com/example/snapper-mcp.git", "mcp-root")
	if laterRepo != first {
		t.Fatalf("a session in the mcp project joined %s, want %s", laterRepo, first)
	}
	if out := call(t, s, "touch", touchRequest(later, "later-edit", "main.go")); out["action"] != "deny" {
		t.Fatalf("the later session was not refused: %v", out)
	}
}

// A checkout the hub cannot place takes no claim and allocates nothing: the call says so, and the
// rest of it is granted.
func TestAnAmbiguousCheckoutTakesNoClaimAndAllocatesNothing(t *testing.T) {
	s, _, _ := testStore(t)
	call(t, s, "sessions", rewriteRegistration("host", "old", "old"))
	call(t, s, "sessions", rewriteRegistration("host", "current", "current"))
	parent, parentRepo := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	count := func() (n int) {
		if err := s.db.QueryRow("SELECT COUNT(*) FROM Repository").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	nested := ClaimGroup{Worktree: "/src/snapper/vendor/iris", RemoteURL: rewriteRemote, Paths: []string{"main.go"}}
	own := ClaimGroup{Worktree: "/src/snapper", RemoteURL: superRemote, RootCommit: "snapper-root", Paths: []string{"README.md"}}
	out := call(t, s, "touch_files", filesTouch(parent, "mixed", nested, own))
	if count() != before {
		t.Fatalf("%d repositories became %d", before, count())
	}
	if got := grantedIn(out); len(got) != 1 || got["README.md"] != parentRepo {
		t.Fatalf("granted %v", got)
	}
	if out["action"] != "allow" || !strings.Contains(out["message"].(string), "cannot tell which repository /src/snapper/vendor/iris belongs to") {
		t.Fatalf("answer: %v", out)
	}
	if u := out["unclaimed"].([]string); len(u) != 1 || u[0] != "/src/snapper/vendor/iris" {
		t.Fatalf("unclaimed %v", u)
	}
}

// A conflict in any group grants nothing in any group. The action is the strictest policy among
// the repositories that had a conflict; a clean repository's policy does not decide it.
func TestAConflictInOneRepositoryGrantsNothingAndItsPolicyDecides(t *testing.T) {
	s, _, _ := testStore(t)
	parent, parentRepo := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	call(t, s, "touch", touchRequest(child, "child-edit", "src/x.ts"))
	if err := s.SetRepositoryPolicy(t.Context(), childRepo, "folded", "warn"); err != nil {
		t.Fatal(err)
	}
	own := ClaimGroup{Worktree: "/src/snapper", RemoteURL: superRemote, RootCommit: "snapper-root", Paths: []string{"README.md"}}
	out := call(t, s, "touch_files", filesTouch(parent, "both", own, submodule("src/x.ts")))
	if out["action"] != "warn" || len(out["granted"].([]map[string]any)) != 0 {
		t.Fatalf("answer: %v", out)
	}
	if strings.Contains(out["message"].(string), "Ask the human") {
		t.Fatalf("a warning asks for the human: %v", out["message"])
	}
	var held int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE repository_id=? AND revoked_ms IS NULL", parentRepo).Scan(&held); err != nil || held != 0 {
		t.Fatalf("the clean repository took %d claims, %v", held, err)
	}
	if err := s.SetRepositoryPolicy(t.Context(), childRepo, "folded", "ask"); err != nil {
		t.Fatal(err)
	}
	// Codex cannot ask on a pre-tool hook, so ask becomes deny, with the instruction to ask.
	if out := call(t, s, "touch_files", filesTouch(parent, "ask", submodule("src/x.ts"))); out["action"] != "deny" || !strings.Contains(out["message"].(string), "Ask the human") {
		t.Fatalf("ask for codex: %v", out)
	}
}

// A replayed tool call is allowed only while the session still holds what it was granted, in
// the repository it was granted in: a same-named claim elsewhere does not count.
func TestAReplayNeedsTheClaimInItsOwnRepository(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	request := filesTouch(parent, "replayed", submodule("src/x.ts"))
	first := call(t, s, "touch_files", request)
	if replay := call(t, s, "touch_files", request); replay["action"] != "allow" || len(replay["granted"].([]any)) != 1 {
		t.Fatalf("replay: %v (first %v)", replay, first)
	}
	call(t, s, "release", Request{ExecutionID: parent.ExecutionID, Generation: parent.Generation, All: true})
	call(t, s, "touch", touchRequest(parent, "same-name-in-own", "src/x.ts"))
	expectError(t, s, "touch_files", request, "lost")
	refused := filesTouch(parent, "refused", submodule("src/y.ts"))
	child, _ := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	call(t, s, "touch", touchRequest(child, "child-holds", "src/y.ts"))
	call(t, s, "touch_files", refused)
	if again := call(t, s, "touch_files", refused); again["action"] != "deny" {
		t.Fatalf("a refused call replayed as %v", again)
	}
}

// A release frees each repository its claims were in, and wakes the sessions waiting there.
func TestAReleaseWakesWaitersInTheRepositoryOfTheFile(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	call(t, s, "touch_files", filesTouch(parent, "edit", submodule("src/x.ts")))
	call(t, s, "touch", touchRequest(child, "blocked", "src/x.ts"))
	waiting := func() (n int) {
		if err := s.db.QueryRow("SELECT COUNT(*) FROM Waiter WHERE execution_id=? AND repository_id=?", child.ExecutionID, childRepo).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if waiting() != 1 {
		t.Fatal("the refused child is not waiting in its repository")
	}
	call(t, s, "release", Request{ExecutionID: parent.ExecutionID, Generation: parent.Generation, All: true})
	if waiting() != 0 {
		t.Fatal("the release did not wake the child")
	}
	var available int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Event WHERE kind='claim_available' AND object_id=?", "src/x.ts").Scan(&available); err != nil || available != 1 {
		t.Fatalf("%d availability events, %v", available, err)
	}
	// A parent waiting on the child's file waits in the child's repository, and taking the path
	// there clears it.
	call(t, s, "touch", touchRequest(child, "child-holds", "src/z.ts"))
	call(t, s, "touch_files", filesTouch(parent, "parent-blocked", submodule("src/z.ts")))
	var parentWaits string
	if err := s.db.QueryRow("SELECT repository_id FROM Waiter WHERE execution_id=?", parent.ExecutionID).Scan(&parentWaits); err != nil || parentWaits != childRepo {
		t.Fatalf("the parent waits in %q, %v", parentWaits, err)
	}
	call(t, s, "release", Request{ExecutionID: child.ExecutionID, Generation: child.Generation, All: true})
	call(t, s, "touch_files", filesTouch(parent, "parent-takes", submodule("src/z.ts")))
	var left int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Waiter WHERE execution_id=?", parent.ExecutionID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d waits left, %v", left, err)
	}
}

// One path named twice, by two checkouts of one repository, is claimed once, and an ignored
// path is reported with its repository without taking an automatic claim.
func TestGroupsOfOneRepositoryAreDeduplicatedAndIgnoredPathsReported(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	_, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	other := ClaimGroup{Worktree: "/src/frontend", RemoteURL: frontendRemote, RootCommit: "frontend-root", Paths: []string{"SRC/X.ts"}}
	out := call(t, s, "touch_files", filesTouch(parent, "twice", submodule("src/x.ts", "package-lock.json"), other))
	if got := grantedIn(out); len(got) != 1 || got["src/x.ts"] != childRepo {
		t.Fatalf("granted %v", got)
	}
	ignored := out["ignored"].([]map[string]any)
	if len(ignored) != 1 || ignored[0]["path"] != "package-lock.json" || ignored[0]["repository_id"] != childRepo || !strings.Contains(out["message"].(string), "advisory only") {
		t.Fatalf("answer: %v", out)
	}
}

// Another path to the session's own worktree, named by the same worktree id, is its own
// repository, even where a remote would place it elsewhere.
func TestTheSessionsOwnWorktreeIsItsRepositoryByAnyPath(t *testing.T) {
	s, _, _ := testStore(t)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "own", Cwd: "/src/snapper", Worktree: "/src/snapper", WorktreeID: "dev:1", RemoteURL: superRemote, RootCommit: "snapper-root", Source: "hook"})
	own := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	alias := ClaimGroup{Worktree: "/mnt/snapper", WorktreeID: "dev:1", RemoteURL: superRemote, RootCommit: "snapper-root", Paths: []string{"main.go"}}
	if got := grantedIn(call(t, s, "touch_files", filesTouch(own, "alias", alias))); got["main.go"] != out["repository_id"] {
		t.Fatalf("granted %v", got)
	}
}

// A grouped touch is refused when it names no tool call, no group, a group without its worktree,
// no path at all, or a path a claim cannot hold.
func TestAGroupedTouchIsRefusedWhenItCannotBePlaced(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	expectError(t, s, "touch_files", filesTouch(parent, "", submodule("src/x.ts")), "bad_request")
	expectError(t, s, "touch_files", filesTouch(parent, "none"), "bad_request")
	expectError(t, s, "touch_files", filesTouch(parent, "no-worktree", ClaimGroup{Paths: []string{"x"}}), "bad_request")
	expectError(t, s, "touch_files", filesTouch(parent, "no-paths", submodule()), "bad_request")
	expectError(t, s, "touch_files", filesTouch(parent, "control", submodule("a\x07b")), "bad_request")
	expectError(t, s, "touch_files", filesTouch(parent, "escape", submodule("../outside")), "bad_request")
	expectError(t, s, "touch_files", filesTouch(Request{ExecutionID: "ex_none", Generation: 1}, "lost", submodule("src/x.ts")), "lost")
}

// A shell status from another checkout, such as a submodule the session's working directory
// moved into, neither replaces the baseline nor takes or releases a claim.
func TestAShellStatusFromAnotherCheckoutIsNotAttributed(t *testing.T) {
	s, _, _ := testStore(t)
	parent, parentRepo := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	snap := func(kind, worktree, id string, status map[string]string) map[string]any {
		r := parent
		r.Kind, r.Status, r.Worktree, r.WorktreeID, r.RemoteURL, r.RootCommit = kind, status, worktree, id, superRemote, "snapper-root"
		return call(t, s, "reconcile", r)
	}
	snap("before", "/src/snapper", "", map[string]string{"main.go": " M"})
	call(t, s, "touch", touchRequest(parent, "edit", "main.go"))
	if out := snap("after", "/src/snapper/frontend", "", map[string]string{"src/x.ts": " M"}); out["claimed"] != nil || out["message"] != "" {
		t.Fatalf("a submodule status was attributed: %v", out)
	}
	var held []string
	rows, err := s.db.Query("SELECT path FROM Claim WHERE repository_id=? AND revoked_ms IS NULL ORDER BY path", parentRepo)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		held = append(held, p)
	}
	_ = rows.Close()
	if len(held) != 1 || held[0] != "main.go" {
		t.Fatalf("the parent holds %v after the submodule status", held)
	}
	// The baseline is still the superproject's, so the next status of the superproject that no
	// longer lists main.go releases its claim.
	snap("after", "/src/snapper", "", map[string]string{})
	var live int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE repository_id=? AND revoked_ms IS NULL", parentRepo).Scan(&live); err != nil || live != 0 {
		t.Fatalf("%d claims left, %v", live, err)
	}
}

// The worktree id folds another path to the session's directory into its worktree for a shell
// status too, but only for the repository the host first named under that id.
func TestAShellStatusFromAnotherPathToTheWorktreeIsAttributed(t *testing.T) {
	s, _, _ := testStore(t)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "own", Cwd: "/src/snapper", Worktree: "/src/snapper", WorktreeID: "dev:1", RemoteURL: superRemote, RootCommit: "snapper-root", Source: "hook"})
	own := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	snap := func(kind, root string, status map[string]string) map[string]any {
		r := own
		r.Kind, r.Status, r.Worktree, r.WorktreeID, r.RemoteURL, r.RootCommit = kind, status, "/mnt/snapper", "dev:1", superRemote, root
		return call(t, s, "reconcile", r)
	}
	snap("before", "snapper-root", map[string]string{})
	if got := snap("after", "snapper-root", map[string]string{"main.go": " M"})["claimed"].([]string); len(got) != 1 {
		t.Fatalf("another path to the worktree claimed %v", got)
	}
	if got := snap("after", "another-root", map[string]string{"other.go": " M"}); got["claimed"] != nil {
		t.Fatalf("another repository under the same id was attributed: %v", got)
	}
}

// A repository whose files another repository's session holds is still in use.
func TestForgettingARepositoryCountsClaimsHeldOnItsFiles(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	call(t, s, "end", child)
	call(t, s, "touch_files", filesTouch(parent, "edit", submodule("src/x.ts")))
	expectError(t, s, "forget", Request{RepositoryID: childRepo}, "conflict")
}

// A path the session already holds is reused: an automatic claim is renewed, an explicit one
// kept as it is, and neither counts again toward the claim limit. A subagent of the holder is
// not refused.
func TestAGroupedTouchReusesTheSessionsClaimsAndSparesItsFamily(t *testing.T) {
	s, clock, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	call(t, s, "touch_files", filesTouch(parent, "first", submodule("src/x.ts")))
	expiry := func(path string) (ms int64) {
		if err := s.db.QueryRow("SELECT expires_ms FROM Claim WHERE path=? AND revoked_ms IS NULL", path).Scan(&ms); err != nil {
			t.Fatal(err)
		}
		return ms
	}
	before := expiry("src/x.ts")
	clock.advance(time.Minute)
	call(t, s, "touch_files", filesTouch(parent, "second", submodule("src/x.ts")))
	if after := expiry("src/x.ts"); after <= before {
		t.Fatalf("the automatic claim was not renewed: %d then %d", before, after)
	}
	var live int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM Claim WHERE execution_id=? AND revoked_ms IS NULL", parent.ExecutionID).Scan(&live); err != nil || live != 1 {
		t.Fatalf("%d live claims, %v", live, err)
	}
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	call(t, s, "claim", claimRequest(child, "planned", "docs", "prefix"))
	if out := call(t, s, "touch_files", filesTouch(child, "own-explicit", ClaimGroup{Worktree: "/src/frontend", RemoteURL: frontendRemote, RootCommit: "frontend-root", Paths: []string{"docs/a.md"}})); out["action"] != "allow" || grantedIn(out)["docs/a.md"] != childRepo {
		t.Fatalf("a path under the session's explicit claim: %v", out)
	}
	sub, _ := register(t, s, "sub", "/src/snapper", superRemote, "snapper-root")
	if _, err := s.db.Exec("UPDATE Execution SET parent_id=? WHERE id=?", parent.ExecutionID, sub.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if out := call(t, s, "touch_files", filesTouch(sub, "family", submodule("src/x.ts"))); out["action"] != "allow" {
		t.Fatalf("a subagent of the holder: %v", out)
	}
}

// With conflicts in two repositories, the stricter policy decides, whichever comes first.
func TestTheStricterOfTwoConflictingPoliciesDecides(t *testing.T) {
	s, _, _ := testStore(t)
	parent, parentRepo := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	other, _ := register(t, s, "other", "/src/snapper", superRemote, "snapper-root")
	call(t, s, "touch", touchRequest(child, "child-holds", "src/x.ts"))
	call(t, s, "touch", touchRequest(other, "other-holds", "README.md"))
	if err := s.SetRepositoryPolicy(t.Context(), childRepo, "folded", "warn"); err != nil {
		t.Fatal(err)
	}
	own := ClaimGroup{Worktree: "/src/snapper", RemoteURL: superRemote, RootCommit: "snapper-root", Paths: []string{"README.md"}}
	if out := call(t, s, "touch_files", filesTouch(parent, "deny-first", own, submodule("src/x.ts"))); out["action"] != "deny" || len(out["conflicts"].([]map[string]any)) != 2 {
		t.Fatalf("deny then warn: %v", out)
	}
	if err := s.SetRepositoryPolicy(t.Context(), parentRepo, "folded", "warn"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepositoryPolicy(t.Context(), childRepo, "folded", "deny"); err != nil {
		t.Fatal(err)
	}
	if out := call(t, s, "touch_files", filesTouch(parent, "warn-first", own, submodule("src/x.ts"))); out["action"] != "deny" {
		t.Fatalf("warn then deny: %v", out)
	}
}

// A plain touch's replay counts only a claim in the session's own repository: a claim of the same
// name the session holds in another repository is another file, and must not let a replay through
// a conflict that arose in between.
func TestAPlainReplayIgnoresTheSameNameInAnotherRepository(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	plain := touchRequest(parent, "plain", "src/x.ts")
	call(t, s, "touch", plain)
	call(t, s, "release", Request{ExecutionID: parent.ExecutionID, Generation: parent.Generation, All: true})
	call(t, s, "touch_files", filesTouch(parent, "grouped", submodule("src/x.ts")))
	other, _ := register(t, s, "other", "/src/snapper", superRemote, "snapper-root")
	call(t, s, "touch", touchRequest(other, "other", "src/x.ts"))
	expectError(t, s, "touch", plain, "lost")
}

// The session's own checkout keeps the history check of a registration from there: a group of its
// worktree with another known root is refused, not claimed in the old history's repository.
func TestTheSessionsOwnWorktreeMustStillBeItsHistory(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	rewritten := ClaimGroup{Worktree: "/src/snapper", RemoteURL: superRemote, RootCommit: "rewritten-root", Paths: []string{"README.md"}}
	expectError(t, s, "touch_files", filesTouch(parent, "rewritten", rewritten), "conflict")
}

// A worktree is the session's own however its separators, a trailing slash or, with a drive
// letter, its case are spelled; a POSIX path differing in case is another directory.
func TestWorktreeSpellingsOfOneDirectoryAreTheSame(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{`C:\Repo`, "c:/repo/", true},
		{"/src/app", "/src/app/", true},
		{"/src/App", "/src/app", false},
		{`C:\Repo`, `D:\Repo`, false},
		{`/src/a\b`, "/src/a/b", false},
	} {
		if got := sameWorktree(c.a, c.b); got != c.same {
			t.Errorf("sameWorktree(%q, %q) = %v", c.a, c.b, got)
		}
	}
	s, _, _ := testStore(t)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "win", Cwd: `C:\Repo`, Worktree: `C:\Repo`, RemoteURL: superRemote, RootCommit: "snapper-root", Source: "hook"})
	win := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	snap := func(kind string, status map[string]string) map[string]any {
		r := win
		r.Kind, r.Status, r.Worktree = kind, status, "c:/repo"
		return call(t, s, "reconcile", r)
	}
	snap("before", map[string]string{})
	if got := snap("after", map[string]string{"main.go": " M"})["claimed"]; !reflect.DeepEqual(got, []string{"main.go"}) {
		t.Fatalf("another spelling of the worktree claimed %v", got)
	}
}

// A status from another checkout still clears the baselines of a worktree that another live
// Execution now shares, so a later status cannot attribute that Execution's changes.
func TestAForeignStatusStillClearsASharedWorktreesBaselines(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := register(t, s, "a", "/src/snapper", superRemote, "snapper-root")
	before := a
	before.Kind, before.Status, before.Worktree = "before", map[string]string{}, "/src/snapper"
	call(t, s, "reconcile", before)
	register(t, s, "b", "/src/snapper", superRemote, "snapper-root")
	foreign := a
	foreign.Kind, foreign.Status, foreign.Worktree = "after", map[string]string{"src/x.ts": " M"}, "/src/snapper/frontend"
	call(t, s, "reconcile", foreign)
	var kept int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ShellSnapshot WHERE execution_id=?", a.ExecutionID).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("%d baselines kept, %v", kept, err)
	}
}

// A replay of a grouped touch survives the owner's merge of the file's repository, because its
// grants are checked by fence, which the merge keeps.
func TestAGroupedReplaySurvivesAMergeOfItsRepository(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	request := filesTouch(parent, "merged", submodule("src/x.ts"))
	from := grantedIn(call(t, s, "touch_files", request))["src/x.ts"]
	_, into := register(t, s, "frontend", "/work/frontend", "https://github.com/example/frontend-copy.git", "")
	call(t, s, "merge", mergeRequest(from, into, false))
	if replay := call(t, s, "touch_files", request); replay["action"] != "allow" {
		t.Fatalf("replay after the merge: %v", replay)
	}
}

// No full shell baseline crosses a hub start: one an older hub wrote names no checkout, so it may
// have been taken in another checkout than its Execution's (ADR 033).
func TestAFullBaselineDoesNotCrossAHubStart(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := session(t, s, 1)
	if _, err := s.db.Exec(`INSERT INTO ShellSnapshot VALUES(?,'{"src/x.ts":" M"}')`, a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var kept int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM ShellSnapshot").Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("%d baselines kept, %v", kept, err)
	}
}

// The session's own worktree teaches its repository what a registration from elsewhere could not:
// a remote it learned since, here, so a later clone with that remote joins it.
func TestTheSessionsOwnWorktreeTeachesItsRepository(t *testing.T) {
	s, _, _ := testStore(t)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "local", Cwd: "/src/app", Worktree: "/src/app", RootCommit: "app-root", Source: "hook"})
	own := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	group := ClaimGroup{Worktree: "/src/app", RemoteURL: "https://github.com/example/app.git", RootCommit: "app-root", Paths: []string{"main.go"}}
	call(t, s, "touch_files", filesTouch(own, "learn", group))
	if _, repo := register(t, s, "clone", "/elsewhere/app", "https://github.com/example/app.git", "app-root"); repo != out["repository_id"] {
		t.Fatalf("the clone joined %s, want %s", repo, out["repository_id"])
	}
}

// A grouped replay survives an owner's merge of the file's repository followed by a restart: the
// grant's fence row names the repository the merge moved it to, and the recovered claim there,
// under its new fence, is the same grant.
func TestAGroupedReplaySurvivesAMergeAndARestart(t *testing.T) {
	s, clock, file := testStore(t)
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	request := filesTouch(parent, "merged-restarted", submodule("src/x.ts"))
	from := grantedIn(call(t, s, "touch_files", request))["src/x.ts"]
	_, into := register(t, s, "frontend", "/work/frontend", "https://github.com/example/frontend-copy.git", "")
	call(t, s, "merge", mergeRequest(from, into, false))
	reopened := restart(t, s, file, clock)
	call(t, reopened, "sessions", Request{Client: "codex", Host: "host", SessionID: "parent", Cwd: "/src/snapper", Worktree: "/src/snapper", Generation: parent.Generation})
	if replay := call(t, reopened, "touch_files", request); replay["action"] != "allow" {
		t.Fatalf("replay after the merge and the restart: %v", replay)
	}
}

// A grouped claim stores the checkout the host first named, in display form. Windows
// separators and the drive letter are canonical, and a later spelling of the same
// directory, including a different case, does not replace that path.
func TestAGroupedClaimStoresTheCanonicalWorktreePath(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := checkoutSession(t, s, "codex", "winhost", "win", `c:\Repo\Feature\`, "dev:win", scopeRemote, "root", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	group := func(id, worktree, wtID, path string) {
		t.Helper()
		out := call(t, s, "touch_files", filesTouch(a, id, ClaimGroup{Worktree: worktree, WorktreeID: wtID, RemoteURL: scopeRemote, RootCommit: "root", Branch: "main", Paths: []string{path}}))
		if out["action"] != "allow" || len(out["granted"].([]map[string]any)) != 1 {
			t.Fatal(id, out)
		}
	}
	group("own", `c:\Repo\Feature\`, "dev:win", "a.go")
	group("other", `d:\Other\Tree\`, "dev:other", "b.go")
	// Same directory as dev:other, written with the other case and separators.
	group("other-again", `D:/OTHER/tree/`, "dev:other", "c.go")
	group("own-again", `C:/repo/feature`, "dev:win", "d.go")
	want := map[string]string{"a.go": "C:/Repo/Feature", "b.go": "D:/Other/Tree", "c.go": "D:/Other/Tree", "d.go": "C:/Repo/Feature"}
	for _, row := range liveClaims(t, s, repo) {
		if got := want[row.path]; got == "" || row.wpath.String != got {
			t.Fatalf("path %s stored worktree %q, want %q", row.path, row.wpath.String, got)
		}
		delete(want, row.path)
	}
	if len(want) != 0 {
		t.Fatal("missing claims", want)
	}
	for _, c := range call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any) {
		path, _ := c["path"].(string)
		if path == "c.go" && c["worktree_path"] != "D:/Other/Tree" {
			t.Fatalf("claims list shows %v", c["worktree_path"])
		}
		if path == "d.go" && c["worktree_path"] != "C:/Repo/Feature" {
			t.Fatalf("claims list shows %v", c["worktree_path"])
		}
	}
}

// An explicit claim on a submodule's path, sent with that checkout, is taken in the submodule's
// repository, where the submodule's own sessions meet it. An ambiguous checkout is refused, and the
// grouped route needs its checkout.
func TestAnExplicitClaimInASubmoduleLiesInItsRepository(t *testing.T) {
	s, _, _ := testStore(t)
	call(t, s, "sessions", rewriteRegistration("host", "old", "old"))
	call(t, s, "sessions", rewriteRegistration("host", "current", "current"))
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	child, childRepo := register(t, s, "child", "/src/frontend", frontendRemote, "frontend-root")
	sub := submodule()
	sub.Prefix = "frontend"
	r := claimRequest(parent, "planned", "frontend/src", "prefix")
	r.Checkout = &sub
	out := call(t, s, "claim_files", r)
	var repo string
	if err := s.db.QueryRow("SELECT repository_id FROM Claim WHERE fence=?", out["fence"]).Scan(&repo); err != nil || repo != childRepo {
		t.Fatalf("claimed in %q, want %q, %v", repo, childRepo, err)
	}
	if again := call(t, s, "claim_files", r); number(again["fence"]) != number(out["fence"]) {
		t.Fatalf("a replay answered %v, first %v", again, out)
	}
	if refused := call(t, s, "touch", touchRequest(child, "child-edit", "src/x.ts")); refused["action"] != "deny" {
		t.Fatalf("the child's edit under the parent's explicit claim: %v", refused)
	}
	expectError(t, s, "claim_files", claimRequest(child, "child-claims", "src/x.ts", "exact"), "bad_request")
	other, _ := register(t, s, "other", "/src/snapper", superRemote, "snapper-root")
	held := claimRequest(other, "other-blocked", "frontend/src/x.ts", "exact")
	held.Checkout = &sub
	expectError(t, s, "claim_files", held, "conflict")
	nested := ClaimGroup{Worktree: "/src/snapper/vendor/iris", RemoteURL: rewriteRemote, Prefix: "vendor/iris"}
	ambiguous := claimRequest(parent, "ambiguous", "vendor/iris/main.go", "exact")
	ambiguous.Checkout = &nested
	expectError(t, s, "claim_files", ambiguous, "ambiguous")
	// The path is validated as given, and must lie below the checkout's place.
	for _, path := range []string{"frontend/../x.ts", "docs/a.md", "frontend"} {
		bad := claimRequest(parent, "bad-"+path, path, "exact")
		bad.Checkout = &sub
		expectError(t, s, "claim_files", bad, "bad_request")
	}
	unplaced := sub
	unplaced.Prefix = ""
	bare := claimRequest(parent, "no-prefix", "frontend/src/y.ts", "exact")
	bare.Checkout = &unplaced
	expectError(t, s, "claim_files", bare, "bad_request")
}

// On a Windows checkout, an explicit claim's names lose the trailing dots and spaces Windows strips,
// as the hook's paths do, so the two meet; a POSIX checkout keeps such names as they are.
func TestAnExplicitClaimInAWindowsCheckoutFoldsWin32Names(t *testing.T) {
	s, _, _ := testStore(t)
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "win", Cwd: `C:\snapper`, Worktree: `C:\snapper`, RemoteURL: superRemote, RootCommit: "snapper-root", Source: "hook"})
	win := Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}
	sub := ClaimGroup{Worktree: `C:\snapper\frontend`, RemoteURL: frontendRemote, RootCommit: "frontend-root", Prefix: "frontend"}
	for path, want := range map[string]string{"frontend./src./x.ts": "src/x.ts", "frontend/src /y.ts": "src/y.ts"} {
		r := claimRequest(win, "win-"+path, path, "exact")
		r.Checkout = &sub
		fence := call(t, s, "claim_files", r)["fence"]
		var stored string
		if err := s.db.QueryRow("SELECT path FROM Claim WHERE fence=?", fence).Scan(&stored); err != nil || stored != want {
			t.Fatalf("%q stored as %q, want %q, %v", path, stored, want, err)
		}
	}
	parent, _ := register(t, s, "parent", "/src/snapper", superRemote, "snapper-root")
	posix := submodule()
	posix.Prefix = "frontend"
	r := claimRequest(parent, "posix", "frontend/src./z.ts", "exact")
	r.Checkout = &posix
	fence := call(t, s, "claim_files", r)["fence"]
	var stored string
	if err := s.db.QueryRow("SELECT path FROM Claim WHERE fence=?", fence).Scan(&stored); err != nil || stored != "src./z.ts" {
		t.Fatalf("a POSIX name was folded to %q, %v", stored, err)
	}
	if got := win32Path("a/.../b."); got != "a/.../b" {
		t.Fatalf("win32Path: %q", got)
	}
}
