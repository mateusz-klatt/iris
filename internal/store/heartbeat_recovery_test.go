package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProcessBoundHeartbeatRecoveryOptIn(t *testing.T) {
	for _, client := range []string{"cursor", "antigravity", "kimi"} {
		t.Run(client, func(t *testing.T) {
			// Use the production clock and a real restart for the legacy recovery path.
			file := filepath.Join(t.TempDir(), "hub.db")
			s, err := Open(file, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if s.lock != nil {
					_ = s.Close()
				}
			})
			registered := call(t, s, "sessions", Request{
				Client: client, Host: "host", SessionID: "process-bound", Cwd: "/repo/w",
				Worktree: "/repo/w", Source: "hook", ClientPID: 101, ClientPIDStartMS: 1001, SingleSession: true,
			})
			hb := Request{ExecutionID: registered["execution_id"].(string), Generation: 1, Source: "shim", SuccessionProof: true}
			claim := call(t, s, "claim", claimRequest(hb, "reserve", "reserved", "exact"))
			reopened := restart(t, s, file, nil)
			expectError(t, reopened, "validate", hb, "lost")

			before := databaseState(t, reopened)
			out, err := reopened.Call(t.Context(), "heartbeat", hb)
			var lost *Error
			if out != nil || !errors.As(err, &lost) || lost.Code != "lost" ||
				lost.Message != "process-bound recovery requires positive-generation shim registration" {
				t.Fatalf("opted process-bound recovery = (%+v, %v), want the registration refusal", out, err)
			}
			if databaseState(t, reopened) != before {
				t.Fatal("refused heartbeat changed database state")
			}

			hb.SuccessionProof = false
			resumed := call(t, reopened, "heartbeat", hb)
			assertHeartbeatResumed(t, reopened, hb, resumed)
			assertLegacyHeartbeatReply(t, resumed)
			claims := resumed["claims"].([]map[string]any)
			if len(claims) != 1 || claims[0]["path"] != "reserved" || number(claims[0]["fence"]) <= number(claim["fence"]) {
				t.Fatalf("legacy recovery did not rotate the reserved claim fence: %+v", claims)
			}

			hb.SuccessionProof = true
			live := call(t, reopened, "heartbeat", hb)
			if live["execution_id"] != hb.ExecutionID || live["generation"] != hb.Generation || live["lease_ms"] != int64(60000) {
				t.Fatalf("opted live heartbeat did not renew the recovered execution: %+v", live)
			}
		})
	}
}

func TestOptedHeartbeatRecoversWithoutProcessTuple(t *testing.T) {
	for _, tc := range []struct {
		client string
		single bool
	}{
		{client: "claude"},
		{client: "codex", single: true},
	} {
		t.Run(tc.client, func(t *testing.T) {
			s, clock, file := testStore(t)
			registered := call(t, s, "sessions", Request{
				Client: tc.client, Host: "host", SessionID: "native", Cwd: "/repo/w", Worktree: "/repo/w",
				Source: "hook", ClientPID: 201, ClientPIDStartMS: 2001, SingleSession: tc.single,
			})
			hb := Request{ExecutionID: registered["execution_id"].(string), Generation: 1, Source: "shim", SuccessionProof: true}
			s = restart(t, s, file, clock)
			expectError(t, s, "validate", hb, "lost")

			// The same row can bind through a native session or through its process.
			// A process-bound shim explicitly limits its heartbeat to live renewal.
			hb.ProcessBound = true
			before := databaseState(t, s)
			expectError(t, s, "heartbeat", hb, "lost")
			if databaseState(t, s) != before {
				t.Fatal("live-only heartbeat resumed a recovering row")
			}

			hb.ProcessBound = false
			resumed := call(t, s, "heartbeat", hb)
			assertHeartbeatResumed(t, s, hb, resumed)
			assertClassifiedReply(t, resumed, nil, "hook", "initial", true)

			hb.ProcessBound = true
			live := call(t, s, "heartbeat", hb)
			if live["execution_id"] != hb.ExecutionID || live["generation"] != hb.Generation || live["lease_ms"] != int64(60000) {
				t.Fatalf("live-only heartbeat could not renew a live row: %+v", live)
			}
		})
	}
}

func TestProcessBoundRegistrationRefusesSupersededProcessSession(t *testing.T) {
	s, clock, file := testStore(t)
	req := Request{Client: "claude", Host: "host", Cwd: "/repo/w", Worktree: "/repo/w", SessionID: "older",
		Source: "hook", ClientPID: 201, ClientPIDStartMS: 2001}
	original := call(t, s, "sessions", req)
	newerReq := req
	newerReq.SessionID = "newer"
	newer := call(t, s, "sessions", newerReq)
	call(t, s, "end", Request{ExecutionID: newer["execution_id"].(string), Generation: 1})
	s = restart(t, s, file, clock)
	req.Source, req.Generation, req.ProcessBound = "shim", 1, true
	before := databaseState(t, s)
	out, err := s.Call(t.Context(), "sessions", req)
	var lost *Error
	if out != nil || !errors.As(err, &lost) || lost.Code != "lost" || lost.Message != "a newer hook session superseded this client process session" {
		t.Fatalf("superseded recovery = (%+v, %v)", out, err)
	}
	if databaseState(t, s) != before {
		t.Fatal("supersession refusal changed database state")
	}
	req.ProcessBound = false
	if recovered := call(t, s, "sessions", req); recovered["execution_id"] != original["execution_id"] {
		t.Fatalf("legacy recovery changed identity: %+v", recovered)
	}
}

func TestOptedHeartbeatRecoversSingleSessionHooklessChild(t *testing.T) {
	s, clock, file := testStore(t)
	succeed, _ := successionFixture(t, s, clock, Request{
		Client: "cursor", Host: "host", SessionID: "child", Cwd: "/repo/w", Worktree: "/repo/w",
		ClientPID: 301, ClientPIDStartMS: 3001, SingleSession: true,
	})
	child := call(t, s, "succeed", succeed)
	hb := Request{ExecutionID: child["execution_id"].(string), Generation: 2, Source: "shim", SuccessionProof: true}
	s = restart(t, s, file, clock)
	expectError(t, s, "validate", hb, "lost")

	resumed := call(t, s, "heartbeat", hb)
	assertHeartbeatResumed(t, s, hb, resumed)
	assertClassifiedReply(t, resumed, succeed.PriorExecutionID, "shim", "shim_succession", false)
}

func assertHeartbeatResumed(t *testing.T, s *Store, hb Request, reply map[string]any) {
	t.Helper()
	if reply["execution_id"] != hb.ExecutionID || reply["generation"] != hb.Generation ||
		reply["handle"] != hb.ExecutionID || reply["lease_ms"] != int64(60000) {
		t.Fatalf("heartbeat did not reattach the exact execution on a strong lease: %+v", reply)
	}
	call(t, s, "validate", hb)
}
