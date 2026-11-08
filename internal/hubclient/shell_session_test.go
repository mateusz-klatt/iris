package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ReconcileSession sends the session's identity, its remembered generation, the phase and the
// snapshot in one request to the shell route, flat as the hub reads them, and returns the
// result with the Execution the session registered as.
func TestReconcileSessionSendsTheSessionAndReadsItsExecution(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/shell/reconcile" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":"held","conflicts":[],"session":{"execution_id":"ex_9","generation":4}}`))
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	out, err := c.ReconcileSession(context.Background(), ShellSessionRequest{SessionRequest: SessionRequest{SessionID: "s", Client: "claude", Generation: 3, Source: "hook"}, Kind: "after", Status: map[string]string{"a.go": " M"}})
	if err != nil || out.Message != "held" || out.Session.ExecutionID != "ex_9" || out.Session.Generation != 4 {
		t.Fatalf("%+v %v", out, err)
	}
	if got["session_id"] != "s" || got["generation"] != float64(3) || got["kind"] != "after" || got["status"].(map[string]any)["a.go"] != " M" || got["execution_id"] != nil {
		t.Fatalf("sent %v", got)
	}
}
