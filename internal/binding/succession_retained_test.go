package binding

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestRetainedProofAdoptsWinnerBeforeFirstProofRead(t *testing.T) {
	f := newSuccessionBindingFixture(t, "claude", "SessionStart", "S", proc.Ancestor{})
	b := f.resolver("claude", "S", proc.Ancestor{})
	first, err := b.Resolve(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	f.hub.clock.Advance(16 * time.Minute)
	a := f.resolver("claude", "S", proc.Ancestor{})
	winner, err := a.Resolve(t.Context(), "")
	if err != nil || winner.Generation != 2 {
		t.Fatalf("shim A did not publish G+1: bound=%+v err=%v", winner, err)
	}

	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		switch req.URL.Path {
		case "/v1/bind":
			if body["prior_generation"] == nil {
				return refused("unbound")
			}
			if body["prior_generation"] != float64(1) || body["prior_execution_id"] != first.ExecutionID {
				t.Fatalf("winner lookup did not use retained G proof: %+v", body)
			}
			return http.StatusOK, map[string]any{"execution_id": winner.ExecutionID, "generation": 2, "session_id": "S", "match": "succession"}
		case "/v1/sessions":
			if body["generation"] != float64(2) {
				t.Fatalf("B did not first try the advanced local proof: %+v", body)
			}
			return refused("lost")
		case "/v1/sessions/succeed":
			t.Fatal("B attempted another succession instead of adopting A")
			return 0, nil
		default:
			t.Fatalf("unexpected request: %s", req.URL.Path)
			return 0, nil
		}
	}}
	b.bound = Bound{}
	b.Hub = h.client()
	got, err := b.Resolve(t.Context(), "")
	if err != nil || got.ExecutionID != winner.ExecutionID || got.Generation != 2 {
		t.Fatalf("B did not adopt A's winner: bound=%+v err=%v", got, err)
	}
	h.expect(t, "bind session_id=S", "/v1/sessions", "bind session_id=S")
}

func TestRetainedProofWinnerLookupFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		adoptError bool
	}{
		{name: "no confirmed winner"},
		{name: "winner verification fails", adoptError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				if req.URL.Path != "/v1/bind" {
					t.Fatalf("unexpected request: %s", req.URL.Path)
				}
				if !tc.adoptError {
					return refused("unbound")
				}
				return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S", "match": "succession"}
			}}
			memory := &fakeSuccessionMemory{generation: 2, execution: "ex_two"}
			if tc.adoptError {
				memory.advance = func(string, int, string) (int, bool, error) { return 0, false, os.ErrInvalid }
			}
			r := scriptedSuccessionResolver(h, memory)
			r.last = Bound{SessionID: "S", Rung: 2, Generation: 1, ExecutionID: "ex_one", HookSeen: true}
			if b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 2); !attempted || b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("invalid retained-winner result: bound=%+v attempted=%t err=%v", b, attempted, err)
			}
			if memory.prepares != 0 {
				t.Fatalf("winner lookup fell through to a new succession proof read: %d", memory.prepares)
			}
			h.expect(t, "bind session_id=S")
		})
	}
}
