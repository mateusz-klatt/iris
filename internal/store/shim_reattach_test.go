package store

import (
	"testing"
	"time"
)

func TestShimGenerationReattachRequiresHookRegisteredSession(t *testing.T) {
	s, clock, file := testStore(t)

	hookReq := Request{Client: "claude", Host: "host", SessionID: "hook-sess", Cwd: "/repo", Worktree: "/repo", Source: "hook"}
	hookReg := call(t, s, "sessions", hookReq)
	mcpReq := Request{Client: "claude", Host: "host", SessionID: "mcp-sess", Cwd: "/repo", Worktree: "/repo", Source: "mcp"}
	mcpReg := call(t, s, "sessions", mcpReq)
	endedReq := Request{Client: "claude", Host: "host", SessionID: "ended-sess", Cwd: "/repo", Worktree: "/repo", Source: "hook"}
	endedReg := call(t, s, "sessions", endedReq)
	call(t, s, "end", Request{ExecutionID: endedReg["execution_id"].(string), Generation: 1})

	// Restart the hub so active Executions enter the 120 s recovery window.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Mismatched generation on a hook-registered session is refused.
	expectError(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "hook-sess", Cwd: "/repo", Generation: 2, Source: "shim"}, "lost")

	// Matching generation on a hook-registered session reattaches in heartbeat mode and keeps hook_seen.
	reattached := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "hook-sess", Cwd: "/repo", Generation: 1, Source: "shim"})
	if reattached["execution_id"] != hookReg["execution_id"] || reattached["generation"] != int64(1) || reattached["lease_ms"] != int64(60000) {
		t.Fatalf("unexpected reattach response: %+v", reattached)
	}
	boundResp := call(t, s, "bind", Request{Execution: hookReg["execution_id"].(string), Client: "claude", Host: "host"})
	if boundResp["hook_seen"] != true {
		t.Fatalf("expected hook_seen=true after shim reattach, got %+v", boundResp)
	}

	// MCP-only fallback session (hook_seen_ms == 0) is refused by shim generation reattach.
	expectError(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "mcp-sess", Cwd: "/repo", Generation: 1, Source: "shim"}, "lost")
	_ = mcpReg

	// Ended session stays terminal and cannot be revived with its generation.
	expectError(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "ended-sess", Cwd: "/repo", Generation: 1, Source: "shim"}, "lost")

	// Restart again and advance past the 120 s recovery window: expired generation cannot be revived.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(file, clock)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(121 * time.Second)
	expectError(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "hook-sess", Cwd: "/repo", Generation: 1, Source: "shim"}, "lost")
}
