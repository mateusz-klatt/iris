package store

import (
	"testing"
)

// One directory reached by two paths, as through holzera's bind mount (/mnt/d/projects/iris
// and /home/klattm/projects/iris), is one worktree: the second path is made the first one this
// host named for that device and inode, so a session there gets its own mailbox back rather
// than a second agent, and a commit from either path sees the same worktree's claims (D6).
func TestTwoPathsToOneDirectoryAreOneWorktree(t *testing.T) {
	s, _, _ := testStore(t)
	const remote = "https://example.com/team/project.git"
	at := func(session, worktree, id string) Request {
		return Request{Client: "claude", Host: "holzera", SessionID: session, Cwd: worktree + "/sub", Worktree: worktree, WorktreeID: id, RemoteURL: remote, RootCommit: "root", Source: "hook"}
	}
	first := call(t, s, "sessions", at("one", "/mnt/d/projects/iris", "64768:4242"))
	firstExec := Request{ExecutionID: first["execution_id"].(string), Generation: first["generation"].(int64)}
	call(t, s, "end", firstExec)
	// The same session id from the other path resumes the same agent, its worktree the first path.
	again := call(t, s, "sessions", at("one", "/home/klattm/projects/iris", "64768:4242"))
	if again["agent"].(map[string]any)["name"] != first["agent"].(map[string]any)["name"] {
		t.Fatalf("another path to the same directory made another agent: %v and %v", first["agent"], again["agent"])
	}
	overview := call(t, s, "ui_overview", Request{})["agents"].([]map[string]any)
	if len(overview) != 1 || overview[0]["worktree"] != "/mnt/d/projects/iris" {
		t.Fatalf("agents %v", overview)
	}
	// A claim from one path is this worktree's for a commit from the other.
	held := Request{ExecutionID: again["execution_id"].(string), Generation: again["generation"].(int64)}
	call(t, s, "touch", touchRequest(held, "t1", "a.go"))
	check := Request{Host: "holzera", Worktree: "/home/klattm/projects/iris", WorktreeID: "64768:4242", RemoteURL: remote, RootCommit: "root", Paths: []string{"a.go"}}
	if out := call(t, s, "check", check); out["message"] != "" {
		t.Fatalf("a commit in the same worktree was warned: %v", out)
	}
	// Without an id, as from an older client or on Windows, a path is keyed on itself, as
	// before; and the same id on another host is another directory.
	old := call(t, s, "sessions", at("two", "/elsewhere/iris", ""))
	other := at("three", "/home/klattm/projects/iris", "64768:4242")
	other.Host = "laptop"
	elsewhere := call(t, s, "sessions", other)
	names := map[any]bool{first["agent"].(map[string]any)["name"]: true}
	for _, out := range []map[string]any{old, elsewhere} {
		if names[out["agent"].(map[string]any)["name"]] {
			t.Fatalf("a different worktree shared an agent: %v", out["agent"])
		}
		names[out["agent"].(map[string]any)["name"]] = true
	}
}
