package binding

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type fakeSuccessionProof struct {
	generation int
	execution  string
	err        error
}

type fakeSuccessionMemory struct {
	current     string
	currentOK   bool
	currentErr  error
	generation  int
	execution   string
	prepareErr  error
	proofs      []fakeSuccessionProof
	prepares    int
	advance     func(string, int, string) (int, bool, error)
	advanceCall int
}

func (m *fakeSuccessionMemory) CurrentSession(string, proc.Ancestor) (string, bool, error) {
	return m.current, m.currentOK, m.currentErr
}

func (m *fakeSuccessionMemory) PrepareSuccession(string, string, int, string, proc.Ancestor) (int, string, error) {
	m.prepares++
	if len(m.proofs) > 0 {
		proof := m.proofs[0]
		m.proofs = m.proofs[1:]
		return proof.generation, proof.execution, proof.err
	}
	return m.generation, m.execution, m.prepareErr
}

func (m *fakeSuccessionMemory) AdvanceGeneration(_ string, _ string, expectedExec string, expectedOld, newGen int, newExec string) (int, bool, error) {
	m.advanceCall++
	if m.advance != nil {
		return m.advance(expectedExec, expectedOld, newExec)
	}
	m.generation, m.execution = newGen, newExec
	return newGen, true, nil
}

func scriptedSuccessionResolver(h *tickHub, memory *fakeSuccessionMemory) *Resolver {
	r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
	r.Succession = memory
	r.ParentAlive = func() bool { return true }
	r.Recall = func(string, string) (int, int, error) { return memory.generation, 0, nil }
	return r
}

func TestSuccessionCurrentSessionDiscoveryAndUntrustedFallback(t *testing.T) {
	t.Run("witness error", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			if req.URL.Path != "/v1/bind" {
				t.Fatalf("witness error reached %s", req.URL.Path)
			}
			return refused("unbound")
		}}
		memory := &fakeSuccessionMemory{currentErr: errors.New("bad witness")}
		r := tickResolver(h.client(), "cursor", nil, proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"})
		r.Recall = func(string, string) (int, int, error) { return 0, 0, nil }
		r.Succession = memory
		if _, err := r.Resolve(t.Context(), ""); !IsCode(err, "lost") {
			t.Fatalf("witness error did not fail closed: %v", err)
		}
	})
	t.Run("no hook history keeps synthetic fallback", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/bind":
				return refused("unbound")
			case "/v1/sessions/outside":
				if body["generation"] != float64(0) || body["source"] != "mcp" || body["session_id"] != "mcp-42-100" {
					t.Fatalf("untrusted fallback request: %+v", body)
				}
				return http.StatusOK, map[string]any{"execution_id": "ex_fallback", "generation": 1}
			default:
				t.Fatalf("unexpected request: %s", req.URL.Path)
				return 0, nil
			}
		}}
		memory := &fakeSuccessionMemory{currentOK: false}
		r := tickResolver(h.client(), "cursor", nil, proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"})
		r.Recall = func(string, string) (int, int, error) { return 0, 0, nil }
		r.Succession = memory
		b, err := r.Resolve(t.Context(), "")
		if err != nil || b.ExecutionID != "ex_fallback" || b.SessionID != "mcp-42-100" || !r.own {
			t.Fatalf("untrusted fallback changed: bound=%+v own=%t err=%v", b, r.own, err)
		}
	})
}

func TestSuccessionEligibilityAndOlderHubFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*Resolver)
		attempted bool
	}{
		{"no parent check", func(r *Resolver) { r.ParentAlive = nil }, false},
		{"watch", func(r *Resolver) { r.HeartbeatSource = "watch" }, false},
		{"wake helper", func(r *Resolver) { r.wakeSession = "wake" }, false},
		{"dead parent", func(r *Resolver) { r.ParentAlive = func() bool { return false } }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				t.Fatalf("excluded caller reached %s", req.URL.Path)
				return 0, nil
			}}
			r := scriptedSuccessionResolver(h, &fakeSuccessionMemory{generation: 1, execution: "ex_one"})
			tc.configure(r)
			_, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1)
			if attempted != tc.attempted || attempted && !IsCode(err, "lost") {
				t.Fatalf("attempted=%t err=%v", attempted, err)
			}
		})
	}
	t.Run("older hub", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
			if req.URL.Path != "/v1/sessions/succeed" || body["prior_generation"] != float64(1) || body["prior_execution_id"] != "ex_one" {
				t.Fatalf("older-hub request: %s %+v", req.URL.Path, body)
			}
			return http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "unknown API route"}}
		}}
		r := scriptedSuccessionResolver(h, &fakeSuccessionMemory{generation: 1, execution: "ex_one"})
		if _, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1); !attempted || !IsCode(err, "lost") {
			t.Fatalf("older hub did not fail closed with lost: attempted=%t err=%v", attempted, err)
		}
		if len(h.lines) != 1 || h.lines[0] != "/v1/sessions/succeed" {
			t.Fatalf("older hub triggered another request: %v", h.lines)
		}
	})
	t.Run("prepare failure", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			t.Fatalf("prepare failure reached %s", req.URL.Path)
			return 0, nil
		}}
		r := scriptedSuccessionResolver(h, &fakeSuccessionMemory{prepareErr: errors.New("bad proof")})
		if _, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1); !attempted || !IsCode(err, "lost") {
			t.Fatalf("prepare failure: attempted=%t err=%v", attempted, err)
		}
	})
}

func TestSuccessionRung3AncestorReverification(t *testing.T) {
	agent := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	for _, tc := range []struct {
		name     string
		ancestor func() (proc.Ancestor, error)
		nilFunc  bool
	}{
		{name: "nil ancestor", nilFunc: true},
		{name: "lookup error", ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, errors.New("lookup") }},
		{name: "invalid process", ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }},
		{name: "Cursor IDE", ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor"}, nil }},
		{name: "PID reuse", ancestor: func() func() (proc.Ancestor, error) {
			calls := 0
			return func() (proc.Ancestor, error) {
				calls++
				if calls == 1 {
					return agent, nil
				}
				after := agent
				after.StartMS++
				return after, nil
			}
		}()},
		{name: "second lookup error", ancestor: func() func() (proc.Ancestor, error) {
			calls := 0
			return func() (proc.Ancestor, error) {
				calls++
				if calls == 1 {
					return agent, nil
				}
				return proc.Ancestor{}, errors.New("second lookup")
			}
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				t.Fatalf("invalid ancestor reached %s", req.URL.Path)
				return 0, nil
			}}
			memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one"}
			r := scriptedSuccessionResolver(h, memory)
			r.Client = "cursor"
			r.Ancestor = tc.ancestor
			if tc.nilFunc {
				r.Ancestor = nil
			}
			if _, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 3, 1); !attempted || !IsCode(err, "lost") {
				t.Fatalf("ancestor failure: attempted=%t err=%v", attempted, err)
			}
		})
	}
}

func TestSuccessionValidatesResponsesAndCASFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response map[string]any
		advance  func(string, int, string) (int, bool, error)
		recall   func(string, string) (int, int, error)
		bind     bool
	}{
		{name: "missing execution", response: map[string]any{}},
		{name: "wrong session", response: map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "other"}},
		{name: "wrong generation", response: map[string]any{"execution_id": "ex_two", "generation": 3, "session_id": "S"}},
		{name: "advance failure", response: map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}, advance: func(string, int, string) (int, bool, error) { return 0, false, errors.New("CAS") }},
		{name: "same generation different execution after CAS", response: map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}, advance: func(string, int, string) (int, bool, error) { return 2, false, nil }},
		{name: "advance mismatch without winner", response: map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}, advance: func(string, int, string) (int, bool, error) { return 0, false, nil }, bind: true},
		{name: "generation recall failure", response: map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}, recall: func(string, string) (int, int, error) { return -1, 0, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				switch req.URL.Path {
				case "/v1/sessions/succeed":
					return http.StatusOK, tc.response
				case "/v1/bind":
					return refused("unbound")
				default:
					t.Fatalf("unexpected request: %s", req.URL.Path)
					return 0, nil
				}
			}}
			memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one", advance: tc.advance}
			r := scriptedSuccessionResolver(h, memory)
			if tc.recall != nil {
				r.Recall = tc.recall
			}
			if _, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1); !attempted || !IsCode(err, "lost") {
				t.Fatalf("invalid response/CAS: attempted=%t err=%v", attempted, err)
			}
			if tc.bind && len(h.lines) != 2 {
				t.Fatalf("losing shim did not make one winner lookup: %v", h.lines)
			}
		})
	}
}

func TestSuccessionAdoptsValidWinnersAndRejectsOthers(t *testing.T) {
	validWinner := map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S", "match": "succession"}
	for _, tc := range []struct {
		name         string
		bind         map[string]any
		bindErr      bool
		gone         bool
		proofs       []fakeSuccessionProof
		advance      func(string, int, string) (int, bool, error)
		recall       func(string, string) (int, int, error)
		wantWinner   string
		wantBindOnly bool
	}{
		{name: "bind failure", bindErr: true},
		{name: "divergent next generation after refusal", bind: map[string]any{"execution_id": "ex_divergent", "generation": 2, "session_id": "S"}, advance: func(string, int, string) (int, bool, error) {
			t.Error("unconfirmed winner overwrote the predecessor proof")
			return 2, true, nil
		}},
		{name: "gone winner", bind: validWinner, gone: true},
		{name: "wrong session", bind: map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "other", "match": "succession"}},
		{name: "older winner", bind: map[string]any{"execution_id": "ex_old", "generation": 0, "session_id": "S", "match": "succession"}},
		{name: "same generation different execution", bind: map[string]any{"execution_id": "ex_other", "generation": 1, "session_id": "S", "match": "succession"}},
		{name: "advance failure", bind: validWinner, advance: func(string, int, string) (int, bool, error) { return 0, false, errors.New("CAS") }},
		{name: "winner proof failure", bind: validWinner, proofs: []fakeSuccessionProof{{generation: 1, execution: "ex_one"}, {err: errors.New("bad winner proof")}}},
		{name: "winner proof mismatch", bind: validWinner, proofs: []fakeSuccessionProof{{generation: 1, execution: "ex_one"}, {generation: 3, execution: "ex_two"}}},
		{name: "winner generation recall failure", bind: validWinner, recall: func(string, string) (int, int, error) { return -1, 0, nil }},
		{name: "later hook winner", bind: map[string]any{"execution_id": "ex_three", "generation": 3, "session_id": "S", "match": "succession"}, proofs: []fakeSuccessionProof{{generation: 1, execution: "ex_one"}, {generation: 3, execution: "ex_three"}}, wantWinner: "ex_three"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
				switch req.URL.Path {
				case "/v1/sessions/succeed":
					return refused("lost")
				case "/v1/bind":
					if tc.bindErr {
						return refused("unbound")
					}
					return http.StatusOK, tc.bind
				default:
					t.Fatalf("unexpected request: %s", req.URL.Path)
					return 0, nil
				}
			}}
			memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one", proofs: tc.proofs, advance: tc.advance}
			r := scriptedSuccessionResolver(h, memory)
			if tc.gone {
				r.gone = map[string]bool{"ex_two": true}
			}
			if tc.recall != nil {
				r.Recall = tc.recall
			}
			b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1)
			if !attempted {
				t.Fatal("succession was not attempted")
			}
			if tc.wantWinner != "" {
				if err != nil || b.ExecutionID != tc.wantWinner {
					t.Fatalf("winner was not adopted: bound=%+v err=%v", b, err)
				}
				return
			}
			if b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("invalid winner was adopted: bound=%+v err=%v", b, err)
			}
		})
	}
}

func TestSuccessionProcessWinnerAndAdvanceRace(t *testing.T) {
	t.Run("Rung 3 adopts process winner without session field", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/sessions/succeed":
				return refused("lost")
			case "/v1/bind":
				if body["client_pid"] != float64(42) || body["client_pid_start_ms"] != float64(100) {
					t.Fatalf("process winner lookup: %+v", body)
				}
				return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "match": "succession"}
			default:
				t.Fatalf("unexpected request: %s", req.URL.Path)
				return 0, nil
			}
		}}
		memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one", current: "S", currentOK: true}
		r := scriptedSuccessionResolver(h, memory)
		r.Client = "cursor"
		r.Ancestor = func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}, nil }
		b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 3, 1)
		if !attempted || err != nil || b.ExecutionID != "ex_two" || b.SessionID != "S" || b.Rung != 3 {
			t.Fatalf("process winner: bound=%+v attempted=%t err=%v", b, attempted, err)
		}
	})
	t.Run("successful response already committed locally", func(t *testing.T) {
		h := &tickHub{reply: func(_ *http.Request, _ map[string]any) (int, any) {
			return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}
		}}
		memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one"}
		memory.advance = func(string, int, string) (int, bool, error) {
			memory.generation, memory.execution = 2, "ex_two"
			return 2, false, nil
		}
		r := scriptedSuccessionResolver(h, memory)
		b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1)
		if !attempted || err != nil || b.ExecutionID != "ex_two" || memory.prepares < 2 {
			t.Fatalf("same exact proof after CAS: bound=%+v attempted=%t err=%v proof reads=%d", b, attempted, err, memory.prepares)
		}
	})
	t.Run("successful response overtaken by later hook winner", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/sessions/succeed":
				return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}
			case "/v1/bind":
				return http.StatusOK, map[string]any{"execution_id": "ex_three", "generation": 3, "session_id": "S", "match": "succession"}
			default:
				t.Fatalf("unexpected request: %s", req.URL.Path)
				return 0, nil
			}
		}}
		calls := 0
		memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one", proofs: []fakeSuccessionProof{{generation: 1, execution: "ex_one"}, {generation: 3, execution: "ex_three"}}}
		memory.advance = func(string, int, string) (int, bool, error) {
			calls++
			if calls == 1 {
				return 3, false, nil
			}
			return 0, false, errors.New("unexpected second advance")
		}
		r := scriptedSuccessionResolver(h, memory)
		b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1)
		if !attempted || err != nil || b.ExecutionID != "ex_three" {
			t.Fatalf("later winner after committed response: bound=%+v attempted=%t err=%v", b, attempted, err)
		}
	})
	t.Run("advance mismatch adoption failure", func(t *testing.T) {
		h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
			switch req.URL.Path {
			case "/v1/sessions/succeed":
				return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}
			case "/v1/bind":
				return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S", "match": "succession"}
			default:
				t.Fatalf("unexpected request: %s", req.URL.Path)
				return 0, nil
			}
		}}
		calls := 0
		memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one"}
		memory.advance = func(string, int, string) (int, bool, error) {
			calls++
			if calls == 1 {
				return 0, false, nil
			}
			return 0, false, errors.New("adoption CAS")
		}
		r := scriptedSuccessionResolver(h, memory)
		if _, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1); !attempted || !IsCode(err, "lost") {
			t.Fatalf("adoption CAS failure: attempted=%t err=%v", attempted, err)
		}
	})
}

func TestSuccessionFollowsProofAdvancedDuringReattach(t *testing.T) {
	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		if body["prior_generation"] != float64(2) || body["prior_execution_id"] != "ex_hook" {
			t.Fatalf("request did not follow the advanced hook proof: %+v", body)
		}
		if req.URL.Path == "/v1/sessions/succeed" {
			return refused("lost")
		}
		return http.StatusOK, map[string]any{"execution_id": "ex_hook", "generation": 2, "session_id": "S", "match": "succession"}
	}}
	memory := &fakeSuccessionMemory{generation: 2, execution: "ex_hook"}
	r := scriptedSuccessionResolver(h, memory)
	b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1)
	if !attempted || err != nil || b.ExecutionID != "ex_hook" || memory.advanceCall != 0 {
		t.Fatalf("already-proven winner: bound=%+v attempted=%t err=%v writes=%d", b, attempted, err, memory.advanceCall)
	}
}

func TestSuccessionRetainedRung3UsesProcessIdentity(t *testing.T) {
	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		if req.URL.Path != "/v1/sessions/succeed" || body["client_pid"] != float64(42) || body["client_pid_start_ms"] != float64(100) || body["prior_execution_id"] != "ex_one" {
			t.Fatalf("retained Rung 3 request: %s %+v", req.URL.Path, body)
		}
		return http.StatusOK, map[string]any{"execution_id": "ex_two", "generation": 2, "session_id": "S"}
	}}
	memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one", current: "S", currentOK: true}
	r := scriptedSuccessionResolver(h, memory)
	r.Client = "cursor"
	r.Ancestor = func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}, nil }
	r.last = Bound{SessionID: "S", Rung: 3, ExecutionID: "ex_one", Generation: 1, HookSeen: true}
	b, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1)
	if !attempted || err != nil || b.ExecutionID != "ex_two" || b.Rung != 2 {
		t.Fatalf("retained Rung 3 succession: bound=%+v attempted=%t err=%v", b, attempted, err)
	}
}

func TestSuccessionNetworkFailureIsLost(t *testing.T) {
	client := &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: adoptionTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}}
	memory := &fakeSuccessionMemory{generation: 1, execution: "ex_one"}
	r := &Resolver{Hub: client, Client: "claude", Host: "host", Getenv: func(string) string { return "" }, Succession: memory, ParentAlive: func() bool { return true }}
	r.Recall = func(string, string) (int, int, error) { return memory.generation, 0, nil }
	if _, attempted, err := r.succeedAfterLostHeld(t.Context(), "S", 2, 1); !attempted || !IsCode(err, "lost") {
		t.Fatalf("network failure did not become lost: attempted=%t err=%v", attempted, err)
	}
}
