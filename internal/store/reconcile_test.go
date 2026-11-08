package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestShellReconciliation(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	shell.Kind = "before"
	shell.Status = map[string]string{"old": " M"}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"old": " M", "src/new name": "??"}
	if out := call(t, s, "reconcile", shell); out["message"] == "" {
		t.Fatal("missing shell conflict", out)
	}
	if out := call(t, s, "reconcile", shell); out["message"] != "" {
		t.Fatal("unchanged status warned", out)
	}
	// A second Execution in this same worktree makes attribution impossible.
	call(t, s, "sessions", Request{Client: "claude", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "shared"})
	shell.Status = map[string]string{"src/other": "??"}
	if out := call(t, s, "reconcile", shell); out["message"] != "" {
		t.Fatal("shared worktree warned", out)
	}
}
func TestShellFirstPostOnlyEstablishesBaseline(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "src", "prefix"))
	shell.Kind = "after"
	shell.Status = map[string]string{"src/new": "??"}
	if out := call(t, s, "reconcile", shell); out["message"] != "" {
		t.Fatal("unattributed initial state warned", out)
	}
}

// A write found by reconciliation is claimed for the session, as an edit tool's would be (gap C7).
// A path someone else holds stays theirs and is reported, a path that left the status is not
// claimed, and the session's own automatic claim is renewed rather than duplicated.
func TestReconciliationClaimsWhatTheSessionWrote(t *testing.T) {
	s, clock, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, joined := session(t, s, 2)
	repo := Request{RepositoryID: joined["repository_id"].(string)}
	call(t, s, "claim", claimRequest(holder, "held", "theirs.txt", "exact"))
	shell.Kind = "before"
	shell.Status = map[string]string{"committed.txt": " M", "node_modules/x.js": "??"}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"mine.txt": "??", "theirs.txt": " M", "node_modules/x.js": " M"}
	out := call(t, s, "reconcile", shell)
	claimed, _ := out["claimed"].([]string)
	if len(claimed) != 1 || claimed[0] != "mine.txt" || out["message"] == "" {
		t.Fatalf("claimed %v, message %q", out["claimed"], out["message"])
	}
	// Another session is now refused mine.txt, which nobody had claimed before.
	other, _ := session(t, s, 3)
	if got := call(t, s, "touch", touchRequest(other, "t", "mine.txt")); got["action"] != "deny" {
		t.Fatalf("mine.txt not protected: %v", got)
	}
	// A later write to the same path renews the automatic claim instead of adding one.
	expiry := func() (string, int) {
		t.Helper()
		var at string
		n := 0
		for _, c := range call(t, s, "claims", repo)["claims"].([]map[string]any) {
			if c["path"] == "mine.txt" {
				at, n = c["expires_at"].(string), n+1
			}
		}
		return at, n
	}
	first, _ := expiry()
	clock.advance(30 * time.Second)
	for _, r := range []Request{holder, shell, other} {
		r.Source = "shim"
		call(t, s, "heartbeat", r)
	}
	shell.Kind = "before"
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{"mine.txt": " M", "theirs.txt": " M", "node_modules/x.js": " M"}
	out = call(t, s, "reconcile", shell)
	if c, _ := out["claimed"].([]string); len(c) != 1 || c[0] != "mine.txt" {
		t.Fatalf("renewal: %v", out["claimed"])
	}
	if second, n := expiry(); n != 1 || second <= first {
		t.Fatalf("%d claims on mine.txt, expiry %s then %s", n, first, second)
	}
}

func TestReconciliationRespectsTheClaimLimit(t *testing.T) {
	s, _, _ := testStore(t)
	shell, _ := session(t, s, 1)
	shell.Kind = "before"
	shell.Status = map[string]string{}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{}
	for i := range 501 {
		shell.Status[fmt.Sprintf("f%03d", i)] = "??"
	}
	// One command can change more files than the limit: claim up to it, keep the reconcile
	// answer, and say what was left, rather than fail and lose the conflict warning.
	out := call(t, s, "reconcile", shell)
	claimed, _ := out["claimed"].([]string)
	if len(claimed) != 500 || !strings.Contains(out["message"].(string), "left 1 unclaimed") {
		t.Fatalf("%d claimed, message %q", len(claimed), out["message"])
	}
}

// A path that left the status was committed, stashed or restored by git in this worktree only.
// Nothing changed in the holder's worktree, so it is no shell edit to warn about; only paths
// still changed after the command are checked.
func TestReconciliationWarnsOnlyAboutPathsStillChanged(t *testing.T) {
	s, _, _ := testStore(t)
	holder, _ := session(t, s, 1)
	shell, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(holder, "held", "theirs.txt", "exact"))
	shell.Kind = "before"
	shell.Status = map[string]string{"theirs.txt": " M"}
	call(t, s, "reconcile", shell)
	shell.Kind = "after"
	shell.Status = map[string]string{}
	if out := call(t, s, "reconcile", shell); out["message"] != "" || len(out["conflicts"].([]map[string]any)) != 0 {
		t.Fatalf("a path git restored warned: %v", out)
	}
}
