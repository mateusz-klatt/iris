package store

import "testing"

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
	call(t, s, "sessions", Request{Client: "claude", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/iris.git", RootCommit: "root", SessionID: "shared"})
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
