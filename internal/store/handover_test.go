package store

import (
	"testing"
)

// An MCP that had to register an Execution of its own, because the session's hook had not
// registered in time, ends it once the hook's appears. Mail sent to its stray mailbox in the
// meantime moves to the hook's agent, once, unread, so it is not stranded; and only to an
// Execution of the same host and client.
func TestEndingAStrayExecutionHandsItsMailToTheHooks(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 9)
	stray := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "session-b", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "mcp"})
	hook := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "session-a", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook"})
	strayName := stray["agent"].(map[string]any)["name"].(string)
	for _, key := range []string{"one", "two"} {
		mail := sender
		mail.Key, mail.To, mail.Subject, mail.Body = key, []string{strayName}, key, "for the session"
		call(t, s, "messages", mail)
	}
	// One of them the stray mailbox already read and acknowledged: it stays where it was.
	strayReq := Request{ExecutionID: stray["execution_id"].(string), Generation: stray["generation"].(int64)}
	got := call(t, s, "inbox", strayReq)["deliveries"].([]map[string]any)
	call(t, s, "ack", Request{ExecutionID: strayReq.ExecutionID, Generation: strayReq.Generation, Attempt: got[0]["attempt"].(int64), Key: got[0]["id"].(string)})
	// Another host's Execution cannot take the mail.
	elsewhere := call(t, s, "sessions", Request{Client: "claude", Host: "other", SessionID: "x", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook"})
	refused := strayReq
	refused.HandOver = elsewhere["execution_id"].(string)
	expectError(t, s, "end", refused, "bad_request")

	end := strayReq
	end.HandOver = hook["execution_id"].(string)
	call(t, s, "end", end)
	hookReq := Request{ExecutionID: hook["execution_id"].(string), Generation: hook["generation"].(int64)}
	moved := call(t, s, "inbox", hookReq)["deliveries"].([]map[string]any)
	if len(moved) != 1 || moved[0]["subject"] != got[1]["subject"] {
		t.Fatalf("the hook's inbox has %v, want only the unacknowledged %v", moved, got[1]["subject"])
	}
	// Ending again changes nothing, so the mail moves exactly once.
	call(t, s, "end", end)
	if again := call(t, s, "inbox", hookReq)["deliveries"].([]map[string]any); len(again) != 1 {
		t.Fatalf("after a second end: %v", again)
	}
}
