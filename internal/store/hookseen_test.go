package store

import (
	"context"
	"testing"
	"time"
)

func seenOn(t *testing.T, s *Store, host string) map[string]any {
	t.Helper()
	return call(t, s, "hooks_seen", Request{Host: host})["clients"].(map[string]any)
}

// Doctor's "last hook" is hook activity only: a session a hook registered, an automatic claim
// or a shell reconcile. A heartbeat from the MCP shim or the monitor is not a hook (C4).
func TestOnlyHooksCountAsTheLastHook(t *testing.T) {
	s, c, _ := testStore(t)
	// session() registers as a hook does.
	a, _ := session(t, s, 1)
	if got := seenOn(t, s, "host")["codex"]; got != "2027-01-15T08:00:00Z" {
		t.Fatalf("after a hook's session: %v", got)
	}
	c.advance(10 * time.Second)
	hb := a
	hb.Source = "shim"
	call(t, s, "heartbeat", hb)
	if got := seenOn(t, s, "host")["codex"]; got != "2027-01-15T08:00:00Z" {
		t.Fatalf("a heartbeat moved it: %v", got)
	}
	call(t, s, "touch", touchRequest(a, "edit", "file"))
	if got := seenOn(t, s, "host")["codex"]; got != "2027-01-15T08:00:10Z" {
		t.Fatalf("after an automatic claim: %v", got)
	}
	c.advance(10 * time.Second)
	shell := a
	shell.Kind = "before"
	call(t, s, "reconcile", shell)
	if got := seenOn(t, s, "host")["codex"]; got != "2027-01-15T08:00:20Z" {
		t.Fatalf("after a shell reconcile: %v", got)
	}
	// An MCP process that registers its own session is not a hook either.
	c.advance(10 * time.Second)
	call(t, s, "sessions", Request{Client: "claude", Host: "host", Cwd: "/repo/w9", Worktree: "/repo/w9", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "mcp-9", Source: "mcp"})
	if _, seen := seenOn(t, s, "host")["claude"]; seen {
		t.Fatal("an MCP session counted as a hook")
	}
	if len(seenOn(t, s, "elsewhere")) != 0 {
		t.Fatal("another host's hooks were reported")
	}
	expectError(t, s, "hooks_seen", Request{}, "bad_request")
}

// The guard's check also says the repository's case policy, which doctor needs to warn of a
// sensitive repository on a checkout that folds case (ADR 003).
func TestTheCheckSaysTheCasePolicy(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := session(t, s, 1)
	check := Request{Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root"}
	if got := call(t, s, "check", check)["case_policy"]; got != "folded" {
		t.Fatalf("policy %v", got)
	}
	if err := s.SetRepositoryPolicy(context.Background(), out["repository_id"].(string), "sensitive", "deny"); err != nil {
		t.Fatal(err)
	}
	if got := call(t, s, "check", check)["case_policy"]; got != "sensitive" {
		t.Fatalf("policy %v", got)
	}
}
