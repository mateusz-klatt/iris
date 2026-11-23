package binding

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type staleHookMemory struct{ generation int }

func (m *staleHookMemory) RecallGeneration(string, string) (int, error) { return m.generation, nil }
func (m *staleHookMemory) Remember(_ string, _ string, generation int)  { m.generation = generation }
func (m *staleHookMemory) Forget(string, string, int)                   {}

func TestHookAttachesToSuccessorWithCurrentOrStaleProof(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "current proof"
		if stale {
			name = "stale proof with restore-floor retry"
		}
		t.Run(name, func(t *testing.T) {
			f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
			f.hub.clock.Advance(16 * time.Minute)
			r := f.resolver("claude", "S", proc.Ancestor{})
			succeeded, err := r.Resolve(t.Context(), "")
			if err != nil || succeeded.Generation != 2 {
				t.Fatalf("succession setup: %+v %v", succeeded, err)
			}
			before := len(f.hub.sessions())
			var memory hook.Memory
			if stale {
				memory = &staleHookMemory{generation: 1}
			}
			out := (hook.Runner{
				ClientName: "claude", EventName: "SessionStart", Stdin: strings.NewReader(`{"session_id":"S","cwd":` + quoteJSON(f.cwd) + `}`),
				Hub: &hubclient.Client{Config: config{f.hub.srv.URL}}, Getenv: successionEnvironment("claude", "S", f.token), Host: func() string { return "floor-host" },
				Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
				Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
				Memory:   memory,
			}).Run()
			if failures, err := hook.ReadRecentFailures(f.token, 10); err != nil || len(failures) != 0 {
				t.Fatalf("hook after succession failed: %v %v, stdout=%s stderr=%s", failures, err, out.Stdout, out.Stderr)
			}
			requests := f.hub.sessions()[before:]
			if stale {
				if len(requests) != 2 || requests[0].Generation != 1 || requests[0].RetryGeneration != 0 || requests[1].Generation != 0 || requests[1].RetryGeneration != 1 {
					t.Fatalf("stale hook did not use its bounded retry: %+v", requests)
				}
			} else if len(requests) != 1 || requests[0].Generation != 2 || requests[0].RetryGeneration != 0 {
				t.Fatalf("current hook did not attach directly to the successor: %+v", requests)
			}
			rows := executionRows(t, f.hub.file)
			if len(rows) != 2 || rows[1].generation != 2 || rows[1].hookSeen == 0 || rows[1].state != "active" {
				t.Fatalf("hook created another generation or left no evidence: %+v", rows)
			}
		})
	}
}

func TestIDModeRootSuccessionSendsVerifiedSingleSessionProcess(t *testing.T) {
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S", anc)
	assertRegisteredProcess(t, f.hub.file, anc)
	f.hub.clock.Advance(16 * time.Minute)
	r := f.resolver("cursor", "", anc)
	b, primary, err := r.ResolveCall(t.Context(), "", "S", "S")
	if err != nil || !primary || b.Generation != 2 || b.SessionID != "S" {
		t.Fatalf("id-mode root succession: bound=%+v primary=%t err=%v", b, primary, err)
	}
	requests := f.hub.successions()
	if len(requests) != 1 || requests[0]["client_pid"] != float64(anc.PID) || requests[0]["client_pid_start_ms"] != float64(anc.StartMS) {
		t.Fatalf("succeed request did not carry the verified process: %+v", requests)
	}
	assertNoGenerationZeroRegistration(t, f.hub.sessions())
}

func assertRegisteredProcess(t *testing.T, file string, anc proc.Ancestor) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var pid, start, single, hookSeen int64
	if err = db.QueryRow("SELECT client_pid, client_pid_start_ms, single_session, hook_seen_ms FROM Execution ORDER BY generation LIMIT 1").Scan(&pid, &start, &single, &hookSeen); err != nil {
		t.Fatal(err)
	}
	if pid != int64(anc.PID) || start != anc.StartMS || single != 1 || hookSeen == 0 {
		t.Fatalf("hook registered pid=%d start=%d single=%d hook_seen=%d, want pid=%d start=%d single=1 hook_seen>0", pid, start, single, hookSeen, anc.PID, anc.StartMS)
	}
}

func TestCodexRootSucceedsButSubagentAndExplicitBindingsDoNot(t *testing.T) {
	t.Run("Codex id-mode root", func(t *testing.T) {
		f := newSuccessionBindingFixture(t, "codex", "SessionStart", "S", proc.Ancestor{})
		f.hub.clock.Advance(16 * time.Minute)
		r := f.resolver("codex", "", proc.Ancestor{})
		b, primary, err := r.ResolveCall(t.Context(), "", "S", "S")
		if err != nil || !primary || b.Generation != 2 || b.Rung != 2 {
			t.Fatalf("Codex root succession: bound=%+v primary=%t err=%v", b, primary, err)
		}
		assertNoGenerationZeroRegistration(t, f.hub.sessions())
	})
	for _, tc := range []struct {
		name, execution, session, thread string
		override                         bool
	}{
		{name: "subagent", session: "S", thread: "child"},
		{name: "foreign session lookup", session: "other", thread: "other"},
		{name: "live override", execution: "ex_live", override: true},
		{name: "explicit handle", execution: "ex_handle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binds := 0
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				if req.URL.Path != "/v1/bind" {
					t.Fatalf("excluded binding reached %s", req.URL.Path)
				}
				binds++
				if tc.name == "foreign session lookup" && binds == 1 {
					return http.StatusOK, map[string]any{"execution_id": "ex_root", "generation": 1, "session_id": "S"}
				}
				return refused("unbound")
			}}
			memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one"}
			r := scriptedSuccessionResolver(h, memory)
			r.Client = "codex"
			if tc.override {
				r.Getenv = func(key string) string {
					if key == "IRIS_EXECUTION" {
						return "ex_live"
					}
					return ""
				}
			}
			if tc.name == "foreign session lookup" {
				if _, _, err := r.ResolveCall(t.Context(), "", "S", "S"); err != nil {
					t.Fatalf("root mode setup: %v", err)
				}
			}
			_, _, err := r.ResolveCall(t.Context(), tc.execution, tc.session, tc.thread)
			if err == nil || len(h.lines) == 0 {
				t.Fatalf("excluded call unexpectedly succeeded: %v", err)
			}
			for _, line := range h.lines {
				if line == "/v1/sessions/succeed" {
					t.Fatalf("excluded call attempted succession: %v", h.lines)
				}
			}
		})
	}
}
