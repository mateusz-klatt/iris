package store

import (
	"errors"
	"testing"
	"time"
)

// A generation-zero registration from iris mcp or iris watch carries no retry_generation.
// After the hook session has ended, that request must not open generation G+1. The hub reports
// lost with highest_generation, the same floor a retried hook registration uses.

func TestFloorBypassHubRefusesProofLessMCPAndWatch(t *testing.T) {
	for _, source := range []string{"mcp", "watch"} {
		t.Run(source+"/ended", func(t *testing.T) {
			s, _, _ := testStore(t)
			registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			first := call(t, s, "sessions", registration)
			id := first["execution_id"].(string)
			call(t, s, "end", Request{ExecutionID: id, Generation: 1})
			var state string
			var generation int64
			if err := s.db.QueryRow("SELECT state, generation FROM Execution WHERE id=?", id).Scan(&state, &generation); err != nil {
				t.Fatal(err)
			}
			if state != "ended" || generation != 1 {
				t.Fatalf("setup left state %s generation %d", state, generation)
			}
			before := databaseState(t, s)
			registration.Source = source
			registration.Generation = 0
			registration.RetryGeneration = 0
			out, err := s.Call(t.Context(), "sessions", registration)
			if err == nil {
				t.Fatalf("%s after end created %+v", source, out)
			}
			assertRegistrationLost(t, err, 1)
			if out != nil || databaseState(t, s) != before {
				t.Fatalf("%s after end changed retained history: %v", source, out)
			}
		})
	}
}

func TestFloorBypassProofLessMCPAndWatchAfterLossAreRefused(t *testing.T) {
	for _, source := range []string{"mcp", "watch", ""} {
		name := source
		if name == "" {
			name = "legacy"
		}
		t.Run(name+"/lost", func(t *testing.T) {
			s, clock, _ := testStore(t)
			registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			call(t, s, "sessions", registration)
			clock.advance(15*time.Minute + time.Millisecond)
			call(t, s, "expire", Request{})
			registration.Source = source
			before := databaseState(t, s)
			out, err := s.Call(t.Context(), "sessions", registration)
			if out != nil {
				t.Fatalf("%s after loss returned authority: %+v", source, out)
			}
			assertRegistrationLost(t, err, 1)
			if databaseState(t, s) != before {
				t.Fatalf("%s after loss changed the mailbox, deliveries, claims or retained history", source)
			}
		})
	}
}

func TestEndedSessionWithoutHookHistoryRefusesNonHookGenerationZero(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp"}
	first := call(t, s, "sessions", registration)
	id := first["execution_id"].(string)
	var hookSeen int64
	var state string
	if err := s.db.QueryRow("SELECT hook_seen_ms, state FROM Execution WHERE id=?", id).Scan(&hookSeen, &state); err != nil {
		t.Fatal(err)
	}
	if hookSeen != 0 || state != "active" {
		t.Fatalf("setup recorded a hook or was not active: hook_seen=%d state=%s", hookSeen, state)
	}
	call(t, s, "end", Request{ExecutionID: id, Generation: 1})
	if err := s.db.QueryRow("SELECT hook_seen_ms, state FROM Execution WHERE id=?", id).Scan(&hookSeen, &state); err != nil || hookSeen != 0 || state != "ended" {
		t.Fatalf("ended row hook_seen=%d state=%s err=%v", hookSeen, state, err)
	}
	for _, source := range []string{"mcp", "shim", "watch", ""} {
		registration.Source = source
		registration.Generation = 0
		registration.RetryGeneration = 0
		before := databaseState(t, s)
		out, err := s.Call(t.Context(), "sessions", registration)
		if out != nil {
			t.Fatalf("%q revived an ended session: %+v", source, out)
		}
		assertRegistrationLost(t, err, 1)
		var lost *Error
		if !errors.As(err, &lost) || lost.Message != "non-hook generation-zero registration cannot succeed an ended session" {
			t.Fatalf("%q refusal: %v", source, err)
		}
		if databaseState(t, s) != before {
			t.Fatalf("%q changed retained rows", source)
		}
	}
	registration.Source = "hook"
	out := call(t, s, "sessions", registration)
	if out["generation"] != int64(2) || out["execution_id"] == id {
		t.Fatalf("hook did not advance the ended session: %+v", out)
	}
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", id).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("hook changed the ended row: state=%s err=%v", state, err)
	}
}

func TestFloorBypassHookSessionStartAfterEndGetsNextGeneration(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	first := call(t, s, "sessions", registration)
	id := first["execution_id"].(string)
	call(t, s, "end", Request{ExecutionID: id, Generation: 1})
	out := call(t, s, "sessions", registration)
	if out["generation"] != int64(2) || out["execution_id"] == id {
		t.Fatalf("hook SessionStart after end did not get G+1: %+v", out)
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM Execution WHERE id=?", id).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("hook SessionStart changed the ended row: state=%s err=%v", state, err)
	}
}

func TestFloorBypassHubRejectsNonHookRegistrationVariantsAfterEnd(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		retry        int64
		code         string
	}{
		{"spoofed shim", "shim", 0, "lost"},
		{"legacy empty source", "", 0, "lost"},
		{"uppercase source", "MCP", 0, "bad_request"},
		{"whitespace source", "mcp ", 0, "bad_request"},
		{"unknown source", "foreign", 0, "bad_request"},
		{"negative retry", "mcp", -1, "bad_request"},
		{"mcp caller retry floor", "mcp", 1, "lost"},
		{"watch caller retry floor", "watch", 1, "lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := testStore(t)
			registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
			first := call(t, s, "sessions", registration)
			call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
			registration.Source, registration.RetryGeneration = tc.source, tc.retry
			before := databaseState(t, s)
			out, err := s.Call(t.Context(), "sessions", registration)
			if out != nil {
				t.Fatalf("refused registration returned authority: %+v", out)
			}
			if tc.code == "lost" {
				assertRegistrationLost(t, err, 1)
			} else {
				var domain *Error
				if !errors.As(err, &domain) || domain.Code != tc.code {
					t.Fatalf("got %v, want %s", err, tc.code)
				}
			}
			if databaseState(t, s) != before {
				t.Fatalf("refused registration changed retained history")
			}
		})
	}
}

func TestFloorBypassConcurrentSessionEndAndRegistration(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"}
	first := call(t, s, "sessions", registration)
	id := first["execution_id"].(string)
	registration.Source = "shim"
	start := make(chan struct{})
	registrationAtLock := make(chan struct{})
	releaseRegistration := make(chan struct{})
	type registrationResult struct {
		out map[string]any
		err error
	}
	registrations := make(chan registrationResult, 1)
	ends := make(chan error, 1)
	go func() {
		<-start
		ctx := withLockObserver(t.Context(), lockObserver{
			beforeLock: func() {
				close(registrationAtLock)
				<-releaseRegistration
			},
			afterUnlock: func(time.Duration, time.Duration) {},
		})
		out, err := s.Call(ctx, "sessions", registration)
		registrations <- registrationResult{out, err}
	}()
	go func() {
		<-start
		_, err := s.Call(t.Context(), "end", Request{ExecutionID: id, Generation: 1})
		ends <- err
	}()
	close(start)
	<-registrationAtLock
	endErr := <-ends
	close(releaseRegistration)
	result := <-registrations
	if endErr != nil {
		t.Fatalf("SessionEnd failed: %v", endErr)
	}
	if result.out != nil {
		t.Fatalf("registration after SessionEnd returned authority: %+v", result.out)
	}
	assertRegistrationLost(t, result.err, 1)
	var count, generation int
	var state string
	if err := s.db.QueryRow("SELECT COUNT(*),MIN(state),MAX(generation) FROM Execution WHERE host_id='host' AND client='claude' AND session_id='S'").Scan(&count, &state, &generation); err != nil {
		t.Fatal(err)
	}
	if count != 1 || state != "ended" || generation != 1 {
		t.Fatalf("concurrent end created a successor: count=%d state=%s generation=%d", count, state, generation)
	}
}

func TestFloorBypassHubRefusesExactGenerationAfterEnd(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp"}
	first := call(t, s, "sessions", Request{Client: "claude", Host: "host", SessionID: "S", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook"})
	call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
	before := databaseState(t, s)
	registration.Generation = 1
	out, err := s.Call(t.Context(), "sessions", registration)
	if err == nil {
		t.Fatalf("exact generation revived %+v", out)
	}
	assertRegistrationLost(t, err, 1)
	if out != nil || databaseState(t, s) != before {
		t.Fatalf("exact-generation refusal changed history: %v", out)
	}
}

func TestFloorBypassHubRefusesSyntheticRegistrationOverHookProcessHistory(t *testing.T) {
	for _, state := range []string{"active", "lost", "ended"} {
		t.Run(state, func(t *testing.T) {
			s, clock, _ := testStore(t)
			hook := Request{Client: "cursor", Host: "host", SessionID: "real-session", Cwd: "/repo/w", Worktree: "/repo/w", Source: "hook", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true}
			first := call(t, s, "sessions", hook)
			switch state {
			case "lost":
				clock.advance(16 * time.Minute)
				call(t, s, "expire", Request{})
			case "ended":
				call(t, s, "end", Request{ExecutionID: first["execution_id"].(string), Generation: 1})
			}
			registration := hook
			registration.SessionID, registration.Source = "mcp-42-100", "mcp"
			before := databaseState(t, s)
			out, err := s.Call(t.Context(), "sessions", registration)
			if out != nil {
				t.Fatalf("synthetic registration returned authority: %+v", out)
			}
			assertRegistrationLost(t, err, 0)
			if databaseState(t, s) != before {
				t.Fatal("synthetic registration changed hook history or mailbox state")
			}
		})
	}
}
