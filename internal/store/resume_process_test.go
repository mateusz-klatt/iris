package store

import (
	"testing"
)

// A session resumed in a new client process within its lease, as `agy --conversation=…` after an
// exit without SessionEnd, keeps its Execution. Its hook's registration then names the new process,
// so a lookup by that process finds the session again: the MCP server of the new process, which had
// to register a fallback, now finds the hook's Execution past its own and can hand its mail over
// (D6). Only a hook with a whole process identity rewrites it; a shim, watch or MCP registration of
// the same session never does, and the generation rules are unchanged.
func TestAHookResumedInANewProcessNamesThatProcess(t *testing.T) {
	s, _, _ := testStore(t)
	hook := Request{Client: "antigravity", Host: "host", SessionID: "conversation", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 100, ClientPIDStartMS: 1, SingleSession: true}
	first := call(t, s, "sessions", hook)

	// The new process's MCP server finds nothing by its process and registers its own Execution.
	newProcess := Request{Client: "antigravity", Host: "host", ClientPID: 200, ClientPIDStartMS: 2}
	fallback := call(t, s, "sessions", Request{Client: "antigravity", Host: "host", SessionID: "mcp-200-2", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "mcp", ClientPID: 200, ClientPIDStartMS: 2, SingleSession: true})
	past := newProcess
	past.Exclude = fallback["execution_id"].(string)
	expectError(t, s, "bind", past, "unbound")

	// The session's hook runs in the new process and resumes the same Execution.
	resumed := hook
	resumed.ClientPID, resumed.ClientPIDStartMS, resumed.Generation = 200, 2, first["generation"].(int64)
	if again := call(t, s, "sessions", resumed); again["execution_id"] != first["execution_id"] || again["generation"] != first["generation"] {
		t.Fatalf("the resume made %v, not %v", again, first)
	}
	if got := call(t, s, "bind", past); got["execution_id"] != first["execution_id"] {
		t.Fatalf("past its own, the new process found %v, not the hook's %v", got["execution_id"], first["execution_id"])
	}
	expectError(t, s, "bind", Request{Client: "antigravity", Host: "host", ClientPID: 100, ClientPIDStartMS: 1}, "unbound")

	// Registrations that are not the hook's, and a hook without a whole process identity, leave it.
	for _, source := range []string{"mcp", "shim", "watch"} {
		other := hook
		other.Source, other.ClientPID, other.ClientPIDStartMS = source, 300, 3
		call(t, s, "sessions", other)
	}
	partial := hook
	partial.ClientPID, partial.ClientPIDStartMS = 400, 0
	call(t, s, "sessions", partial)
	var pid, start int64
	if err := s.db.QueryRow("SELECT client_pid,client_pid_start_ms FROM Execution WHERE id=?", first["execution_id"]).Scan(&pid, &start); err != nil || pid != 200 || start != 2 {
		t.Fatalf("the process became %d/%d (%v), not 200/2", pid, start, err)
	}

	// The generation rules are unchanged: a hook naming another generation is refused.
	stale := resumed
	stale.ClientPID, stale.ClientPIDStartMS, stale.Generation = 500, 5, first["generation"].(int64)+1
	expectError(t, s, "sessions", stale, "lost")
	if err := s.db.QueryRow("SELECT client_pid FROM Execution WHERE id=?", first["execution_id"]).Scan(&pid); err != nil || pid != 200 {
		t.Fatalf("a refused hook rewrote the process to %d (%v)", pid, err)
	}
}

// After a hub restart the session is recovering. Its hook, in a new process and with its
// generation as proof, reattaches it and names the new process; a hook that also changes only whether
// its process hosts one session updates that too.
func TestARecoveringSessionReattachedFromANewProcessNamesIt(t *testing.T) {
	s, clock, file := testStore(t)
	hook := Request{Client: "kimi", Host: "host", SessionID: "session_k", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 100, ClientPIDStartMS: 1, SingleSession: true}
	first := call(t, s, "sessions", hook)
	s = restart(t, s, file, clock)
	moved := hook
	moved.ClientPID, moved.ClientPIDStartMS, moved.Generation = 200, 2, first["generation"].(int64)
	call(t, s, "sessions", moved)
	if got := call(t, s, "bind", Request{Client: "kimi", Host: "host", ClientPID: 200, ClientPIDStartMS: 2}); got["execution_id"] != first["execution_id"] {
		t.Fatalf("the reattached session was not found by its new process: %v", got)
	}
	multiplexed := moved
	multiplexed.SingleSession = false
	call(t, s, "sessions", multiplexed)
	expectError(t, s, "bind", Request{Client: "kimi", Host: "host", ClientPID: 200, ClientPIDStartMS: 2}, "unbound")
}
