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

// Migration 27 drops every full shell baseline once: one from before ADR 033 has no recorded
// checkout, so it may have been taken in another checkout than its Execution's.
func TestMigration27DropsUnprovenFullBaselines(t *testing.T) {
	s, clock, file := testStore(t)
	a, _ := session(t, s, 1)
	if _, err := s.db.Exec(`INSERT INTO ShellSnapshot VALUES(?,'{"src/x.ts":" M"}'); PRAGMA user_version=26;`, a.ExecutionID); err != nil {
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
	var version, kept int
	if err = s.db.QueryRow("SELECT (SELECT user_version FROM pragma_user_version),(SELECT COUNT(*) FROM ShellSnapshot)").Scan(&version, &kept); err != nil || version != latestVersion || kept != 0 {
		t.Fatalf("version %d, %d baselines kept, %v", version, kept, err)
	}
}
