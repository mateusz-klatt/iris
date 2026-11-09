package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAbsentGenerationRetryMarkerPreservesLegacyRequestEncoding(t *testing.T) {
	// Retry hashes include the Request's JSON. Adding an absent diagnostic must
	// not invalidate retry records written by an older hub for claims or mail.
	raw, err := json.Marshal(Request{ExecutionID: "ex_original", Generation: 1, Key: "unchanged"})
	if err != nil || strings.Contains(string(raw), "retry_generation") {
		t.Fatalf("absent diagnostic changed legacy request encoding: %s, %v", raw, err)
	}
}

func TestGenerationObserverNamesCommittedReplacementCauses(t *testing.T) {
	for _, tc := range []struct {
		reason, source, label string
		retry                 int64
	}{
		{"ended", "hook", "hook", 0},
		{"lease_expired", "mcp", "mcp", 9},
		{"recovery_expired", "shim", "shim", 0},
		{"epoch_lost", "watch", "watch", 0},
		{"ended", "", "legacy", 0},
		{"ended", "sensitive-source", "other", -1},
	} {
		t.Run(tc.reason+"/"+tc.label, func(t *testing.T) {
			s, clock, file := testStore(t)
			var changes []GenerationChange
			ctx := WithGenerationObserver(context.Background(), func(change GenerationChange) { changes = append(changes, change) })
			registration := Request{Client: "claude", Host: "host", SessionID: "session", Cwd: "/repo", Source: tc.source}
			call := func(op string, req Request) map[string]any {
				t.Helper()
				out, err := s.Call(ctx, op, req)
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				return out
			}
			first := call("sessions", registration)
			ex := first["execution_id"].(string)
			call("sessions", registration)
			if len(changes) != 0 {
				t.Fatalf("initial or resumed session looked like a bump: %+v", changes)
			}
			authority := Request{ExecutionID: ex, Generation: 1, Source: "watch"}
			switch tc.reason {
			case "ended":
				call("end", authority)
			case "lease_expired":
				call("heartbeat", authority)
				clock.advance(61 * time.Second)
			case "recovery_expired":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s.lock = nil
				reopened, err := Open(file, clock)
				if err != nil {
					t.Fatal(err)
				}
				s = reopened
				defer func() { _ = reopened.Close() }()
				// A successful reattachment keeps its generation and stays quiet.
				if recovered := call("heartbeat", authority); recovered["execution_id"] != ex || recovered["generation"] != int64(1) {
					t.Fatalf("reattachment changed identity: %v", recovered)
				}
				if len(changes) != 0 {
					t.Fatalf("reattachment logged a bump: %+v", changes)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(file, clock)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				clock.advance(121 * time.Second)
			case "epoch_lost":
				if _, err := s.db.Exec("UPDATE Execution SET boot_epoch=boot_epoch-1 WHERE id=?", ex); err != nil {
					t.Fatal(err)
				}
			}
			registration.RetryGeneration = tc.retry
			second := call("sessions", registration)
			proof := "absent"
			if tc.retry > 0 {
				proof = "stale_retry"
			}
			want := GenerationChange{ExecutionID: second["execution_id"].(string), PreviousExecutionID: ex, Reason: tc.reason, Source: tc.label, Proof: proof, Generation: 2, PreviousGeneration: 1, RetryGeneration: max(tc.retry, 0)}
			if len(changes) != 1 || changes[0] != want {
				t.Fatalf("replacement diagnostic: got %+v, want %+v", changes, want)
			}
			call("sessions", registration)
			if len(changes) != 1 {
				t.Fatalf("retry duplicated a bump: %+v", changes)
			}
			// A diagnostic retry marker cannot grant authority to an old generation.
			registration.Generation = 1
			_, err := s.Call(ctx, "sessions", registration)
			var lost *Error
			if !errors.As(err, &lost) || lost.Code != "lost" || len(changes) != 1 {
				t.Fatalf("diagnostic marker affected stale authority: %v, changes %+v", err, changes)
			}
		})
	}
}

func TestRolledBackGenerationNeverReachesObserver(t *testing.T) {
	s, _, file := testStore(t)
	authority, _ := session(t, s, 1)
	call(t, s, "end", authority)
	var changes []GenerationChange
	ctx := WithGenerationObserver(context.Background(), func(change GenerationChange) { changes = append(changes, change) })
	registration := Request{Client: "codex", Host: "host", SessionID: "1", Cwd: "/repo/w1", Source: "hook"}
	fault := installFaultDriver(t, s, file)
	fault.discover = true // Fail the commit after the replacement was prepared.
	if _, err := s.Call(ctx, "sessions", registration); !errors.Is(err, errStorageFault) {
		t.Fatalf("commit failure: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("rolled-back replacement was reported: %+v", changes)
	}
	fault.discover = false
	fault.count = 0
	second, err := s.Call(ctx, "sessions", registration)
	if err != nil || len(changes) != 1 || changes[0].Generation != 2 || changes[0].ExecutionID != second["execution_id"] {
		t.Fatalf("committed retry: %v, %v, changes %+v", second, err, changes)
	}
}

func TestGenerationObserverCanReadAndRenewTheCommittedSession(t *testing.T) {
	s, _, _ := testStore(t)
	authority, _ := session(t, s, 1)
	call(t, s, "end", authority)
	observed := false
	ctx := WithGenerationObserver(context.Background(), func(change GenerationChange) {
		// Keep the regression timing-free: the old callback ran with this lock
		// held, so a reentrant Call would deadlock instead of reporting failure.
		if !s.mu.TryLock() {
			t.Error("generation observer prevents other store operations")
			return
		}
		s.mu.Unlock()
		status := call(t, s, "status", Request{})
		if status["counts"].(map[string]int)["Execution"] != 2 {
			t.Fatalf("observer did not see the committed replacement: %v", status)
		}
		fresh := Request{ExecutionID: change.ExecutionID, Generation: change.Generation, Source: "watch"}
		if valid := call(t, s, "validate", fresh); valid["execution_id"] != change.ExecutionID {
			t.Fatalf("observer did not see current authority: %v", valid)
		}
		if lease := call(t, s, "heartbeat", fresh); lease["lease_ms"] != int64(60000) {
			t.Fatalf("observer blocked renewal: %v", lease)
		}
		observed = true
	})
	_, err := s.Call(ctx, "sessions", Request{Client: "codex", Host: "host", SessionID: "1", Cwd: "/repo/w1", Source: "hook"})
	if err != nil || !observed {
		t.Fatalf("observer could not use the committed session: %v, observed %v", err, observed)
	}
}
