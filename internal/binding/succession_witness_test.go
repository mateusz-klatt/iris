package binding

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestSuccessionRechecksTheProcessWitnessAfterHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adopt  bool
		change func(string) error
	}{
		{"clear while succeeding", false, func(path string) error { return os.WriteFile(path, []byte("S2\n"), 0o600) }},
		{"missing after success", false, os.Remove},
		{"damaged after success", false, func(path string) error { return os.WriteFile(path, nil, 0o600) }},
		{"clear while adopting", true, func(path string) error { return os.WriteFile(path, []byte("S2\n"), 0o600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ancestor := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
			f := newSuccessionBindingFixture(t, "cursor", "sessionStart", "S1", ancestor)
			prior := executionRows(t, f.hub.file)[0].id
			witness := filepath.Join(f.sessions.Dir(), fmt.Sprintf("proc-%d-%d", ancestor.PID, ancestor.StartMS))
			changed := false
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				if tc.adopt && req.URL.Path == "/v1/sessions/succeed" {
					return refused("lost")
				}
				if !changed {
					if err := tc.change(witness); err != nil {
						t.Fatal(err)
					}
					changed = true
				}
				return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S1", "match": "succession"}
			}}
			r := f.resolver("cursor", "", ancestor)
			r.Hub = h.client()
			b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S1", 3, 1)
			if !attempted || b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("the shim accepted the stale reply: bound=%+v attempted=%t err=%v", b, attempted, err)
			}
			if tc.adopt {
				h.expect(t, "/v1/sessions/succeed", "bind client_pid=42 client_pid_start_ms=100")
			} else {
				h.expect(t, "/v1/sessions/succeed")
			}
			generation, id, proofErr := f.sessions.PrepareSuccession("cursor", "S1", 1, prior, proc.Ancestor{})
			if generation != 1 || id != prior || proofErr != nil {
				t.Fatalf("the stale reply changed local proof: generation=%d id=%q err=%v", generation, id, proofErr)
			}
			if b, err := r.Resolve(t.Context(), ""); b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("the next process lookup accepted the rejected successor: bound=%+v err=%v", b, err)
			}
			stale := Bound{ExecutionID: "ex_two", Generation: 2, SessionID: "S1", Rung: 3}
			r = f.resolver("cursor", "", ancestor)
			r.Hub, r.bound = h.client(), stale
			if b, err := r.Resolve(t.Context(), ""); b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("refresh accepted the rejected successor: bound=%+v err=%v", b, err)
			}
			r = f.resolver("cursor", "", ancestor)
			r.Hub, r.bound = h.client(), stale
			beforeHeartbeat := len(h.lines)
			if err := r.Heartbeat(t.Context(), "shim"); !IsCode(err, "lost") {
				t.Fatalf("renewal accepted the rejected successor: %v", err)
			}
			if len(h.lines) != beforeHeartbeat {
				t.Fatalf("renewal reached the hub after the witness changed: %v", h.lines[beforeHeartbeat:])
			}
		})
	}
}

func TestSuccessionRejectsWitnessChangeDuringProofAdvance(t *testing.T) {
	hub := &tickHub{reply: func(_ *http.Request, _ map[string]any) (int, any) {
		return found("ex_two", 2, "S1")
	}}
	memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one", current: "S1", currentOK: true}
	memory.advance = func(string, int, string) (int, bool, error) {
		memory.generation, memory.execution, memory.current = 2, "ex_two", "S2"
		return 2, true, nil
	}
	r := scriptedSuccessionResolver(hub, memory)
	r.Client = "cursor"
	r.Ancestor = func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}, nil
	}
	b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S1", 3, 1)
	if !attempted || memory.advanceCall != 1 || b.ExecutionID != "" || !IsCode(err, "lost") {
		t.Fatalf("the shim accepted the stale binding after CAS: bound=%+v attempted=%t err=%v", b, attempted, err)
	}
	hub.expect(t, "/v1/sessions/succeed")
}
