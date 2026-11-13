package store

import (
	"testing"
	"time"
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

// A session and its client process are looked up together (ADR 030), as Kimi's one-shot watcher
// asks. After `/clear` whose SessionEnd failed, one Kimi process has two live Executions, which the
// process alone cannot tell apart; the session id selects the new one. Every constraint applies,
// the answer says it came from this lookup, and an incomplete process identity is refused rather
// than looked up by session alone.
func TestASessionAndItsClientProcessAreLookedUpTogether(t *testing.T) {
	s, clock, file := testStore(t)
	hook := Request{Client: "kimi", Host: "host", SessionID: "session_old", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 4242, ClientPIDStartMS: 77, SingleSession: true}
	call(t, s, "sessions", hook)
	cleared := hook
	cleared.SessionID = "session_new"
	current := call(t, s, "sessions", cleared)

	process := Request{Client: "kimi", Host: "host", ClientPID: 4242, ClientPIDStartMS: 77}
	expectError(t, s, "bind", process, "ambiguous")
	lookup := process
	lookup.SessionID, lookup.Cwd = "session_new", "/somewhere/else"
	got := call(t, s, "bind", lookup)
	if got["execution_id"] != current["execution_id"] || got["match"] != "process_session" || got["hook_seen"] != true {
		t.Fatalf("bound %v, the new session's is %v", got, current["execution_id"])
	}

	// Each constraint on its own refuses the lookup.
	for name, change := range map[string]func(*Request){
		"another process":    func(r *Request) { r.ClientPID = 4243 },
		"another start time": func(r *Request) { r.ClientPIDStartMS = 78 },
		"another session":    func(r *Request) { r.SessionID = "session_other" },
		"another client":     func(r *Request) { r.Client = "codex" },
		"another host":       func(r *Request) { r.Host = "elsewhere" },
	} {
		wrong := lookup
		change(&wrong)
		if _, err := s.Call(t.Context(), "bind", wrong); !isCode(err, "unbound") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A process not known to host one session, and a registration no hook made, never match.
	multiplexed := hook
	multiplexed.SessionID, multiplexed.ClientPID, multiplexed.SingleSession = "session_multi", 5000, false
	call(t, s, "sessions", multiplexed)
	mcp := hook
	mcp.SessionID, mcp.ClientPID, mcp.Source = "session_mcp", 6000, "mcp"
	call(t, s, "sessions", mcp)
	for _, r := range []Request{
		{Client: "kimi", Host: "host", SessionID: "session_multi", ClientPID: 5000, ClientPIDStartMS: 77},
		{Client: "kimi", Host: "host", SessionID: "session_mcp", ClientPID: 6000, ClientPIDStartMS: 77},
	} {
		expectError(t, s, "bind", r, "unbound")
	}
	// An incomplete or invalid process identity is refused, not looked up by session alone.
	for _, r := range []Request{
		{Client: "kimi", Host: "host", SessionID: "session_new", ClientPID: 4242},
		{Client: "kimi", Host: "host", SessionID: "session_new", ClientPIDStartMS: 77},
		{Client: "kimi", Host: "host", SessionID: "session_new", ClientPID: -1, ClientPIDStartMS: 77},
	} {
		expectError(t, s, "bind", r, "bad_request")
	}
	// The older lookups are unchanged and do not claim the combined match.
	bySession := call(t, s, "bind", Request{Client: "kimi", Host: "host", SessionID: "session_new"})
	if bySession["execution_id"] != current["execution_id"] || bySession["match"] != nil {
		t.Fatalf("by session: %v", bySession)
	}
	// The process-only lookup still returns the MCP-only registration that the combined one refuses.
	if byProcess := call(t, s, "bind", Request{Client: "kimi", Host: "host", ClientPID: 6000, ClientPIDStartMS: 77}); byProcess["session_id"] != "session_mcp" || byProcess["match"] != nil {
		t.Fatalf("by process: %v", byProcess)
	}

	// A recovering Execution, after a restart, and a lapsed lease are not live.
	s = restart(t, s, file, clock)
	expectError(t, s, "bind", lookup, "unbound")
	call(t, s, "sessions", Request{Client: "kimi", Host: "host", SessionID: "session_new", Cwd: "/repo/w", Worktree: "/repo/w", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook", ClientPID: 4242, ClientPIDStartMS: 77, SingleSession: true, Generation: current["generation"].(int64)})
	if got := call(t, s, "bind", lookup); got["execution_id"] != current["execution_id"] {
		t.Fatalf("after reattaching: %v", got)
	}
	clock.advance(time.Hour)
	expectError(t, s, "bind", lookup, "unbound")
}
