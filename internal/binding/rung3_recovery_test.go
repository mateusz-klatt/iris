package binding

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
)

const (
	rung3OffHeartbeat = `{"generation":2,"source":"shim"}`
	rung3OnHeartbeat  = `{"generation":2,"source":"shim","succession_proof":true}`
	rung3ProcessQuery = "?process_bound=true"
)

type resolverWireRequest struct {
	method string
	path   string
	body   string
	status int
}

type rung3RecoveryFixture struct {
	hub       *liveHub
	resolver  *Resolver
	ancestor  proc.Ancestor
	selection Bound
	config    string
	exact     string
	requests  []resolverWireRequest
}

func recordResolverRequests(client *http.Client, requests *[]resolverWireRequest) {
	base := client.Transport
	client.Transport = adoptionTransport(func(req *http.Request) (*http.Response, error) {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			return nil, err
		}
		res, err := base.RoundTrip(req)
		if err == nil {
			*requests = append(*requests, resolverWireRequest{req.Method, req.URL.RequestURI(), string(raw), res.StatusCode})
		}
		return res, err
	})
}

func observeResolverResponse(client *http.Client, observe func(*http.Request, *http.Response)) {
	base := client.Transport
	client.Transport = adoptionTransport(func(req *http.Request) (*http.Response, error) {
		res, err := base.RoundTrip(req)
		if err == nil {
			observe(req, res)
		}
		return res, err
	})
}

func newRung3RecoveryFixture(t *testing.T, opted bool) *rung3RecoveryFixture {
	t.Helper()
	return newRung3RecoveryFixtureFor(t, opted, "cursor")
}

func newRung3RecoveryFixtureFor(t *testing.T, opted bool, hookClient string) *rung3RecoveryFixture {
	t.Helper()
	f := &rung3RecoveryFixture{hub: newLiveHub(t), ancestor: proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}}
	if hookClient == "claude" {
		f.ancestor.Name = "claude"
	}
	client := f.hub.client()
	recordResolverRequests(client.HTTP, &f.requests)
	cwd := t.TempDir()
	registration := store.Request{
		Client: hookClient, Host: "host", SessionID: hookClient + "-session", Cwd: cwd, Worktree: cwd,
		Source: "hook", SingleSession: proc.SingleSession(f.ancestor, hookClient), ClientPID: int64(f.ancestor.PID), ClientPIDStartMS: f.ancestor.StartMS,
	}
	first, err := f.hub.s.Call(t.Context(), "sessions", registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.s.Call(t.Context(), "end", store.Request{ExecutionID: first["execution_id"].(string), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	registration.EndedThrough = 1
	current, err := f.hub.s.Call(t.Context(), "sessions", registration)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	f.config = filepath.Join(dir, "client.json")
	f.setSuccession(t, opted)
	resolved := hubclient.Resolved{ConfigFile: f.config}
	sessions := hook.NewSessions(filepath.Join(dir, "hub.token"))
	if err := sessions.Protect(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sessions.InspectEvidence(registration.Client, registration.SessionID, time.Time{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	execution := current["execution_id"].(string)
	exact, err := sessionproof.EncodeRecord(sessionproof.Record{Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: 2, ExecutionID: execution})
	if err != nil {
		t.Fatal(err)
	}
	f.exact = snapshot.BasePath + ".execution"
	for path, body := range map[string][]byte{
		snapshot.BasePath: []byte("2\n"), snapshot.BasePath + ".ended": []byte("1\n"), f.exact: exact,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.resolver = &Resolver{
		Hub: client, Client: registration.Client, Host: registration.Host, CWD: cwd,
		Getenv: func(key string) string {
			if hookClient == "claude" && key == "CLAUDE_CODE_SESSION_ID" {
				return "continue-alias"
			}
			return ""
		},
		Ancestor: func() (proc.Ancestor, error) { return f.ancestor, nil },
		Recall:   sessions.RecallBindingGeneration, Evidence: sessions, SuccessionEnabled: resolved.SuccessionEnabledContext,
		ParentAlive: func() bool { return true },
	}
	f.selection, err = f.resolver.Resolve(t.Context(), "")
	if err != nil || f.selection.ExecutionID != execution || f.selection.SessionID != registration.SessionID || f.selection.Rung != 3 || f.selection.Generation != 2 || !f.selection.HookSeen {
		t.Fatalf("could not bind the process's hook session: %+v %v", f.selection, err)
	}
	processLookup := 0
	if hookClient == "claude" {
		processLookup = 1 // --continue's alias first fails its native session lookup.
	}
	if len(f.requests) != processLookup+1 {
		t.Fatalf("initial Rung-3 bind dispatched extra requests: %+v", f.requests)
	}
	if hookClient == "claude" {
		f.requireBind(t, 0, http.StatusConflict)
	}
	f.requireBind(t, processLookup, http.StatusOK)
	f.requests = nil
	return f
}

func (f *rung3RecoveryFixture) setSuccession(t *testing.T, enabled bool) {
	t.Helper()
	if err := os.WriteFile(f.config, fmt.Appendf(nil, `{"session_succession":%t}`, enabled), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *rung3RecoveryFixture) requireHeartbeat(t *testing.T, index int, body string, status int) {
	t.Helper()
	path := "/v1/executions/" + f.selection.ExecutionID + "/heartbeat"
	if body == rung3OnHeartbeat {
		path += rung3ProcessQuery
	}
	want := resolverWireRequest{http.MethodPost, path, body, status}
	if got := f.requests[index]; got != want {
		t.Fatalf("heartbeat request %d = %+v, want %+v", index, got, want)
	}
}

func (f *rung3RecoveryFixture) requireBind(t *testing.T, index int, status int) {
	t.Helper()
	if got := f.requests[index]; got.method != http.MethodPost || got.path != "/v1/bind" || got.status != status {
		t.Fatalf("request %d = %+v, want POST /v1/bind returning %d", index, got, status)
	}
}

func (f *rung3RecoveryFixture) requireReattach(t *testing.T, index int, status int) {
	t.Helper()
	got := f.requests[index]
	if got.method != http.MethodPost || got.path != "/v1/sessions"+rung3ProcessQuery || got.status != status {
		t.Fatalf("request %d = %+v, want process-bound POST /v1/sessions returning %d", index, got, status)
	}
	var request hubclient.SessionRequest
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got.body), &request); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(got.body), &fields); err != nil {
		t.Fatal(err)
	}
	if request.SessionID != f.selection.SessionID || request.Generation != f.selection.Generation || request.EndedThrough != 1 || request.Source != "shim" ||
		request.ClientPID != f.ancestor.PID || request.ClientPIDStartMS != f.ancestor.StartMS || fields["succession_proof"] != nil {
		t.Fatalf("reattachment did not send the process tuple and local floor: %s", got.body)
	}
}

func TestRung3ResolverRestartRecoveryWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opted   bool
		refresh bool
		count   int
	}{
		{name: "off tick", count: 1},
		{name: "on tick", opted: true, count: 2},
		{name: "off refresh", refresh: true, count: 3},
		{name: "on refresh", opted: true, refresh: true, count: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRung3RecoveryFixture(t, tc.opted)
			f.hub.restart(t)
			if tc.refresh {
				if _, err := f.resolver.Resolve(t.Context(), ""); err != nil {
					t.Fatalf("refresh did not reattach: %v, requests=%+v", err, f.requests)
				}
			} else {
				ticks := 0
				f.resolver.Maintain(t.Context(), "shim", time.Millisecond, func() bool {
					ticks++
					return ticks == 1
				}, func() {})
			}
			current := f.resolver.bound
			if current.ExecutionID != f.selection.ExecutionID || current.Generation != f.selection.Generation || current.Rung != 3 || !current.HookSeen || f.resolver.Lost() {
				t.Fatalf("recovery lost the admitted process binding: %+v (lost %v)", current, f.resolver.Lost())
			}
			if len(f.requests) != tc.count {
				t.Fatalf("recovery requests = %+v, want %d", f.requests, tc.count)
			}
			switch {
			case tc.refresh && tc.opted:
				f.requireBind(t, 0, http.StatusConflict)
				f.requireReattach(t, 1, http.StatusOK)
			case tc.refresh:
				f.requireBind(t, 0, http.StatusConflict)
				f.requireHeartbeat(t, 1, rung3OffHeartbeat, http.StatusOK)
				f.requireBind(t, 2, http.StatusOK)
			case tc.opted:
				f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
				f.requireReattach(t, 1, http.StatusOK)
			default:
				f.requireHeartbeat(t, 0, rung3OffHeartbeat, http.StatusOK)
			}
		})
	}
}

func TestClaudeContinueRung3RestartRequiresProcessRegistration(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_tuple=%t", changed), func(t *testing.T) {
			f := newRung3RecoveryFixtureFor(t, true, "claude")
			f.hub.restart(t)
			if changed {
				f.ancestor.StartMS++
			}
			err := f.resolver.Heartbeat(t.Context(), "shim")
			if len(f.requests) != 2 {
				t.Fatalf("Claude process recovery requests = %+v, want a live-only heartbeat and registration", f.requests)
			}
			f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
			if changed {
				f.requireReattach(t, 1, http.StatusConflict)
				if !IsCode(err, "lost") || f.resolver.bound != (Bound{}) || !f.resolver.gone[f.selection.ExecutionID] {
					t.Fatalf("Claude process tuple mismatch was not confirmed lost: %v, binding=%+v, gone=%v", err, f.resolver.bound, f.resolver.gone)
				}
			} else {
				f.requireReattach(t, 1, http.StatusOK)
				if err != nil || f.resolver.bound.ExecutionID != f.selection.ExecutionID || f.resolver.bound.Generation != 2 || f.resolver.Lost() {
					t.Fatalf("Claude process did not reattach its hook session: %v, binding=%+v", err, f.resolver.bound)
				}
			}
		})
	}
}

func TestRung3ResolverRestartRejectsChangedProcessTuple(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%t", refresh), func(t *testing.T) {
			f := newRung3RecoveryFixture(t, true)
			f.hub.restart(t)
			f.ancestor.StartMS++ // Same PID reused by another process.
			var err error
			if refresh {
				_, err = f.resolver.Resolve(t.Context(), "")
			} else {
				err = f.resolver.Heartbeat(t.Context(), "shim")
			}
			if !IsCode(err, "lost") || f.resolver.bound != (Bound{}) || !f.resolver.gone[f.selection.ExecutionID] {
				t.Fatalf("changed process tuple was not confirmed lost: %v, %+v, gone=%v", err, f.resolver.bound, f.resolver.gone)
			}
			if len(f.requests) != 2 {
				t.Fatalf("changed process tuple dispatched unexpected requests: %+v", f.requests)
			}
			if refresh {
				f.requireBind(t, 0, http.StatusConflict)
			} else {
				f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
			}
			f.requireReattach(t, 1, http.StatusConflict)

			f.ancestor.StartMS--
			f.requests = nil
			if _, err := f.resolver.Resolve(t.Context(), ""); !IsCode(err, "lost") || len(f.requests) != 1 {
				t.Fatalf("memory retried an Execution the hub refused: %v, requests=%+v", err, f.requests)
			}
			f.requireBind(t, 0, http.StatusConflict)
		})
	}
}

func TestRung3ResolverRestartRequiresLocalProcessTuple(t *testing.T) {
	f := newRung3RecoveryFixture(t, true)
	f.hub.restart(t)
	start := f.ancestor.StartMS
	f.ancestor.StartMS = 0
	if err := f.resolver.Heartbeat(t.Context(), "shim"); !IsCode(err, "lost") || len(f.resolver.gone) != 0 {
		t.Fatalf("missing process tuple was not a local refusal: %v, gone=%v", err, f.resolver.gone)
	}
	if len(f.requests) != 1 {
		t.Fatalf("missing process tuple dispatched reattachment: %+v", f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)

	f.ancestor.StartMS = start
	f.requests = nil
	if err := f.resolver.Heartbeat(t.Context(), "shim"); err != nil || f.resolver.bound.ExecutionID != f.selection.ExecutionID || f.resolver.Lost() {
		t.Fatalf("a local refusal blocked later recovery of the same execution: %v, binding=%+v", err, f.resolver.bound)
	}
	if len(f.requests) != 2 {
		t.Fatalf("retry after a local refusal = %+v, want heartbeat then reattachment", f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
	f.requireReattach(t, 1, http.StatusOK)
}

func TestRung3ResolverRechecksEvidenceBeforeRegistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *rung3RecoveryFixture)
		ok     bool
	}{
		{name: "damaged exact record", change: func(t *testing.T, f *rung3RecoveryFixture) {
			if err := os.WriteFile(f.exact, []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "advanced end", change: func(t *testing.T, f *rung3RecoveryFixture) {
			if err := os.WriteFile(f.exact[:len(f.exact)-len(".execution")]+".ended", []byte("2\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same generation fork", change: func(t *testing.T, f *rung3RecoveryFixture) {
			f.publishExact(t, 2, "ex_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		}},
		{name: "missing exact record retains earlier pair", ok: true, change: func(t *testing.T, f *rung3RecoveryFixture) {
			if err := os.Remove(f.exact); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRung3RecoveryFixture(t, true)
			f.hub.restart(t)
			observeResolverResponse(f.resolver.Hub.HTTP, func(req *http.Request, res *http.Response) {
				if res.StatusCode == http.StatusConflict && req.URL.Path != "/v1/sessions" {
					tc.change(t, f)
				}
			})
			err := f.resolver.Heartbeat(t.Context(), "shim")
			if tc.ok {
				if err != nil || len(f.requests) != 2 || f.resolver.bound.ExecutionID != f.selection.ExecutionID {
					t.Fatalf("local retirement discarded the earlier exact pair: %v, requests=%+v", err, f.requests)
				}
				f.requireReattach(t, 1, http.StatusOK)
			} else if !IsCode(err, "lost") || len(f.requests) != 1 || len(f.resolver.gone) != 0 {
				t.Fatalf("new local evidence failed to veto reattachment: %v, requests=%+v, gone=%v", err, f.requests, f.resolver.gone)
			}
			f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
		})
	}
}

func (f *rung3RecoveryFixture) publishExact(t *testing.T, gen int, execution string) {
	t.Helper()
	raw, err := sessionproof.EncodeRecord(sessionproof.Record{Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: gen, ExecutionID: execution})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.exact, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRung3ResolverRechecksParentAfterRegistration(t *testing.T) {
	f := newRung3RecoveryFixture(t, true)
	f.hub.restart(t)
	alive := true
	f.resolver.ParentAlive = func() bool { return alive }
	observeResolverResponse(f.resolver.Hub.HTTP, func(req *http.Request, res *http.Response) {
		if req.URL.Path == "/v1/sessions" && res.StatusCode == http.StatusOK {
			alive = false
		}
	})
	b, err := f.resolver.RenewPrimary(t.Context(), f.selection)
	if !IsCode(err, "lost") || b != (Bound{}) || len(f.resolver.gone) != 0 || len(f.requests) != 2 {
		t.Fatalf("reattachment was returned after parent loss: %+v %v, gone=%v, requests=%+v", b, err, f.resolver.gone, f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
	f.requireReattach(t, 1, http.StatusOK)
}

func TestRung3ResolverSkipsRegistrationWhenParentExitsDuringHeartbeat(t *testing.T) {
	f := newRung3RecoveryFixture(t, true)
	f.hub.restart(t)
	alive := true
	f.resolver.ParentAlive = func() bool { return alive }
	observeResolverResponse(f.resolver.Hub.HTTP, func(_ *http.Request, res *http.Response) {
		if res.StatusCode == http.StatusConflict {
			alive = false
		}
	})
	if err := f.resolver.Heartbeat(t.Context(), "shim"); !IsCode(err, "lost") || len(f.requests) != 1 || len(f.resolver.gone) != 0 {
		t.Fatalf("parent exit triggered recovery or confirmed loss: %v, requests=%+v, gone=%v", err, f.requests, f.resolver.gone)
	}
	f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
}

func TestRung3LateHeartbeatLeavesNewSelectionAlone(t *testing.T) {
	f := newRung3RecoveryFixture(t, true)
	f.hub.restart(t)
	r := f.resolver
	other, err := f.hub.s.Call(t.Context(), "sessions", store.Request{
		Client: r.Client, Host: r.Host, SessionID: "another-session", Cwd: r.CWD, Worktree: r.CWD,
		Source: "hook", SingleSession: true, ClientPID: int64(f.ancestor.PID + 1), ClientPIDStartMS: f.ancestor.StartMS,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := r.Resolve(t.Context(), other["execution_id"].(string))
	if err != nil || current.ExecutionID != other["execution_id"] {
		t.Fatalf("could not select the new destination: %+v %v", current, err)
	}
	f.requests = nil
	if err := r.beatOne(t.Context(), f.selection, "shim"); !IsCode(err, "lost") || r.bound != current || len(r.gone) != 0 || len(f.requests) != 1 {
		t.Fatalf("retiring heartbeat disturbed the current binding: %v, binding=%+v, gone=%v, requests=%+v", err, r.bound, r.gone, f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
}

func TestRung3PrimaryDiscardsRenewalOfNewerGeneration(t *testing.T) {
	f := newRung3RecoveryFixture(t, true)
	r := f.resolver
	if _, err := f.hub.s.Call(t.Context(), "end", store.Request{ExecutionID: f.selection.ExecutionID, Generation: 2}); err != nil {
		t.Fatal(err)
	}
	newer, err := f.hub.s.Call(t.Context(), "sessions", store.Request{
		Client: r.Client, Host: r.Host, SessionID: f.selection.SessionID, Cwd: r.CWD, Worktree: r.CWD,
		Source: "hook", SingleSession: true, ClientPID: int64(f.ancestor.PID), ClientPIDStartMS: f.ancestor.StartMS,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := newer["execution_id"].(string)
	observeResolverResponse(r.Hub.HTTP, func(req *http.Request, res *http.Response) {
		if res.StatusCode == http.StatusConflict && req.URL.Path != "/v1/sessions" {
			f.publishExact(t, 3, execution)
		}
	})
	if renewed, err := r.RenewPrimary(t.Context(), f.selection); !IsCode(err, "lost") || renewed != (Bound{}) {
		t.Fatalf("whoami returned a different binding from the one its call resolved: %+v %v", renewed, err)
	}
	if r.bound.ExecutionID != execution || r.bound.Generation != 3 || !r.bound.HookSeen || len(f.requests) != 2 {
		t.Fatalf("new hook generation was not admitted: %+v, requests=%+v", r.bound, f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OnHeartbeat, http.StatusConflict)
	f.selection = r.bound
	f.requireReattach(t, 1, http.StatusOK)
}

func TestRung3ResolverIgnoresDamagedExactProofOnlyWhileOff(t *testing.T) {
	f := newRung3RecoveryFixture(t, false)
	if err := os.WriteFile(f.exact, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.resolver.Heartbeat(t.Context(), "shim"); err != nil || f.resolver.Lost() {
		t.Fatalf("OFF heartbeat depended on damaged .execution: %v, lost=%v", err, f.resolver.Lost())
	}
	if len(f.requests) != 1 {
		t.Fatalf("OFF heartbeat requests = %+v, want one heartbeat", f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OffHeartbeat, http.StatusOK)

	f.setSuccession(t, true)
	f.requests = nil
	if err := f.resolver.Heartbeat(t.Context(), "shim"); !IsCode(err, "lost") {
		t.Fatalf("ON heartbeat ignored damaged .execution: %v", err)
	}
	if len(f.requests) != 0 || len(f.resolver.gone) != 0 {
		t.Fatalf("local veto contacted the hub or marked the Execution gone: %+v, gone=%v", f.requests, f.resolver.gone)
	}

	f.setSuccession(t, false)
	if current, err := f.resolver.Resolve(t.Context(), ""); err != nil || current.ExecutionID != f.selection.ExecutionID {
		t.Fatalf("OFF could not use the same hook binding after a local veto: %+v %v", current, err)
	}
	if _, err := f.hub.s.Call(t.Context(), "end", store.Request{ExecutionID: f.selection.ExecutionID, Generation: int64(f.selection.Generation)}); err != nil {
		t.Fatal(err)
	}
	f.requests = nil
	if err := f.resolver.Heartbeat(t.Context(), "shim"); !IsCode(err, "lost") || !f.resolver.Lost() || len(f.resolver.gone) != 0 {
		t.Fatalf("OFF terminal heartbeat permanently excluded an Execution: %v, gone=%v", err, f.resolver.gone)
	}
	if len(f.requests) != 1 {
		t.Fatalf("OFF terminal heartbeat dispatched unexpected requests: %+v", f.requests)
	}
	f.requireHeartbeat(t, 0, rung3OffHeartbeat, http.StatusConflict)
}
