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

// A child shell is not warned about its parent's claim and still takes its own,
// which is what the grant does. The overlap is not an advisory.
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

// The shell's own automatic claim in another checkout is an advisory, and its
// own null-key claim is a conflict: the grant refuses that one and the warning
// says so. A reusable claim in this checkout stays quiet.
func TestShellWarnsForItsOwnNonReusableClaim(t *testing.T) {
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
	text := messageOf(out)
	claimed, _ := out["claimed"].([]string)
	if !strings.Contains(text, "null.go") || strings.Contains(text, "mine.go") || len(claimed) != 1 || claimed[0] != "mine.go" {
		t.Fatalf("own claims: message %q claimed %v", text, claimed)
	}
}
