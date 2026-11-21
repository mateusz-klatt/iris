package binding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// A binding keeps the hub's capabilities as one value, so it stays comparable, and gives them
// back as the hub's list, in JSON as well.
func TestCapabilitiesRoundTripTheHubsList(t *testing.T) {
	var b Bound
	if err := json.Unmarshal([]byte(`{"execution_id":"ex","worktree":"/src/app","capabilities":["shell_scope","file_claims"]}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.Worktree != "/src/app" || !reflect.DeepEqual(b.Capabilities.List(), []string{"shell_scope", "file_claims"}) {
		t.Fatalf("%+v", b)
	}
	// A binding is a map key, as a wake target compares it (StartIfPrimary).
	if _, ok := map[Bound]bool{b: true}[b]; !ok {
		t.Fatal("a binding is not comparable")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if !reflect.DeepEqual(back["capabilities"], []any{"shell_scope", "file_claims"}) {
		t.Fatalf("marshalled %s", raw)
	}
	// An older hub names none, and the binding then says so.
	var old Bound
	_ = json.Unmarshal([]byte(`{"execution_id":"ex"}`), &old)
	if len(old.Capabilities.List()) != 0 {
		t.Fatalf("an older hub's binding: %+v", old.Capabilities.List())
	}
	if raw, _ := json.Marshal(old); string(raw) != `{"execution_id":"ex","generation":0,"repository_id":"","agent":{"id":"","name":""},"binding_rung":0}` {
		t.Fatalf("an older hub's binding marshalled %s", raw)
	}
	if err := json.Unmarshal([]byte(`{"capabilities":"shell_scope"}`), &b); err == nil {
		t.Fatal("a capability string was taken for a list")
	}
}

// An MCP server that registers an Execution of its own takes the worktree and capabilities from
// the registration answer.
func TestARegistrationKeepsTheWorktreeAndCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "ex_new", "generation": 1, "worktree": "/src/app", "capabilities": []string{"shell_scope", "file_claims"}})
	}))
	t.Cleanup(srv.Close)
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "codex", Host: "host", CWD: "/src/app"}
	b, err := r.register(context.Background(), "s", proc.Ancestor{PID: 1, StartMS: 2}, true, 3)
	if err != nil || b.Worktree != "/src/app" || !reflect.DeepEqual(b.Capabilities.List(), []string{"shell_scope", "file_claims"}) {
		t.Fatalf("%+v %v", b, err)
	}
}

// A binding bound again over an older hub's answer drops the worktree and capabilities a newer
// hub gave, so a hub rolled back under a running MCP server is not offered the grouped routes.
func TestABindingBoundAgainForgetsANewerHubsOffer(t *testing.T) {
	answer := `{"execution_id":"ex","generation":3,"repository_id":"rp"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(answer)) }))
	t.Cleanup(srv.Close)
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "codex", Host: "host"}
	r.bound = Bound{ExecutionID: "ex", Generation: 2, RepositoryID: "rp", Rung: 2, Worktree: "/src/app", Capabilities: Capabilities("shell_scope\nfile_claims")}
	b, err := r.refresh(context.Background())
	if err != nil || b.Worktree != "" || b.Capabilities != "" || b.Generation != 3 || b.Rung != 2 {
		t.Fatalf("%+v %v", b, err)
	}
}
