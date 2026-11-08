package store

import (
	"testing"
)

// A shell reconcile can carry the session's identity instead of an Execution: the hub then
// registers the session and reconciles in one transaction, so a hook pays one round trip, not
// two. The registration is the one /v1/sessions makes, recovery proof included (D9, #127).
func TestAShellReconcileCanRegisterItsSession(t *testing.T) {
	s, _, file := testStore(t)
	hook := Request{Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "s", Source: "hook"}
	shell := func(r Request, kind string, status map[string]string) Request {
		r.Kind, r.Status = kind, status
		return r
	}
	out := call(t, s, "reconcile", shell(hook, "before", map[string]string{}))
	sess, ok := out["session"].(map[string]any)
	if !ok || sess["execution_id"] == "" || sess["generation"] != int64(1) {
		t.Fatalf("no session in %v", out)
	}
	first := sess["execution_id"].(string)
	// The same session again is the same Execution, as a second registration would be, and the
	// after snapshot claims what the command changed.
	again := hook
	again.Generation = 1
	out = call(t, s, "reconcile", shell(again, "after", map[string]string{"made.txt": "??"}))
	if out["session"].(map[string]any)["execution_id"] != first {
		t.Fatalf("a second shell call made another Execution: %v", out)
	}
	claims := call(t, s, "claims", Request{RepositoryID: call(t, s, "sessions", again)["repository_id"].(string)})["claims"].([]map[string]any)
	if len(claims) != 1 || claims[0]["path"] != "made.txt" {
		t.Fatalf("claims %v", claims)
	}
	// After a restart the session recovers. Without its generation it cannot prove it is the
	// same one, and the combined call is refused as lost, exactly as a registration would be.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(file, s.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	expectError(t, s2, "reconcile", shell(hook, "before", map[string]string{}), "lost")
	// With it, the session reattaches through the same call.
	out = call(t, s2, "reconcile", shell(again, "before", map[string]string{}))
	if out["session"].(map[string]any)["execution_id"] != first {
		t.Fatalf("the proof did not reattach the same Execution: %v", out)
	}
}

// The old form still works: an Execution and generation, and no session.
func TestAShellReconcileByExecutionStillWorks(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	out := call(t, s, "reconcile", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Kind: "before", Status: map[string]string{}})
	if _, has := out["session"]; has {
		t.Fatalf("the old form answered with a session: %v", out)
	}
	expectError(t, s, "reconcile", Request{ExecutionID: a.ExecutionID, Kind: "before"}, "bad_request")
}
