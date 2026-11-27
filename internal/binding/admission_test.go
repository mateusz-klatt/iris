package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

const (
	admissionPriorID = "ex_11111111111111111111111111111111"
	admissionChildID = "ex_22222222222222222222222222222222"
	admissionOtherID = "ex_33333333333333333333333333333333"
)

func bindingEvidenceFiles(t *testing.T) (*hook.Sessions, string, string) {
	t.Helper()
	return bindingEvidenceFilesFor(t, "claude")
}

func bindingEvidenceFilesFor(t *testing.T, client string) (*hook.Sessions, string, string) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "hub.token")
	sessions := hook.NewSessions(token)
	if err := sessions.Protect(); err != nil {
		t.Fatal(err)
	}
	snap, err := sessions.InspectEvidence(client, "native", time.Time{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	return sessions, snap.BasePath, token
}

func writeBindingEvidence(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func shimBindingEvidence() sessionproof.Record {
	return sessionproof.Record{
		Version:          sessionproof.FormatVersion,
		Origin:           sessionproof.OriginShim,
		Generation:       3,
		ExecutionID:      admissionChildID,
		PriorGeneration:  2,
		PriorExecutionID: admissionPriorID,
	}
}

func TestCandidateAdmissionRequiresNativeLiveShimLineage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opted       bool
		eligible    bool
		parentAlive bool
		record      string
		ended       string
		want        error
	}{
		{name: "matching shim record", opted: true, eligible: true, parentAlive: true, record: "shim", ended: "1"},
		{name: "absent record", opted: true, eligible: true, parentAlive: true, want: errGenerationFloor},
		{name: "hook record", opted: true, eligible: true, parentAlive: true, record: "hook", want: errGenerationFloor},
		{name: "higher exact record", opted: true, eligible: true, parentAlive: true, record: "higher", want: errGenerationFloor},
		{name: "conflicting child", opted: true, eligible: true, parentAlive: true, record: "other child", want: errGenerationFloor},
		{name: "conflicting predecessor", opted: true, eligible: true, parentAlive: true, record: "other prior", want: errGenerationFloor},
		{name: "ended predecessor", opted: true, eligible: true, parentAlive: true, record: "shim", ended: "2", want: errGenerationFloor},
		{name: "ended child", opted: true, eligible: true, parentAlive: true, record: "shim", ended: "3", want: errGenerationFloor},
		{name: "foreign lookup", opted: true, parentAlive: true, record: "shim", want: errGenerationFloor},
		{name: "dead parent", opted: true, eligible: true, record: "shim", want: errGenerationFloor},
		{name: "disabled", eligible: true, parentAlive: true, record: "shim", want: errGenerationFloor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, prefix, _ := bindingEvidenceFiles(t)
			record := shimBindingEvidence()
			switch tc.record {
			case "hook":
				record.Origin, record.PriorGeneration, record.PriorExecutionID = sessionproof.OriginHook, 0, ""
			case "higher":
				record.Generation, record.PriorGeneration = 4, 3
			case "other child":
				record.ExecutionID = admissionOtherID
			case "other prior":
				record.PriorExecutionID = admissionOtherID
			}
			if tc.record != "" {
				writeAdmissionRecord(t, prefix, record)
			}
			if tc.ended != "" {
				writeBindingEvidence(t, prefix+".ended", tc.ended)
			}
			r := &Resolver{Client: "claude", Evidence: sessions, ParentAlive: func() bool { return tc.parentAlive }}
			b := Bound{ExecutionID: admissionChildID, Generation: 3, SessionID: "native", Rung: 2, Classified: true,
				Class: hubclient.Classification{Kind: hubclient.CandidateShimSuccession, PriorGeneration: 2, PriorExecutionID: admissionPriorID}}
			err := r.admitCandidate(t.Context(), b, 0, true, tc.eligible, tc.opted, sessionproof.Snapshot{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("admission error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCandidateAdmissionKeepsEarlierFloorsAndExactIdentityAfterPrune(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation string
		ended      string
		exact      bool
	}{
		{name: "higher generation", generation: "4"},
		{name: "end marker", ended: "3"},
		{name: "forked exact record", exact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, prefix, _ := bindingEvidenceFiles(t)
			path, data := prefix, tc.generation
			if tc.ended != "" {
				path, data = prefix+".ended", tc.ended
			}
			if tc.exact {
				record := shimBindingEvidence()
				record.ExecutionID = admissionOtherID
				raw, err := sessionproof.EncodeRecord(record)
				if err != nil {
					t.Fatal(err)
				}
				path, data = prefix+".execution", string(raw)
			}
			writeBindingEvidence(t, path, data)
			r := &Resolver{Client: "claude", Evidence: sessions}
			snap, err := r.inspectEvidence(t.Context(), "native", true)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			b := Bound{ExecutionID: admissionChildID, Generation: 3, SessionID: "native", Rung: 2, HookSeen: true}
			if err := r.admitCandidate(t.Context(), b, 0, true, false, true, snap); !errors.Is(err, errGenerationFloor) {
				t.Fatalf("proof retired during the attempt did not veto stale candidate: %v", err)
			}
			if err := r.admitCandidate(t.Context(), b, 0, true, false, true, sessionproof.Snapshot{}); err != nil {
				t.Fatalf("a new attempt inherited old proof: %v", err)
			}
		})
	}
}

func TestCandidateAdmissionRefusesEvidenceLockContentionAndNamesDamage(t *testing.T) {
	sessions, prefix, token := bindingEvidenceFiles(t)
	r := &Resolver{Client: "claude", Evidence: sessions}
	b := Bound{ExecutionID: admissionChildID, Generation: 3, SessionID: "native", Rung: 2, HookSeen: true}
	unlock, err := sessions.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = r.admitCandidate(t.Context(), b, 0, true, true, true, sessionproof.Snapshot{})
	unlock()
	if !errors.Is(err, errMalformedGeneration) {
		t.Fatalf("admitted while evidence lock was unavailable: %v", err)
	}
	writeBindingEvidence(t, prefix+".execution", "corrupt")
	if err := r.admitCandidate(t.Context(), b, 0, true, true, true, sessionproof.Snapshot{}); !errors.Is(err, errMalformedGeneration) {
		t.Fatalf("admitted with damaged exact record: %v", err)
	}
	assertBindingObservation(t, token, "claude", hook.DamagedGenerationProof+" at "+prefix+".execution")
	if err := r.admitCandidate(t.Context(), b, 0, true, true, false, sessionproof.Snapshot{}); err != nil {
		t.Fatalf("disabled succession vetoed damaged exact record: %v", err)
	}
}

func admissionWireCandidate(hookless bool) map[string]any {
	source, proof := "hook", "stale_retry"
	if hookless {
		source, proof = "shim", "shim_succession"
	}
	return map[string]any{
		"execution_id": admissionChildID, "generation": 3, "session_id": "native",
		"capabilities": []string{hubclient.SuccessionProofCapability}, "hook_seen": !hookless,
		"creation_source": source, "creation_proof": proof, "prior_execution_id": admissionPriorID,
	}
}

func optedAdmissionResolver(hub *hubclient.Client, evidence *hook.Sessions) *Resolver {
	r := tickResolver(hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "native"}, proc.Ancestor{})
	r.Grace = 0
	r.Evidence = evidence
	r.SuccessionEnabled = func(context.Context) bool { return true }
	r.ParentAlive = func() bool { return true }
	return r
}

func TestResolverDiscardsIncompleteClassifiedRefreshWithoutRecovery(t *testing.T) {
	sessions, _, _ := bindingEvidenceFiles(t)
	broken := false
	h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
		if req.URL.Path != "/v1/bind" || body["succession_proof"] != true {
			t.Fatalf("unexpected request %s: %+v", req.URL.Path, body)
		}
		reply := admissionWireCandidate(false)
		if broken {
			delete(reply, "prior_execution_id")
			delete(reply, "creation_source")
			delete(reply, "creation_proof")
		}
		return http.StatusOK, reply
	}}
	r := optedAdmissionResolver(h.client(), sessions)
	b, err := r.Resolve(t.Context(), "")
	if err != nil || !b.Classified || !b.HookSeen {
		t.Fatalf("initial ordinary bind: %+v, %v", b, err)
	}
	broken = true
	if fresh, err := r.Resolve(t.Context(), ""); fresh != (Bound{}) || !errors.Is(err, errInvalidCandidate) {
		t.Fatalf("M1-shaped reply was admitted or retried: %+v, %v", fresh, err)
	}
	if r.bound != b || r.last != b || r.Lost() || len(r.gone) != 0 {
		t.Fatalf("unusable reply changed the admitted state: bound=%+v last=%+v gone=%v", r.bound, r.last, r.gone)
	}
	h.expect(t, "bind session_id=native", "bind execution="+admissionChildID)
}

func TestResolverSkipsDispatchUntilDamagedEvidenceIsRepaired(t *testing.T) {
	for _, tc := range []struct {
		name, client string
		cached       bool
	}{
		{name: "fresh native shim", client: "claude"},
		{name: "fresh Codex root", client: "codex"},
		{name: "cached native shim", client: "claude", cached: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, prefix, _ := bindingEvidenceFilesFor(t, tc.client)
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				if req.URL.Path != "/v1/bind" || body["succession_proof"] != true {
					t.Fatalf("unexpected request %s: %+v", req.URL.Path, body)
				}
				return http.StatusOK, admissionWireCandidate(false)
			}}
			r := optedAdmissionResolver(h.client(), sessions)
			r.Client = tc.client
			resolve := func() (Bound, error) {
				if tc.client == "codex" {
					b, _, err := r.ResolveCall(t.Context(), "", "native", "native")
					return b, err
				}
				return r.Resolve(t.Context(), "")
			}
			var before Bound
			var trace []string
			if tc.cached {
				var err error
				before, err = resolve()
				if err != nil || before.ExecutionID != admissionChildID {
					t.Fatalf("initial bind: %+v, %v", before, err)
				}
				trace = append(trace, "bind session_id=native")
			}
			writeBindingEvidence(t, prefix+".execution", "damaged")
			if fresh, err := resolve(); fresh != (Bound{}) || !errors.Is(err, errMalformedGeneration) {
				t.Fatalf("damaged proof supplied a binding: %+v, %v", fresh, err)
			}
			if r.bound != before || r.Lost() || len(r.gone) != 0 {
				t.Fatalf("file damage changed the last usable binding: bound=%+v gone=%v", r.bound, r.gone)
			}
			h.expect(t, trace...)
			writeAdmissionRecord(t, prefix, sessionproof.Record{
				Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook,
				Generation: 3, ExecutionID: admissionChildID,
			})
			if fresh, err := resolve(); err != nil || fresh.ExecutionID != admissionChildID || !fresh.HookSeen {
				t.Fatalf("repaired evidence still refused the live binding: %+v, %v", fresh, err)
			}
			if tc.cached {
				trace = append(trace, "bind execution="+admissionChildID)
			} else {
				trace = append(trace, "bind session_id=native")
			}
			h.expect(t, trace...)
		})
	}
}

func TestResolverDiscardsHeartbeatWhenEvidenceChangesInFlight(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hookless bool
		change   func(*testing.T, string)
		want     error
	}{
		{name: "generation advances", change: func(t *testing.T, prefix string) {
			writeBindingEvidence(t, prefix, "4")
		}, want: errGenerationFloor},
		{name: "conflicting exact pair", change: func(t *testing.T, prefix string) {
			record := shimBindingEvidence()
			record.ExecutionID = admissionOtherID
			writeAdmissionRecord(t, prefix, record)
		}, want: errGenerationFloor},
		{name: "hookless proof disappears", hookless: true, change: func(t *testing.T, prefix string) {
			if err := os.Remove(prefix + ".execution"); err != nil {
				t.Fatal(err)
			}
		}, want: errGenerationFloor},
		{name: "exact proof damaged", change: func(t *testing.T, prefix string) {
			writeBindingEvidence(t, prefix+".execution", "damaged")
		}, want: errMalformedGeneration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, prefix, _ := bindingEvidenceFiles(t)
			writeBindingEvidence(t, prefix, "3")
			writeAdmissionRecord(t, prefix, shimBindingEvidence())
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				if req.URL.Path == "/v1/executions/"+admissionChildID+"/heartbeat" {
					if body["succession_proof"] != true || body["generation"] != float64(3) || body["source"] != "shim" {
						t.Fatalf("incorrect opted heartbeat: %+v", body)
					}
					tc.change(t, prefix)
				}
				reply := admissionWireCandidate(tc.hookless)
				reply["repository_id"] = req.URL.Path
				return http.StatusOK, reply
			}}
			r := optedAdmissionResolver(h.client(), sessions)
			b, err := r.Resolve(t.Context(), "")
			if err != nil || b.Class.IsHooklessSuccession() != tc.hookless {
				t.Fatalf("initial bind: %+v, %v", b, err)
			}
			if err := r.Heartbeat(t.Context(), "shim"); !errors.Is(err, tc.want) {
				t.Fatalf("admitted heartbeat after evidence changed: %v", err)
			}
			if r.bound != b || r.last != b || r.Lost() || len(r.gone) != 0 {
				t.Fatalf("local veto changed authority: bound=%+v last=%+v gone=%v", r.bound, r.last, r.gone)
			}
			writeBindingEvidence(t, prefix, "3")
			writeAdmissionRecord(t, prefix, shimBindingEvidence())
			if fresh, err := r.Resolve(t.Context(), ""); fresh != b || err != nil {
				t.Fatalf("restored proof could not admit the same pair: %+v, %v", fresh, err)
			}
			h.expect(t, "bind session_id=native", "beat "+admissionChildID+" 3 shim", "bind execution="+admissionChildID)
		})
	}
}

func writeAdmissionRecord(t *testing.T, prefix string, record sessionproof.Record) {
	t.Helper()
	data, err := sessionproof.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	writeBindingEvidence(t, prefix+".execution", string(data))
}

func TestResolverUnknownHeartbeatOutcomeRetainsAdmittedPair(t *testing.T) {
	sessions, prefix, _ := bindingEvidenceFiles(t)
	writeAdmissionRecord(t, prefix, shimBindingEvidence())
	var binds, beats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/bind":
			binds.Add(1)
		case "/v1/executions/" + admissionChildID + "/heartbeat":
			if beats.Add(1) == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		default:
			t.Errorf("unexpected recovery request %s", req.URL.Path)
			return
		}
		_ = json.NewEncoder(w).Encode(admissionWireCandidate(true))
	}))
	t.Cleanup(srv.Close)
	r := optedAdmissionResolver(&hubclient.Client{Config: config{srv.URL}}, sessions)
	b, err := r.Resolve(t.Context(), "")
	if err != nil || !b.Class.IsHooklessSuccession() {
		t.Fatalf("initial hookless bind: %+v, %v", b, err)
	}
	if err := r.Heartbeat(t.Context(), "shim"); err == nil || IsCode(err, "lost") {
		t.Fatalf("unknown heartbeat became a loss or success: %v", err)
	}
	if r.bound != b || r.last != b || r.Lost() || len(r.gone) != 0 {
		t.Fatalf("unknown outcome discarded a still-admissible pair: bound=%+v last=%+v gone=%v", r.bound, r.last, r.gone)
	}
	if err := r.Heartbeat(t.Context(), "shim"); err != nil || r.bound != b || binds.Load() != 1 || beats.Load() != 2 {
		t.Fatalf("same pair did not renew on the next tick: bound=%+v binds=%d beats=%d err=%v", r.bound, binds.Load(), beats.Load(), err)
	}
}
