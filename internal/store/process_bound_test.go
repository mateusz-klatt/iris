package store

import (
	"errors"
	"testing"
	"time"
)

func TestProcessBoundOnlyRestricts(t *testing.T) {
	// Every outcome is either the same in both modes or stricter with the flag.
	type decision int
	const (
		refuse decision = iota
		allowUnflagged
		allowBoth
	)
	for _, tc := range []struct {
		name, client                           string
		single, omitTuple                      bool
		heartbeatRecovery                      decision
		registrationLive, registrationRecovery decision
	}{
		{name: "single", client: "cursor", single: true, heartbeatRecovery: refuse, registrationLive: allowBoth, registrationRecovery: allowBoth},
		{name: "non_single", client: "claude", omitTuple: true, heartbeatRecovery: allowUnflagged, registrationLive: allowUnflagged, registrationRecovery: allowUnflagged},
		{name: "codex", client: "codex", single: true, heartbeatRecovery: allowUnflagged, registrationLive: allowBoth, registrationRecovery: allowBoth},
		{name: "hookless_child", client: "cursor", single: true, heartbeatRecovery: allowUnflagged, registrationLive: refuse, registrationRecovery: refuse},
		{name: "d6_fallback", client: "cursor", single: true, heartbeatRecovery: refuse, registrationLive: allowBoth, registrationRecovery: refuse},
	} {
		for _, route := range []string{"heartbeat", "sessions"} {
			for _, mode := range []struct {
				name    string
				flagged bool
			}{
				{name: "unflagged"}, {name: "flagged", flagged: true},
			} {
				t.Run(tc.name+"/"+route+"/"+mode.name, func(t *testing.T) {
					s, clock, file := testStore(t)
					registration := Request{
						Client: tc.client, Host: "host", SessionID: "session", Cwd: "/repo/w", Worktree: "/repo/w",
						ClientPID: 42, ClientPIDStartMS: 100, SingleSession: tc.single, Source: "hook",
					}
					if tc.name == "d6_fallback" {
						registration.SessionID, registration.Source = "mcp-42-100", "mcp"
					}
					var original map[string]any
					if tc.name == "hookless_child" {
						succession, _ := successionFixture(t, s, clock, registration)
						original = call(t, s, "succeed", succession)
					} else {
						original = call(t, s, "sessions", registration)
					}
					identity := Request{ExecutionID: original["execution_id"].(string), Generation: original["generation"].(int64)}
					req := registration
					if req.Source == "hook" {
						req.Source, req.Generation = "shim", identity.Generation
					}
					if tc.omitTuple {
						req.ClientPID, req.ClientPIDStartMS = 0, 0
					}
					live, recovery := tc.registrationLive, tc.registrationRecovery
					if route == "heartbeat" {
						req = identity
						req.Source = "shim"
						live, recovery = allowBoth, tc.heartbeatRecovery
					}
					req.SuccessionProof, req.ProcessBound = true, mode.flagged
					clock.advance(time.Second)
					assertProcessBoundDecision(t, s, route, req, identity, live == allowBoth || live == allowUnflagged && !mode.flagged)
					s = restart(t, s, file, clock)
					expectError(t, s, "validate", identity, "lost")
					assertProcessBoundDecision(t, s, route, req, identity, recovery == allowBoth || recovery == allowUnflagged && !mode.flagged)
				})
			}
		}
	}
}

func assertProcessBoundDecision(t *testing.T, s *Store, route string, req, identity Request, allowed bool) {
	t.Helper()
	before := databaseState(t, s)
	out, err := s.Call(t.Context(), route, req)
	if !allowed {
		var lost *Error
		if out != nil || !errors.As(err, &lost) || lost.Code != "lost" {
			t.Fatalf("%s returned (%+v, %v), want lost with no binding", route, out, err)
		}
		if databaseState(t, s) != before {
			t.Fatalf("refused %s changed database state", route)
		}
		return
	}
	if err != nil || out["execution_id"] != identity.ExecutionID || out["generation"] != identity.Generation || out["lease_ms"] != int64(60000) {
		t.Fatalf("%s returned (%+v, %v), want the original execution on a strong lease", route, out, err)
	}
	var deadline int64
	if err := s.db.QueryRow("SELECT deadline_tick_ms FROM Execution WHERE id=?", identity.ExecutionID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if want := s.clock.Tick() + 60000; deadline != want {
		t.Fatalf("%s left lease deadline at %d, want %d", route, deadline, want)
	}
	call(t, s, "validate", identity)
}

func TestProcessBoundRetriesRecheckCurrentSession(t *testing.T) {
	for _, route := range []string{"heartbeat", "sessions"} {
		t.Run(route, func(t *testing.T) {
			for _, first := range []struct {
				name    string
				flagged bool
			}{
				{name: "unflagged_to_flagged"}, {name: "flagged_to_unflagged", flagged: true},
			} {
				t.Run(first.name, func(t *testing.T) {
					s, clock, file := testStore(t)
					registration := Request{
						Client: "claude", Host: "host", SessionID: "session", Cwd: "/repo/w", Worktree: "/repo/w",
						ClientPID: 42, ClientPIDStartMS: 100, Source: "hook",
					}
					original := call(t, s, "sessions", registration)
					identity := Request{ExecutionID: original["execution_id"].(string), Generation: original["generation"].(int64)}
					req := registration
					if route == "heartbeat" {
						req = identity
					} else if !first.flagged {
						req.ClientPID, req.ClientPIDStartMS = 0, 0
					}
					req.Source, req.Generation, req.Key = "shim", identity.Generation, "same-key"
					req.SuccessionProof, req.ProcessBound = true, first.flagged
					assertProcessBoundDecision(t, s, route, req, identity, true)

					// The second request keeps the complete body, including the key. Only
					// the query flag changes. An earlier success cannot supply its answer.
					if first.flagged {
						call(t, s, "end", identity)
					} else if route == "heartbeat" {
						s = restart(t, s, file, clock)
						expectError(t, s, "validate", identity, "lost")
					}
					req.ProcessBound = !first.flagged
					assertProcessBoundDecision(t, s, route, req, identity, false)
					if !first.flagged {
						req.ProcessBound = false
						assertProcessBoundDecision(t, s, route, req, identity, true)
					}
					if records := rowCount(t, s, "SELECT COUNT(*) FROM Operation"); records != 0 {
						t.Fatalf("%s created %d retry records", route, records)
					}
				})
			}
		})
	}
}
