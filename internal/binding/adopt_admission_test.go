package binding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
)

const adoptionSession = "native"

func optedAdoptionFallback(t *testing.T, hub *liveHub) (*Resolver, Bound, *hook.Sessions, string) {
	t.Helper()
	sessions, prefix, _ := bindingEvidenceFiles(t)
	r := tickResolver(hub.client(), "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": adoptionSession}, proc.Ancestor{PID: 42, StartMS: 77})
	r.CWD = t.TempDir()
	r.Evidence = sessions
	r.SuccessionEnabled = func(context.Context) bool { return true }
	r.ParentAlive = func() bool { return true }
	mine, err := r.Resolve(t.Context(), "")
	if err != nil || !r.own || mine.HookSeen || !mine.Classified || mine.Generation != 1 || mine.SessionID != "mcp-42-77" {
		t.Fatalf("expected an opted process fallback: %+v, own=%t, %v", mine, r.own, err)
	}
	return r, mine, sessions, prefix
}

func adoptionHook(t *testing.T, hub *liveHub, r *Resolver) string {
	t.Helper()
	hook, err := hub.s.Call(t.Context(), "sessions", store.Request{
		Client: r.Client, Host: r.Host, Cwd: r.CWD, SessionID: adoptionSession, Source: "hook",
		ClientPID: 42, ClientPIDStartMS: 77,
	})
	if err != nil {
		t.Fatal(err)
	}
	return hook["execution_id"].(string)
}

func TestOptedFallbackRenewsAfterAdoptionRefusesCandidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		proof      string
		generation int
		prior      any
	}{
		{name: "ordinary without a hook", source: "mcp", proof: "initial", generation: 1},
		{name: "incomplete classification", generation: 1},
		{name: "hookless child with matching lineage", source: "shim", proof: "shim_succession", generation: 3, prior: admissionPriorID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLiveHub(t)
			r, mine, _, prefix := optedAdoptionFallback(t, hub)
			// Newer hook history for the same process must leave the fallback renewable
			// while adoption cannot use the destination.
			adoptionHook(t, hub, r)
			if tc.prior != nil {
				record, err := sessionproof.EncodeRecord(shimBindingEvidence())
				if err != nil {
					t.Fatal(err)
				}
				writeBindingEvidence(t, prefix+".execution", string(record))
			}
			reply := map[string]any{
				"execution_id": admissionChildID, "generation": tc.generation, "session_id": adoptionSession,
				"capabilities": []string{"succession_proof"}, "hook_seen": false,
			}
			if tc.source != "" {
				reply["prior_execution_id"], reply["creation_source"], reply["creation_proof"] = tc.prior, tc.source, tc.proof
			}
			base := r.Hub.HTTP.Transport
			var binds, beats int
			r.Hub.HTTP.Transport = adoptionTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1/bind":
					binds++
					w := httptest.NewRecorder()
					if err := json.NewEncoder(w).Encode(reply); err != nil {
						return nil, err
					}
					return w.Result(), nil
				case "/v1/executions/" + mine.ExecutionID + "/heartbeat":
					beats++
					body, err := req.GetBody()
					if err != nil {
						return nil, err
					}
					raw, err := io.ReadAll(body)
					_ = body.Close()
					if err != nil || string(raw) != `{"generation":1,"source":"shim","succession_proof":true}` {
						t.Fatalf("fallback heartbeat: %s, %v", raw, err)
					}
					res, err := base.RoundTrip(req)
					if err == nil && res.StatusCode != http.StatusOK {
						t.Errorf("newer hook invalidated live fallback: %s", res.Status)
					}
					return res, err
				default:
					t.Errorf("adoption sent an unexpected request: %s %s", req.Method, req.URL.Path)
					return base.RoundTrip(req)
				}
			})
			ticks := 0
			r.Maintain(t.Context(), "shim", time.Millisecond, func() bool {
				ticks++
				return ticks == 1
			}, func() {})
			if binds != 1 || beats != 1 || r.bound.ExecutionID != mine.ExecutionID || !r.own || r.pending != nil || r.Lost() {
				t.Fatalf("invalid handover destination disrupted fallback: binds=%d beats=%d bound=%+v own=%t pending=%+v", binds, beats, r.bound, r.own, r.pending)
			}
		})
	}
}

type evidenceAfterAdoptionBind struct {
	*hook.Sessions
	afterSnapshot func()
}

func (e *evidenceAfterAdoptionBind) InspectEvidence(client, session string, deadline time.Time, enabled, cleanExpired bool) (sessionproof.Snapshot, error) {
	snap, err := e.Sessions.InspectEvidence(client, session, deadline, enabled, cleanExpired)
	if session == adoptionSession && e.afterSnapshot != nil {
		next := e.afterSnapshot
		e.afterSnapshot = nil
		next()
	}
	return snap, err
}

func TestAdoptionRechecksEvidenceBeforeHandover(t *testing.T) {
	for _, tc := range []struct {
		name           string
		alreadyPending bool
	}{
		{name: "before first handover"},
		{name: "during pending handover", alreadyPending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLiveHub(t)
			r, mine, sessions, prefix := optedAdoptionFallback(t, hub)
			target := adoptionHook(t, hub, r)
			exact, err := sessionproof.EncodeRecord(sessionproof.Record{
				Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: 1, ExecutionID: target,
			})
			if err != nil {
				t.Fatal(err)
			}
			path := prefix + ".execution"
			writeBindingEvidence(t, path, string(exact))
			evidence := &evidenceAfterAdoptionBind{Sessions: sessions}
			r.Evidence = evidence
			base := r.Hub.HTTP.Transport
			damageAfterBind := !tc.alreadyPending
			var ends, fallbackBeats int
			r.Hub.HTTP.Transport = adoptionTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1/bind":
					res, err := base.RoundTrip(req)
					if damageAfterBind && err == nil && res.StatusCode == http.StatusOK {
						damageAfterBind = false
						// Damage arrives after reply admission reads healthy proof.
						evidence.afterSnapshot = func() { writeBindingEvidence(t, path, "damaged") }
					}
					return res, err
				case "/v1/executions/" + mine.ExecutionID + "/end":
					ends++
					if tc.alreadyPending && ends == 1 {
						return nil, errors.New("handover interrupted before reaching hub")
					}
				case "/v1/executions/" + mine.ExecutionID + "/heartbeat":
					fallbackBeats++
					res, err := base.RoundTrip(req)
					if err == nil && res.StatusCode != http.StatusOK {
						t.Errorf("fallback could not renew: %s", res.Status)
					}
					return res, err
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
				return base.RoundTrip(req)
			})

			r.adopt(t.Context())
			wantEnds := 0
			if tc.alreadyPending {
				wantEnds = 1
				if r.pending == nil || r.pending.from.ExecutionID != mine.ExecutionID || r.pending.to.ExecutionID != target {
					t.Fatalf("interrupted handover did not remain pending: %+v", r.pending)
				}
				writeBindingEvidence(t, path, "damaged")
			} else if r.pending != nil || r.bound != mine || !r.own {
				t.Fatalf("unchecked destination replaced the fallback: pending=%+v bound=%+v own=%t", r.pending, r.bound, r.own)
			}
			ticks := 0
			r.Maintain(t.Context(), "shim", time.Millisecond, func() bool {
				ticks++
				return ticks == 1
			}, func() {})
			if ends != wantEnds || fallbackBeats != 1 || (r.pending != nil) != tc.alreadyPending || r.Lost() || len(r.gone) != 0 {
				t.Fatalf("damage failed to pause handover while fallback renewed: ends=%d beats=%d pending=%+v lost=%t gone=%v",
					ends, fallbackBeats, r.pending, r.Lost(), r.gone)
			}
			writeBindingEvidence(t, path, string(exact))
			r.adopt(t.Context())
			if ends != wantEnds+1 || r.pending != nil || r.bound.ExecutionID != target || !r.bound.HookSeen || r.own {
				t.Fatalf("repaired evidence did not allow handover: ends=%d pending=%+v bound=%+v own=%t", ends, r.pending, r.bound, r.own)
			}
		})
	}
}
