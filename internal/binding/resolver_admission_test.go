package binding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
)

func enableResolverSuccession(context.Context) bool { return true }

func TestOptedCodexRootRenewsHookBindingAcrossRestart(t *testing.T) {
	h := newLiveHub(t)
	reg, err := h.s.Call(t.Context(), "sessions", store.Request{
		Client: "codex", SessionID: "native", Host: "host", Cwd: t.TempDir(), Source: "hook", SingleSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions, _, _ := bindingEvidenceFiles(t)
	r := New(h.client(), "codex")
	r.Host, r.Evidence, r.SuccessionEnabled = "host", sessions, enableResolverSuccession
	first, primary, err := r.ResolveCall(t.Context(), "", "native", "native")
	if err != nil || !primary || !first.Classified || !first.HookSeen || first.Class.Kind != hubclient.CandidateOrdinary || first.ExecutionID != reg["execution_id"] {
		t.Fatalf("root did not admit the ordinary hook: %+v, primary %t, %v", first, primary, err)
	}
	if target, ok := r.Primary(); !ok || target.Bound != first {
		t.Fatalf("hook binding did not arm the primary: %+v, %t", target, ok)
	}
	if after, err := r.RenewPrimary(t.Context(), first); err != nil || after != first {
		t.Fatalf("primary heartbeat lost the binding: %+v, %v", after, err)
	}
	h.restart(t)
	if after, _, err := r.ResolveCall(t.Context(), "", "native", "native"); err != nil || after != first {
		t.Fatalf("Codex root did not recover the same classified pair: %+v, %v", after, err)
	}
}

func TestOptedCodexRootRequiresPublishedShimChildLineage(t *testing.T) {
	h := newRestartableBindingHub(t)
	req := store.Request{Client: "codex", SessionID: "native", Host: "host", Cwd: t.TempDir(), Source: "hook"}
	prior, err := h.store.Call(t.Context(), "sessions", req)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(16 * time.Minute)
	priorID := prior["execution_id"].(string)
	child, err := h.store.Call(t.Context(), "succeed", store.Request{
		Client: req.Client, SessionID: req.SessionID, Host: req.Host, Source: "shim", PriorExecutionID: priorID, PriorGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := child["execution_id"].(string)
	sessions, _, _ := bindingEvidenceFiles(t)
	snap, err := sessions.InspectEvidence(req.Client, req.SessionID, time.Time{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	r := New(&hubclient.Client{Config: config{h.srv.URL}}, req.Client)
	r.Host, r.Evidence, r.SuccessionEnabled, r.Grace = req.Host, sessions, enableResolverSuccession, 0
	r.ParentAlive = func() bool { return true }
	writeRecord := func(record sessionproof.Record) {
		t.Helper()
		raw, err := sessionproof.EncodeRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		writeBindingEvidence(t, snap.BasePath+".execution", string(raw))
	}
	resolve := func() (Bound, error) {
		b, _, err := r.ResolveCall(t.Context(), "", req.SessionID, req.SessionID)
		return b, err
	}
	record := sessionproof.Record{Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, ExecutionID: childID, Generation: 2}
	writeRecord(record)
	if b, err := resolve(); b != (Bound{}) || !errors.Is(err, errGenerationFloor) || len(r.gone) != 0 {
		t.Fatalf("hook-origin record admitted a hookless child: %+v, %v, gone %v", b, err, r.gone)
	}
	record.Origin, record.PriorExecutionID, record.PriorGeneration = sessionproof.OriginShim, priorID, 1
	writeRecord(record)
	b, err := resolve()
	if err != nil || b.ExecutionID != childID || !b.Class.IsHooklessSuccession() || b.HookSeen {
		t.Fatalf("published shim lineage did not admit the child: %+v, %v", b, err)
	}
	if _, ok := r.Primary(); ok {
		t.Fatal("hookless child armed the primary")
	}
	if after, err := r.RenewPrimary(t.Context(), b); err != nil || after != b {
		t.Fatalf("admitted hookless child could not renew: %+v, %v", after, err)
	}
	r.SuccessionEnabled = nil
	if after, err := resolve(); after != (Bound{}) || !IsCode(err, "lost") || len(r.gone) != 0 || r.Lost() {
		t.Fatalf("OFF accepted or marked the hookless child gone: %+v, %v, gone %v", after, err, r.gone)
	}
	r.SuccessionEnabled = enableResolverSuccession
	if after, err := resolve(); err != nil || after != b {
		t.Fatalf("turning succession back on could not recover the admitted child: %+v, %v", after, err)
	}
	if _, err := h.store.Call(t.Context(), "sessions", req); err != nil {
		t.Fatal(err)
	}
	if after, err := resolve(); err != nil || after.ExecutionID != b.ExecutionID || !after.HookSeen {
		t.Fatalf("real hook did not restore primary eligibility: %+v, %v", after, err)
	}
	if target, ok := r.Primary(); !ok || target.Bound.ExecutionID != b.ExecutionID {
		t.Fatalf("hooked child could not arm the primary: %+v, %t", target, ok)
	}
}

func TestUnoptedRecoveryValidatesCompleteClassAndPreservesItOnLegacyHeartbeat(t *testing.T) {
	const full = `{"execution_id":"ex_recovered","generation":2,"session_id":"native","hook_seen":true,"capabilities":["succession_proof"],"creation_source":"hook","creation_proof":"absent","prior_execution_id":"ex_old"}`
	for _, heartbeat := range []string{`{"lease_ms":60000}`, `{"execution_id":"ex_recovered","generation":2,"session_id":"native","repository_id":"rp_new"}`} {
		t.Run(heartbeat, func(t *testing.T) {
			var mu sync.Mutex
			next := []string{"/v1/bind", "/v1/sessions", "/v1/executions/ex_recovered/heartbeat", "/v1/bind"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				raw, err := io.ReadAll(req.Body)
				var fields map[string]json.RawMessage
				if err != nil || json.Unmarshal(raw, &fields) != nil || fields["succession_proof"] != nil || len(next) == 0 || req.URL.Path != next[0] {
					t.Errorf("unexpected OFF request %s: %s, %v (next %v)", req.URL.Path, raw, err, next)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				next = next[1:]
				switch req.URL.Path {
				case "/v1/sessions":
					if string(fields["generation"]) != "2" || string(fields["source"]) != `"shim"` {
						t.Errorf("registration lost its generation proof: %s", raw)
					}
					_, _ = io.WriteString(w, full)
				case "/v1/bind":
					if len(next) == 0 {
						_, _ = io.WriteString(w, full)
						return
					}
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"error":{"code":"unbound"}}`)
				default:
					if string(raw) != `{"generation":2,"source":"shim"}` {
						t.Errorf("heartbeat changed OFF wire bytes: %s", raw)
					}
					_, _ = io.WriteString(w, heartbeat)
				}
			}))
			defer srv.Close()
			sessions, prefix, _ := bindingEvidenceFiles(t)
			writeBindingEvidence(t, prefix, "2")
			r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "claude", Host: "host", CWD: t.TempDir(),
				Getenv: func(key string) string {
					if key == "CLAUDE_CODE_SESSION_ID" {
						return "native"
					}
					return ""
				}, Evidence: sessions, Recall: sessions.RecallBindingGeneration}
			first, err := r.Resolve(t.Context(), "")
			if err != nil || !first.Classified || first.Class.Kind != hubclient.CandidateOrdinary || !first.HookSeen {
				t.Fatalf("unopted registration lost its classification: %+v, %v", first, err)
			}
			if after, err := r.RenewPrimary(t.Context(), first); err != nil || after.Class != first.Class || !after.HookSeen || !after.Classified {
				t.Fatalf("legacy heartbeat erased classification: %+v, %v", after, err)
			}
			after, err := r.Resolve(t.Context(), "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil || after != first || len(next) != 0 {
				t.Fatalf("complete unopted bind did not revalidate the pair: %+v, %v, remaining %v", after, err, next)
			}
		})
	}
}

func TestOptedRefreshValidatesRecoveredLegacyPair(t *testing.T) {
	old := Bound{ExecutionID: "ex_root", Generation: 2, SessionID: "native", Rung: 2, HookSeen: true}
	for _, tc := range []struct {
		name      string
		bind      string
		heartbeat string
		followup  string
		want      error
	}{
		{name: "older hub confirms the same hooked pair", heartbeat: `{"lease_ms":60000}`,
			followup: `{"execution_id":"ex_root","generation":2,"hook_seen":true}`},
		{name: "older hub does not confirm hook visibility", heartbeat: `{"lease_ms":60000}`,
			followup: `{"execution_id":"ex_root","generation":2}`, want: errInvalidCandidate},
		{name: "heartbeat names a different generation", heartbeat: `{"execution_id":"ex_root","generation":1}`, want: errInvalidCandidate},
		{name: "bind names a different generation", bind: `{"execution_id":"ex_root","generation":3}`, want: errInvalidCandidate},
		{name: "bind names a different execution", bind: `{"execution_id":"ex_other","generation":2}`, want: errInvalidCandidate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, req.URL.Path)
				if req.URL.Path == "/v1/executions/"+old.ExecutionID+"/heartbeat" && tc.heartbeat != "" {
					_, _ = io.WriteString(w, tc.heartbeat)
					return
				}
				if req.URL.Path != "/v1/bind" {
					t.Errorf("unexpected recovery request %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch {
				case len(calls) == 1 && tc.bind != "":
					_, _ = io.WriteString(w, tc.bind)
				case len(calls) == 1:
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"error":{"code":"lost"}}`)
				case len(calls) == 3 && tc.followup != "":
					_, _ = io.WriteString(w, tc.followup)
				default:
					t.Errorf("unexpected follow-up bind, calls: %v", calls)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			r := New(&hubclient.Client{Config: config{srv.URL}}, "claude")
			r.bound, r.last, r.SuccessionEnabled = old, old, enableResolverSuccession
			b, err := r.Resolve(t.Context(), "")
			if !errors.Is(err, tc.want) || r.bound != old || r.Lost() || len(r.gone) != 0 {
				t.Fatalf("refresh = %+v, %v, retained %+v, gone %v", b, err, r.bound, r.gone)
			}
			if tc.want == nil && b != old || tc.want != nil && b != (Bound{}) {
				t.Fatalf("unexpected caller binding %+v after %v", b, err)
			}
			wantCalls := 1
			if tc.heartbeat != "" {
				wantCalls++
			}
			if tc.followup != "" {
				wantCalls++
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != wantCalls {
				t.Fatalf("calls %v, want %d", calls, wantCalls)
			}
		})
	}
}

func TestUnclassifiedRefreshCannotReclassifyTheCachedHooklessChild(t *testing.T) {
	sessions, prefix, _ := bindingEvidenceFiles(t)
	rec := shimBindingEvidence()
	raw, err := sessionproof.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeBindingEvidence(t, prefix+".execution", string(raw))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/bind" {
			t.Errorf("refresh dispatched %s", req.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": rec.ExecutionID, "generation": rec.Generation, "hook_seen": true})
	}))
	defer srv.Close()
	r := New(&hubclient.Client{Config: config{srv.URL}}, "claude")
	r.Evidence, r.SuccessionEnabled, r.ParentAlive = sessions, enableResolverSuccession, func() bool { return true }
	r.Getenv = func(key string) string {
		if key == "CLAUDE_CODE_SESSION_ID" {
			return "native"
		}
		return ""
	}
	r.bound = Bound{ExecutionID: rec.ExecutionID, Generation: rec.Generation, SessionID: "native", Rung: 2, Classified: true,
		Class: hubclient.Classification{Kind: hubclient.CandidateShimSuccession, PriorGeneration: rec.PriorGeneration, PriorExecutionID: rec.PriorExecutionID}}
	if b, err := r.Resolve(t.Context(), ""); err != nil || b.HookSeen || !b.Classified || !b.Class.IsHooklessSuccession() {
		t.Fatalf("legacy bind promoted an admitted hookless child: %+v, %v", b, err)
	}
}

func TestFallbackRechecksAliasProofAndRejectsMalformedRegistration(t *testing.T) {
	for _, proofAppears := range []bool{false, true} {
		t.Run(strconv.FormatBool(proofAppears), func(t *testing.T) {
			sessions, prefix, _ := bindingEvidenceFiles(t)
			var seenRegistration atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body hubclient.SessionRequest
				_ = json.NewDecoder(req.Body).Decode(&body)
				seenRegistration.Store(true)
				if req.URL.Path != "/v1/sessions/outside" || body.SessionID != "mcp-42-100" || body.Generation != 0 {
					t.Errorf("fallback registration on %s = %+v", req.URL.Path, body)
				}
				_, _ = io.WriteString(w, `{"execution_id":"ex_fallback","generation":1,"capabilities":["succession_proof"],"hook_seen":false}`)
			}))
			defer srv.Close()
			r := New(&hubclient.Client{Config: config{srv.URL}}, "claude")
			r.Evidence, r.SuccessionEnabled, r.Grace, r.CWD = sessions, enableResolverSuccession, 0, t.TempDir()
			r.Getenv = func(key string) string {
				if key == "CLAUDE_CODE_SESSION_ID" {
					return "native"
				}
				return ""
			}
			r.Ancestor = func() (proc.Ancestor, error) {
				return proc.Ancestor{PID: 42, StartMS: 100, Name: "claude"}, nil
			}
			// Exercise the final fallback guard directly after a completed unbound
			// lookup, with or without newly visible evidence for the native alias.
			if proofAppears {
				writeBindingEvidence(t, prefix, "1")
			}
			want := errInvalidCandidate
			if proofAppears {
				want = errGenerationFloor
			}
			unbound := &hubclient.Error{Code: "unbound"}
			if b, err := r.orRegister(t.Context(), "", Bound{}, unbound); !errors.Is(err, want) || b != (Bound{}) ||
				r.bound != (Bound{}) || r.own || seenRegistration.Load() == proofAppears {
				t.Fatalf("fallback = %+v, %v, attempted %t, cache %+v", b, err, seenRegistration.Load(), r.bound)
			}
		})
	}
}
