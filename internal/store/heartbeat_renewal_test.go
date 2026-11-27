package store

import (
	"testing"
	"time"
)

func TestOptedHeartbeatRenewsLiveExecutionAfterNewerProcessSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client string
		source string
		single bool
	}{
		{name: "claude_rung_2", client: "claude", source: "hook"},
		{name: "codex_single_session", client: "codex", source: "hook", single: true},
		{name: "d6_fallback", client: "cursor", source: "mcp", single: true},
		{name: "hookless_child", client: "claude", source: "shim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock, _ := testStore(t)
			registration := Request{
				Client: tc.client, Host: "host", SessionID: "original", Source: tc.source,
				Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100,
				SingleSession: tc.single, SuccessionProof: true,
			}
			var original map[string]any
			var prior any
			proof := "initial"
			if tc.source == "shim" {
				succession, _ := successionFixture(t, s, clock, registration)
				prior, proof = succession.PriorExecutionID, "shim_succession"
				original = call(t, s, "succeed", succession)
			} else {
				if tc.source == "mcp" {
					registration.SessionID = "mcp-42-100"
				}
				original = call(t, s, "sessions", registration)
			}
			heartbeat := Request{
				ExecutionID: original["execution_id"].(string), Generation: number(original["generation"]),
				Source: "shim", SuccessionProof: true,
			}
			// Start a 60-second lease, then register a different hook session in the same process.
			call(t, s, "heartbeat", heartbeat)
			clock.advance(30 * time.Second)
			registration.SessionID, registration.Source = "newer", "hook"
			newer := call(t, s, "sessions", registration)
			if readHookSeenMS(t, s, newer["execution_id"].(string)) == 0 {
				t.Fatalf("newer session has no recorded hook: %+v", newer)
			}
			expectError(t, s, "bind", Request{
				Client: registration.Client, Host: registration.Host, ClientPID: registration.ClientPID,
				ClientPIDStartMS: registration.ClientPIDStartMS, SuccessionProof: true,
			}, "ambiguous")

			renewed := call(t, s, "heartbeat", heartbeat)
			if renewed["execution_id"] != heartbeat.ExecutionID || renewed["generation"] != heartbeat.Generation || renewed["lease_ms"] != int64(60000) {
				t.Fatalf("tuple-free heartbeat did not renew the original pair: %+v", renewed)
			}
			assertClassifiedReply(t, renewed, prior, tc.source, proof, tc.source == "hook")
			clock.advance(31 * time.Second)
			call(t, s, "validate", heartbeat)
		})
	}
}
