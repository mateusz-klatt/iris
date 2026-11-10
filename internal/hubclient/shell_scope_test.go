package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// A reduced snapshot goes to the scoped route in both forms, flat as the hub reads it, with its
// scope; the answers carry the capability. A hub without the route answers not_found, which the
// caller sees as that code.
func TestReducedSnapshotsGoToTheScopedRoute(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		switch {
		case r.URL.Path != "/v1/shell/scoped":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown API route"}}`))
		case body["execution_id"] == nil:
			_, _ = w.Write([]byte(`{"message":"","conflicts":[],"session":{"execution_id":"ex_1","generation":2,"capabilities":["shell_scope"]}}`))
		default:
			_, _ = w.Write([]byte(`{"message":"held","conflicts":[]}`))
		}
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	scope := Scope{Mode: "dirty", Submodules: []string{"lib"}}
	session, err := c.ReconcileScopedSession(context.Background(), ScopedSessionRequest{ShellSessionRequest: ShellSessionRequest{SessionRequest: SessionRequest{SessionID: "s", Client: "codex"}, Kind: "before", Status: map[string]string{}}, Scope: scope})
	if err != nil || session.Session.ExecutionID != "ex_1" || !ShellScopeCapable(session.Session.Capabilities) {
		t.Fatalf("session form: %+v %v", session, err)
	}
	out, err := c.ReconcileScoped(context.Background(), ScopedRequest{ReconcileRequest: ReconcileRequest{ExecutionID: "ex_1", Generation: 2, Kind: "after", Status: map[string]string{"a": " M"}}, Scope: scope})
	if err != nil || out.Message != "held" {
		t.Fatalf("execution form: %+v %v", out, err)
	}
	for i, body := range got {
		if !reflect.DeepEqual(body["scope"], map[string]any{"mode": "dirty", "submodules": []any{"lib"}}) {
			t.Fatalf("request %d: %v", i, body)
		}
	}
	if got[0]["session_id"] != "s" || got[1]["execution_id"] != "ex_1" || got[1]["status"].(map[string]any)["a"] != " M" {
		t.Fatalf("sent %v", got)
	}
	older := &Client{Config: staticConfig{url: srv.URL + "/older", token: "tok"}}
	if _, err := older.ReconcileScoped(context.Background(), ScopedRequest{}); !IsCode(err, "not_found") {
		t.Fatalf("a hub without the route: %v", err)
	}
	if ShellScopeCapable(nil) || ShellScopeCapable([]string{"other"}) {
		t.Fatal("a capability that was not named")
	}
}
