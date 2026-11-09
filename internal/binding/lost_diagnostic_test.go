package binding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// A confirmed loss keeps its typed answer, but its retry failure must remain available
// to explain why notifications have not resumed. A retry never registers authority.
func TestLostBindingPreservesItsRetryFailure(t *testing.T) {
	cause := errors.New("the retry connection failed")
	requests := 0
	r := &Resolver{
		Hub: &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: diagnosticTransport(func(req *http.Request) (*http.Response, error) {
			requests++
			if req.URL.Path != "/v1/bind" {
				t.Errorf("lost binding attempted %s", req.URL.Path)
			}
			return nil, cause
		})}},
		Client: "codex", Host: "host", lost: true, gone: map[string]bool{"ex_old": true},
		Getenv: func(key string) string {
			if key == "IRIS_EXECUTION" {
				return "ex_retry"
			}
			return ""
		},
	}
	b, err := r.Resolve(context.Background(), "")
	if !IsCode(err, "lost") || !errors.Is(err, cause) || err.Error() != errLost.Error() || b.ExecutionID != "" || !r.Lost() || requests != 1 {
		t.Fatalf("retry lost its cause or changed authority: bound %+v, error %v, requests %d", b, err, requests)
	}
}

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestLostBindingPreservesSpecificHubResults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     string
		env      map[string]string
		ancestor proc.Ancestor
	}{
		{"session without process evidence", "unbound", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{}},
		{"ambiguous ancestry", "ambiguous", nil, proc.Ancestor{PID: 42, StartMS: 100}},
		{"explicit handle refusal", "unauthorized", map[string]string{"IRIS_EXECUTION": "ex_retry"}, proc.Ancestor{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &scriptedHub{binds: []string{tc.code, ""}}
			r := resolverFor(t, hub, "claude", tc.env, tc.ancestor)
			r.lost, r.gone = true, map[string]bool{"ex_old": true}
			b, err := r.Resolve(context.Background(), "")
			var lost *LostError
			if !errors.As(err, &lost) || !IsCode(err, "lost") || b.ExecutionID != "" || r.bound.ExecutionID != "" || !r.Lost() || hub.session != nil {
				t.Fatalf("failed retry changed authority or hid loss: %+v %v, bound %+v, registered %+v", b, err, r.bound, hub.session)
			}
			var cause *hubclient.Error
			if !errors.As(lost.Cause, &cause) || cause.Code != tc.code || cause.Message != tc.code {
				t.Fatalf("specific binding failure was replaced: %v", lost.Cause)
			}
			if b, err := r.Resolve(context.Background(), ""); err != nil || b.ExecutionID != "ex_bound" || r.Lost() || hub.session != nil {
				t.Fatalf("a positive replacement did not resume: %+v %v, registered %+v", b, err, hub.session)
			}
		})
	}
}

func TestLostBindingExplainsARejectedStaleResult(t *testing.T) {
	hub := &scriptedHub{}
	r := resolverFor(t, hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{})
	r.lost, r.gone = true, map[string]bool{"ex_bound": true}
	b, err := r.Resolve(context.Background(), "")
	var lost *LostError
	if !errors.As(err, &lost) || !IsCode(err, "lost") || b.ExecutionID != "" || r.bound.ExecutionID != "" || !r.Lost() || hub.session != nil {
		t.Fatalf("stale result gained authority or hid loss: %+v %v, bound %+v, registered %+v", b, err, r.bound, hub.session)
	}
	var cause *hubclient.Error
	if !errors.As(lost.Cause, &cause) || cause.Code != "lost" || cause.Message != "the binding retry returned an Execution already found lost" {
		t.Fatalf("stale result lost its diagnosis: %v", lost.Cause)
	}
}

func TestLostBindingKeepsHandleMismatchDetail(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		if req.URL.Path != "/v1/bind" {
			t.Errorf("lost binding attempted %s", req.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"unbound","message":"handle does not match this client and host"}}`))
	}))
	defer srv.Close()
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "claude", Host: "host", lost: true, gone: map[string]bool{"ex_old": true}, Getenv: func(key string) string {
		if key == "IRIS_EXECUTION" {
			return "ex_other_host"
		}
		return ""
	}}
	_, err := r.Resolve(context.Background(), "")
	var lost *LostError
	if !errors.As(err, &lost) || !IsCode(err, "lost") || !r.Lost() || requests != 1 {
		t.Fatalf("handle mismatch changed authority or hid loss: %v, requests %d", err, requests)
	}
	var cause *hubclient.Error
	if !errors.As(lost.Cause, &cause) || cause.Code != "unbound" || cause.Message != "handle does not match this client and host" {
		t.Fatalf("handle mismatch lost its diagnosis: %v", lost.Cause)
	}
}

func TestLostBindingExplainsASkippedStaleHandle(t *testing.T) {
	for _, source := range []string{"environment", "argument"} {
		t.Run(source, func(t *testing.T) {
			hub := &scriptedHub{}
			env := map[string]string{}
			handle := ""
			if source == "environment" {
				env["IRIS_EXECUTION"] = "ex_old"
			} else {
				handle = "ex_old"
			}
			r := resolverFor(t, hub, "claude", env, proc.Ancestor{})
			r.lost, r.gone = true, map[string]bool{"ex_old": true}
			b, err := r.Resolve(context.Background(), handle)
			if b.ExecutionID != "" || r.bound.ExecutionID != "" || !r.Lost() || !IsCode(err, "lost") || len(hub.bound) != 0 || hub.session != nil {
				t.Fatalf("stale handle was used: %+v %v, bound %+v, requests %+v, registered %+v", b, err, r.bound, hub.bound, hub.session)
			}
			var lost *LostError
			var cause *hubclient.Error
			if !errors.As(err, &lost) || !errors.As(lost.Cause, &cause) || cause.Code != "unbound" || cause.Message != "the execution handle names an Execution already found lost" {
				t.Fatalf("the known-stale handle was not explained: %v", err)
			}
			// A stale handle cannot hide newer positive evidence from the session or process.
			env["CLAUDE_CODE_SESSION_ID"] = "s"
			if b, err := r.Resolve(context.Background(), handle); err != nil || b.ExecutionID != "ex_bound" || b.Rung != 2 || r.Lost() || hub.session != nil {
				t.Fatalf("stale handle prevented session binding: %+v %v, registered %+v", b, err, hub.session)
			}
			if len(hub.bound) != 1 || hub.bound[0]["session_id"] != "s" || hub.bound[0]["execution"] != nil {
				t.Fatalf("session retry reused a stale handle: %+v", hub.bound)
			}
			delete(env, "CLAUDE_CODE_SESSION_ID")
			processHub := &scriptedHub{}
			process := resolverFor(t, processHub, "claude", env, proc.Ancestor{PID: 42, StartMS: 100})
			process.lost, process.gone = true, map[string]bool{"ex_old": true}
			if b, err := process.Resolve(context.Background(), handle); err != nil || b.ExecutionID != "ex_bound" || b.Rung != 3 || process.Lost() || processHub.session != nil {
				t.Fatalf("stale handle prevented process binding: %+v %v, registered %+v", b, err, processHub.session)
			}
			if len(processHub.bound) != 1 || processHub.bound[0]["client_pid"] != float64(42) || processHub.bound[0]["execution"] != nil {
				t.Fatalf("process retry reused a stale handle: %+v", processHub.bound)
			}
		})
	}
}
