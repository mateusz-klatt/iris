package store

import (
	"testing"
)

// An MCP that had to register an Execution of its own, because the session's hook had not
// registered in time, ends it once the hook's appears. Mail sent to its stray mailbox in the
// meantime moves to the hook's agent, once, unread, so it is not stranded; and only to the
// session the client process rung finds for it, never to another session or from a hook's.
func TestEndingAStrayExecutionHandsItsMailToTheHooks(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 9)
	stray := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "session-b", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "mcp", ClientPID: 4242, ClientPIDStartMS: 77})
	hook := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "session-a", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 4242, ClientPIDStartMS: 77})
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
	// Nothing else may take the mail: another session of the same client on the same host, in
	// another process, or a subagent of the right process; and a hook's Execution may not hand
	// its own mail away.
	other := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "other", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 5151, ClientPIDStartMS: 99})
	sub := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "sub", ParentSessionID: "session-a", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 4242, ClientPIDStartMS: 77})
	for _, to := range []any{other["execution_id"], sub["execution_id"]} {
		refused := strayReq
		refused.HandOver = to.(string)
		expectError(t, s, "end", refused, "bad_request")
	}
	fromHook := Request{ExecutionID: other["execution_id"].(string), Generation: other["generation"].(int64), HandOver: hook["execution_id"].(string)}
	expectError(t, s, "end", fromHook, "bad_request")

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

// A message sent to both mailboxes reaches the hook's agent once: the stray copy is settled,
// not moved, so the move cannot collide with the copy already there; a moved one asks for
// attention, as new mail does.
func TestAMessageInBothMailboxesIsHandedOverOnce(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 9)
	same := Request{Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", ClientPID: 4242, ClientPIDStartMS: 77}
	strayReq, hookReq := same, same
	strayReq.SessionID, strayReq.Source = "session-b", "mcp"
	hookReq.SessionID, hookReq.Source = "session-a", "hook"
	stray := call(t, s, "sessions", strayReq)
	hook := call(t, s, "sessions", hookReq)
	both := sender
	both.Key, both.Subject, both.Body = "both", "to both", "b"
	both.To = []string{stray["agent"].(map[string]any)["name"].(string), hook["agent"].(map[string]any)["name"].(string)}
	call(t, s, "messages", both)
	only := sender
	only.Key, only.Subject, only.Body, only.To = "stray", "stray only", "b", []string{stray["agent"].(map[string]any)["name"].(string)}
	call(t, s, "messages", only)
	call(t, s, "end", Request{ExecutionID: stray["execution_id"].(string), Generation: stray["generation"].(int64), HandOver: hook["execution_id"].(string)})
	hookExec := Request{ExecutionID: hook["execution_id"].(string), Generation: hook["generation"].(int64)}
	if n := call(t, s, "sessions", hookReq)["attention"].(map[string]any)["unread"]; n != 2 {
		t.Fatalf("the hook's agent asks attention for %v deliveries, want 2", n)
	}
	got := call(t, s, "inbox", hookExec)["deliveries"].([]map[string]any)
	subjects := map[string]int{}
	for _, d := range got {
		subjects[d["subject"].(string)]++
	}
	if len(got) != 2 || subjects["to both"] != 1 || subjects["stray only"] != 1 {
		t.Fatalf("the hook's inbox: %v", subjects)
	}
}
