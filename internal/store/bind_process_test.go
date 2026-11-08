package store

import (
	"testing"
)

// A Claude process hosts one top-level session at a time, so the client-process rung finds its
// session's Execution though the asking process names another session id, as an MCP server
// does after a resume. Its subagents register with their parent and do not compete; two live
// top-level sessions of one process are ambiguous; and a process can ask past its own
// Execution, to find the hook's (D6).
func TestTheClientProcessFindsItsTopLevelSession(t *testing.T) {
	s, _, _ := testStore(t)
	hook := Request{Client: "claude", Host: "host", SessionID: "session-a", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 4242, ClientPIDStartMS: 77}
	top := call(t, s, "sessions", hook)
	child := hook
	child.SessionID, child.ParentSessionID = "subagent", "session-a"
	call(t, s, "sessions", child)
	bind := Request{Client: "claude", Host: "host", ClientPID: 4242, ClientPIDStartMS: 77}
	if got := call(t, s, "bind", bind); got["execution_id"] != top["execution_id"] {
		t.Fatalf("bound %v, the session's is %v", got["execution_id"], top["execution_id"])
	}
	// Another process, or a multiplexing client, is not matched this way.
	other := bind
	other.ClientPID = 4243
	expectError(t, s, "bind", other, "unbound")
	codex := bind
	codex.Client = "codex"
	expectError(t, s, "bind", codex, "unbound")
	// A second live top-level session of the process is ambiguous, unless the asker excludes
	// its own, as an MCP that had to register one does.
	own := hook
	own.SessionID, own.Source = "session-b", "mcp"
	mine := call(t, s, "sessions", own)
	expectError(t, s, "bind", bind, "ambiguous")
	past := bind
	past.Exclude = mine["execution_id"].(string)
	if got := call(t, s, "bind", past); got["execution_id"] != top["execution_id"] {
		t.Fatalf("bound %v past its own, the hook's is %v", got["execution_id"], top["execution_id"])
	}
}
