package binding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
)

type rung2RecoveryFixture struct {
	hub      *restartableBindingHub
	resolver *Resolver
	proof    string
	prior    string
	current  string
	requests []resolverWireRequest
}

func newRung2RecoveryFixture(t *testing.T, origin sessionproof.Origin) *rung2RecoveryFixture {
	t.Helper()
	h := newRestartableBindingHub(t)
	ctx := t.Context()
	cwd := t.TempDir()
	reg := store.Request{Client: "claude", Host: "host", SessionID: "native-session", Cwd: cwd, Worktree: cwd, Source: "hook"}
	prior, err := h.store.Call(ctx, "sessions", reg)
	if err != nil {
		t.Fatal(err)
	}
	f := &rung2RecoveryFixture{hub: h, prior: prior["execution_id"].(string)}
	// Seed retained history and a generation the fresh resolver will discover after restart.
	h.clock.Advance(16 * time.Minute)
	if _, err := h.store.Call(ctx, "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	if origin == sessionproof.OriginShim {
		current, err = h.store.Call(ctx, "succeed", store.Request{
			Client: reg.Client, Host: reg.Host, SessionID: reg.SessionID, Source: "shim",
			PriorGeneration: 1, PriorExecutionID: f.prior,
		})
	} else {
		current, err = h.store.Call(ctx, "sessions", reg)
	}
	if err != nil {
		t.Fatal(err)
	}
	f.current = current["execution_id"].(string)
	h.restart()

	sessions := hook.NewSessions(filepath.Join(t.TempDir(), "hub.token"))
	if err := sessions.Protect(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sessions.InspectEvidence(reg.Client, reg.SessionID, time.Time{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	f.proof = snapshot.BasePath
	if err := os.WriteFile(f.proof, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		record := sessionproof.Record{Version: sessionproof.FormatVersion, Origin: origin, Generation: 2, ExecutionID: f.current}
		if origin == sessionproof.OriginShim {
			record.PriorGeneration, record.PriorExecutionID = 1, f.prior
		}
		raw, err := sessionproof.EncodeRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.proof+".execution", raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	httpClient := h.srv.Client()
	recordResolverRequests(httpClient, &f.requests)
	f.resolver = &Resolver{
		Hub: &hubclient.Client{Config: config{h.srv.URL}, HTTP: httpClient}, Client: reg.Client, Host: reg.Host, CWD: cwd,
		Getenv: func(key string) string {
			if key == "CLAUDE_CODE_SESSION_ID" {
				return reg.SessionID
			}
			return ""
		},
		Evidence: sessions, SuccessionEnabled: func(context.Context) bool { return true }, ParentAlive: func() bool { return true },
	}
	return f
}

func changeRecoveryReply(t *testing.T, res *http.Response, edit func(map[string]any)) {
	t.Helper()
	var fields map[string]any
	if err := json.NewDecoder(res.Body).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	edit(fields)
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	res.Body = io.NopCloser(strings.NewReader(string(raw)))
	res.ContentLength = int64(len(raw))
}

func (f *rung2RecoveryFixture) requireUnboundBind(t *testing.T) {
	t.Helper()
	first := f.requests[0]
	if first.method != http.MethodPost || first.path != "/v1/bind" || first.status != http.StatusConflict {
		t.Fatalf("unexpected initial bind: %+v", first)
	}
	var bind hubclient.BindRequest
	if err := json.Unmarshal([]byte(first.body), &bind); err != nil {
		t.Fatal(err)
	}
	if !bind.SuccessionProof || bind.SessionID != "native-session" || bind.Execution != "" {
		t.Fatalf("initial bind used the wrong evidence: %s", first.body)
	}
}

func TestFreshRung2RecoversPublishedShimChild(t *testing.T) {
	for _, tc := range []struct {
		name         string
		otherSession bool
	}{
		{name: "native session"},
		{name: "another hook session matches the process", otherSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRung2RecoveryFixture(t, sessionproof.OriginShim)
			if tc.otherSession {
				anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "claude"}
				f.resolver.Ancestor = func() (proc.Ancestor, error) { return anc, nil }
				if _, err := f.hub.store.Call(t.Context(), "sessions", store.Request{
					Client: f.resolver.Client, Host: f.resolver.Host, SessionID: "another-session", Cwd: f.resolver.CWD,
					Source: "hook", ClientPID: int64(anc.PID), ClientPIDStartMS: anc.StartMS,
				}); err != nil {
					t.Fatal(err)
				}
			}
			b, err := f.resolver.Resolve(t.Context(), "")
			if err != nil || b.ExecutionID != f.current || b.Generation != 2 || b.Rung != 2 || b.HookSeen || !b.Classified || !b.Class.IsHooklessSuccession() || b.Class.PriorExecutionID != f.prior {
				t.Fatalf("recorded child was not recovered and admitted: %+v %v", b, err)
			}
			if len(f.requests) != 3 {
				t.Fatalf("recovery did not bind, heartbeat and bind: %+v", f.requests)
			}
			f.requireUnboundBind(t)
			beat := resolverWireRequest{http.MethodPost, "/v1/executions/" + f.current + "/heartbeat", `{"generation":2,"source":"shim","succession_proof":true}`, http.StatusOK}
			if got := f.requests[1]; got != beat {
				t.Fatalf("heartbeat = %+v, want %+v", got, beat)
			}
			rebind := f.requests[2]
			if rebind.method != http.MethodPost || rebind.path != "/v1/bind" || rebind.status != http.StatusOK {
				t.Fatalf("recovered child was not classified by bind: %+v", rebind)
			}
			var bind hubclient.BindRequest
			if err := json.Unmarshal([]byte(rebind.body), &bind); err != nil {
				t.Fatal(err)
			}
			if !bind.SuccessionProof || bind.Execution != f.current {
				t.Fatalf("follow-up bind did not require the recorded child: %s", rebind.body)
			}
		})
	}
}

func TestFreshRung2CannotDiscardProofOrRegisterAfterRecoveryRefusal(t *testing.T) {
	for _, mode := range []string{"files retired", "different exact pair", "unbound recovery reply"} {
		t.Run(mode, func(t *testing.T) {
			f := newRung2RecoveryFixture(t, sessionproof.OriginShim)
			base := f.resolver.Hub.HTTP.Transport
			f.resolver.Hub.HTTP.Transport = adoptionTransport(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/heartbeat") {
					if mode != "unbound recovery reply" {
						t.Errorf("local veto dispatched heartbeat: %s", req.URL.Path)
					}
					body := `{"error":{"code":"unbound","message":"could not recover the pair"}}`
					return &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				res, err := base.RoundTrip(req)
				if req.URL.Path == "/v1/bind" && err == nil {
					switch mode {
					case "files retired":
						for _, path := range []string{f.proof, f.proof + ".execution"} {
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
						}
					case "different exact pair":
						raw, err := sessionproof.EncodeRecord(sessionproof.Record{
							Version: sessionproof.FormatVersion, Origin: sessionproof.OriginShim,
							Generation: 2, ExecutionID: admissionOtherID, PriorGeneration: 1, PriorExecutionID: f.prior,
						})
						if err != nil {
							t.Fatal(err)
						}
						writeBindingEvidence(t, f.proof+".execution", string(raw))
					}
				}
				return res, err
			})
			if b, err := f.resolver.Resolve(t.Context(), ""); b != (Bound{}) || !IsCode(err, "lost") || f.resolver.Lost() || len(f.resolver.gone) != 0 {
				t.Fatalf("recovery refusal fell through: %+v, %v, gone %v", b, err, f.resolver.gone)
			}
			if len(f.requests) != 1 || len(f.hub.sessions()) != 0 {
				t.Fatalf("refusal tried more hub requests: %+v, sessions %+v", f.requests, f.hub.sessions())
			}
		})
	}
}

func TestFreshRung2ShimRecoveryVetoSkipsHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix string
		body   string
	}{
		{name: "predecessor ended", suffix: ".ended", body: "1\n"},
		{name: "generation file ahead", body: "3\n"},
		{name: "parent gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRung2RecoveryFixture(t, sessionproof.OriginShim)
			if tc.body == "" {
				f.resolver.ParentAlive = func() bool { return false }
			} else if err := os.WriteFile(f.proof+tc.suffix, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if b, err := f.resolver.Resolve(t.Context(), ""); !IsCode(err, "lost") || b != (Bound{}) || len(f.resolver.gone) != 0 {
				t.Fatalf("local veto did not refuse recovery cleanly: %+v %v, gone=%v", b, err, f.resolver.gone)
			}
			if len(f.requests) != 1 {
				t.Fatalf("local veto dispatched a mutating request: %+v", f.requests)
			}
			f.requireUnboundBind(t)
		})
	}
}

func TestFreshRung2ReattachesHookRegistration(t *testing.T) {
	for _, origin := range []sessionproof.Origin{sessionproof.OriginHook, ""} {
		name := "hook record"
		if origin == "" {
			name = "generation file only"
		}
		t.Run(name, func(t *testing.T) {
			f := newRung2RecoveryFixture(t, origin)
			b, err := f.resolver.Resolve(t.Context(), "")
			if err != nil || b.ExecutionID != f.current || b.Generation != 2 || b.Rung != 2 || !b.HookSeen || !b.Classified {
				t.Fatalf("known hook registration did not recover: %+v %v", b, err)
			}
			if len(f.requests) != 2 {
				t.Fatalf("hook recovery did not bind and register: %+v", f.requests)
			}
			f.requireUnboundBind(t)
			reg := f.requests[1]
			if reg.method != http.MethodPost || reg.path != "/v1/sessions" || reg.status != http.StatusOK {
				t.Fatalf("hook recovery did not register: %+v", reg)
			}
			var request hubclient.SessionRequest
			if err := json.Unmarshal([]byte(reg.body), &request); err != nil {
				t.Fatal(err)
			}
			if request.Generation != 2 || request.SessionID != "native-session" || request.Source != "shim" || !request.SuccessionProof || request.ClientPID != 0 || request.ClientPIDStartMS != 0 {
				t.Fatalf("Rung-2 registration did not carry positive generation proof: %s", reg.body)
			}
		})
	}
}

func TestFreshRung2RecordedRecoveryChecksReplies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		on    string
		count int
		edit  func(*testing.T, *rung2RecoveryFixture, *http.Response)
	}{
		{name: "heartbeat names another Execution", on: "/heartbeat", count: 2, edit: func(t *testing.T, _ *rung2RecoveryFixture, res *http.Response) {
			changeRecoveryReply(t, res, func(fields map[string]any) { fields["execution_id"] = "ex_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" })
		}},
		{name: "Execution ends before confirmation bind", on: "/heartbeat", count: 3, edit: func(t *testing.T, f *rung2RecoveryFixture, _ *http.Response) {
			if _, err := f.hub.store.Call(t.Context(), "end", store.Request{ExecutionID: f.current, Generation: 2}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "bind omits classification", on: "/v1/bind", count: 3, edit: func(t *testing.T, _ *rung2RecoveryFixture, res *http.Response) {
			changeRecoveryReply(t, res, func(fields map[string]any) { delete(fields, "capabilities") })
		}},
		{name: "publication disappears during bind", on: "/v1/bind", count: 3, edit: func(t *testing.T, f *rung2RecoveryFixture, _ *http.Response) {
			if err := os.Remove(f.proof + ".execution"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRung2RecoveryFixture(t, sessionproof.OriginShim)
			observeResolverResponse(f.resolver.Hub.HTTP, func(req *http.Request, res *http.Response) {
				if res.StatusCode == http.StatusOK && strings.HasSuffix(req.URL.Path, tc.on) {
					tc.edit(t, f, res)
				}
			})
			b, err := f.resolver.Resolve(t.Context(), "")
			if !IsCode(err, "lost") || b != (Bound{}) || len(f.resolver.gone) != 0 || len(f.requests) != tc.count {
				t.Fatalf("unsafe reattachment was accepted or fell back: %+v %v, gone=%v, requests=%+v", b, err, f.resolver.gone, f.requests)
			}
			f.requireUnboundBind(t)
		})
	}
}

func TestFreshRung2RecordedRecoveryPreservesHubLoss(t *testing.T) {
	f := newRung2RecoveryFixture(t, sessionproof.OriginShim)
	f.hub.clock.Advance(3 * time.Minute)
	if _, err := f.hub.store.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	b, err := f.resolver.Resolve(t.Context(), "")
	if !IsCode(err, "lost") || b != (Bound{}) || !f.resolver.gone[f.current] || len(f.requests) != 2 {
		t.Fatalf("recovery did not preserve loss for the recorded pair: %+v %v, gone=%v, requests=%+v", b, err, f.resolver.gone, f.requests)
	}
	f.requireUnboundBind(t)
	if beat := f.requests[1]; beat.path != "/v1/executions/"+f.current+"/heartbeat" || beat.status != http.StatusConflict {
		t.Fatalf("expected the hub to refuse the recorded child: %+v", beat)
	}
}

func TestFreshRung2HookRecoveryChecksRegistrationReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *rung2RecoveryFixture, *http.Response)
	}{
		{name: "incomplete capable reply", edit: func(t *testing.T, _ *rung2RecoveryFixture, res *http.Response) {
			changeRecoveryReply(t, res, func(fields map[string]any) { delete(fields, "creation_proof") })
		}},
		{name: "different generation", edit: func(t *testing.T, _ *rung2RecoveryFixture, res *http.Response) {
			changeRecoveryReply(t, res, func(fields map[string]any) { fields["generation"] = 3 })
		}},
		{name: "end marker advanced", edit: func(t *testing.T, f *rung2RecoveryFixture, _ *http.Response) {
			if err := os.WriteFile(f.proof+".ended", []byte("2\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRung2RecoveryFixture(t, sessionproof.OriginHook)
			observeResolverResponse(f.resolver.Hub.HTTP, func(req *http.Request, res *http.Response) {
				if req.URL.Path == "/v1/sessions" && res.StatusCode == http.StatusOK {
					tc.edit(t, f, res)
				}
			})
			b, err := f.resolver.Resolve(t.Context(), "")
			if !IsCode(err, "lost") || b != (Bound{}) || len(f.resolver.gone) != 0 || len(f.requests) != 2 || f.requests[1].path != "/v1/sessions" {
				t.Fatalf("positive registration reply was admitted or retried: %+v %v, gone=%v, requests=%+v", b, err, f.resolver.gone, f.requests)
			}
			f.requireUnboundBind(t)
		})
	}
}
