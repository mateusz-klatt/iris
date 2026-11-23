package store

import (
	"strings"
	"testing"
)

// A superproject Execution's claim on a submodule file carries that submodule
// checkout. An unrelated Execution rooted there used to get no shell warning:
// the guard's same-checkout skip hid the conflict, while the grant still
// refused it. The shell warning follows the grant. The guard still allows a
// commit in that checkout.
func TestShellWarnsForAnotherExecutionInTheSameCheckout(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := checkoutSession(t, s, "codex", "", "parent", "/src/snapper", "dev:super", superRemote, "snapper-root", "main")
	group := submodule("src/a.ts")
	group.WorktreeID = "dev:fe"
	granted := call(t, s, "touch_files", filesTouch(parent, "into-fe", group))
	if granted["action"] != "allow" || len(granted["granted"].([]map[string]any)) != 1 {
		t.Fatal(granted)
	}
	subRepo := granted["granted"].([]map[string]any)[0]["repository_id"].(string)
	setScope(t, s, subRepo, "worktree")
	child, _ := checkoutSession(t, s, "codex", "", "child", "/src/snapper/frontend", "dev:fe", frontendRemote, "frontend-root", "feature")
	child.Kind, child.Status = "before", map[string]string{}
	call(t, s, "reconcile", child)
	child.Kind, child.Status = "after", map[string]string{"src/a.ts": " M"}
	out := call(t, s, "reconcile", child)
	text := messageOf(out)
	claimed, _ := out["claimed"].([]string)
	if !strings.Contains(text, "Shell edit detected") || !strings.Contains(text, "src/a.ts") || len(claimed) != 0 || out["advisories"] != nil {
		t.Fatalf("shell conflict: message %q claimed %v advisories %v", text, claimed, out["advisories"])
	}
	guard := call(t, s, "check", Request{Host: "host", Cwd: "/src/snapper/frontend", Worktree: "/src/snapper/frontend", WorktreeID: "dev:fe", RemoteURL: frontendRemote, RootCommit: "frontend-root", Paths: []string{"src/a.ts"}})
	if messageOf(guard) != "" || guard["advisories"] != nil {
		t.Fatal("guard refused the same checkout", guard)
	}
}

// Under repository a child shell is warned about its parent's claim in another
// checkout, and still takes the path. v0.1.13 warned; the grant does not refuse
// a relative. The overlap is not an advisory.
func TestRepositoryShellWarnsAChildAboutItsParent(t *testing.T) {
	s, _, _ := testStore(t)
	parent, _ := scopeSession(t, s, "", "parent", "/repo/wt1", "dev:1", "main")
	call(t, s, "touch", touchRequest(parent, "parent", "a.go"))
	childReg := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "child", ParentSessionID: "parent", Cwd: "/repo/wt2", Worktree: "/repo/wt2", WorktreeID: "dev:2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "side", Source: "hook"})
	child := Request{ExecutionID: childReg["execution_id"].(string), Generation: childReg["generation"].(int64)}
	child.Kind, child.Status = "before", map[string]string{}
	call(t, s, "reconcile", child)
	child.Kind, child.Status = "after", map[string]string{"a.go": " M"}
	out := call(t, s, "reconcile", child)
	claimed, _ := out["claimed"].([]string)
	text := messageOf(out)
	if !strings.Contains(text, "Shell edit detected") || !strings.Contains(text, "a.go") || len(claimed) != 1 || claimed[0] != "a.go" || out["advisories"] != nil {
		t.Fatalf("repository child shell: message %q claimed %v advisories %v", text, claimed, out["advisories"])
	}
}

// A child shell is not warned about its parent's claim and still takes its own,
// which is what the grant does once the repository opts into worktree scope.
// The overlap is not an advisory.
func TestShellDoesNotWarnAChildAboutItsParent(t *testing.T) {
	s, _, _ := testStore(t)
	parent, po := scopeSession(t, s, "", "parent", "/repo/wt1", "dev:1", "main")
	setScope(t, s, po["repository_id"].(string), "worktree")
	call(t, s, "touch", touchRequest(parent, "parent", "a.go"))
	childReg := call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "child", ParentSessionID: "parent", Cwd: "/repo/wt2", Worktree: "/repo/wt2", WorktreeID: "dev:2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "side", Source: "hook"})
	child := Request{ExecutionID: childReg["execution_id"].(string), Generation: childReg["generation"].(int64)}
	child.Kind, child.Status = "before", map[string]string{}
	call(t, s, "reconcile", child)
	child.Kind, child.Status = "after", map[string]string{"a.go": " M"}
	out := call(t, s, "reconcile", child)
	claimed, _ := out["claimed"].([]string)
	if messageOf(out) != "" || out["advisories"] != nil || len(claimed) != 1 || claimed[0] != "a.go" {
		t.Fatalf("child shell: message %q claimed %v advisories %v", messageOf(out), claimed, out["advisories"])
	}
}

// The shell adopts its own null-key claim instead of warning about it. A keyed
// claim in another checkout, while this Execution still has no key, is a conflict
// the grant also refuses.
func TestShellAdoptsItsOwnNullKeyClaim(t *testing.T) {
	s, _, _ := testStore(t)
	a, ao := scopeSession(t, s, "", "a", "/repo/wt1", "dev:1", "main")
	repo := ao["repository_id"].(string)
	setScope(t, s, repo, "worktree")
	call(t, s, "touch", touchRequest(a, "mine", "mine.go"))
	unkeyed := ClaimGroup{Worktree: "/repo/wt1", RemoteURL: scopeRemote, RootCommit: "root", Branch: "main", Paths: []string{"null.go"}}
	if got := call(t, s, "touch_files", filesTouch(a, "null", unkeyed)); got["action"] != "allow" {
		t.Fatal(got)
	}
	a.Kind, a.Status = "before", map[string]string{}
	call(t, s, "reconcile", a)
	a.Kind, a.Status = "after", map[string]string{"mine.go": " M", "null.go": " M"}
	out := call(t, s, "reconcile", a)
	rows := claimsOn(liveClaims(t, s, repo), "null.go")
	key := executionKey(t, s, a.ExecutionID)
	claimed, _ := out["claimed"].([]string)
	if messageOf(out) != "" || len(claimed) != 2 || len(rows) != 1 || !rows[0].key.Valid || rows[0].key.String != key.String {
		t.Fatalf("null key was not adopted: message %q claimed %v rows %+v", messageOf(out), claimed, rows)
	}

	bare := scopeSessionOnly(t, s, "bare")
	setScope(t, s, repo, "worktree")
	other := ClaimGroup{Worktree: "/repo/wt2", WorktreeID: "dev:2", RemoteURL: scopeRemote, RootCommit: "root", Branch: "other", Paths: []string{"away.go"}}
	if got := call(t, s, "touch_files", filesTouch(bare, "away", other)); got["action"] != "allow" {
		t.Fatal(got)
	}
	if executionKey(t, s, bare.ExecutionID).Valid {
		t.Fatal("another checkout keyed the execution")
	}
	bare.Kind, bare.Status = "before", map[string]string{}
	call(t, s, "reconcile", bare)
	bare.Kind, bare.Status = "after", map[string]string{"away.go": " M"}
	out = call(t, s, "reconcile", bare)
	claimed, _ = out["claimed"].([]string)
	if !strings.Contains(messageOf(out), "away.go") || len(claimed) != 0 {
		t.Fatalf("other checkout was silent: message %q claimed %v", messageOf(out), claimed)
	}
}
