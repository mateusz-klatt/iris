package binding

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func decodeBindingCandidate(t *testing.T, raw string) hubclient.Candidate {
	t.Helper()
	var candidate hubclient.Candidate
	if err := json.Unmarshal([]byte(raw), &candidate); err != nil {
		t.Fatal(err)
	}
	return candidate
}

func TestClassifiedBindingRejectsIncompleteProvenanceAndForeignSession(t *testing.T) {
	incomplete := decodeBindingCandidate(t, `{"execution_id":"ex_child","generation":2,"capabilities":["succession_proof"],"hook_seen":true}`)
	if b, err := boundFromCandidate(incomplete, "native", 2); b != (Bound{}) || !errors.Is(err, errInvalidCandidate) {
		t.Fatalf("incomplete provenance became a binding: %+v, %v", b, err)
	}
	foreign := decodeBindingCandidate(t, `{"execution_id":"ex_foreign","generation":1,"session_id":"foreign"}`)
	if b, err := boundFromCandidate(foreign, "native", 2); b != (Bound{}) || !errors.Is(err, errInvalidCandidate) {
		t.Fatalf("foreign session became a native binding: %+v, %v", b, err)
	}
}

func TestLegacyHeartbeatCannotPromoteOrReplaceClassifiedChild(t *testing.T) {
	child := decodeBindingCandidate(t, `{"execution_id":"ex_child","generation":2,"session_id":"native","capabilities":["succession_proof"],"prior_execution_id":"ex_prior","creation_source":"shim","creation_proof":"shim_succession","hook_seen":false}`)
	b, err := boundFromCandidate(child, "native", 2)
	if err != nil || !b.Classified || !b.Class.IsHooklessSuccession() {
		t.Fatalf("classify child: %+v, %v", b, err)
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"lease only", `{"lease_ms":60000}`},
		{"same pair without classification", `{"execution_id":"ex_child","generation":2,"repository_id":"rp_changed","hook_seen":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh, err := mergeCandidate(b, decodeBindingCandidate(t, tc.raw))
			if err != nil || fresh.Class != b.Class || !fresh.Classified || fresh.HookSeen || fresh.Rung != 2 || fresh.SessionID != "native" {
				t.Fatalf("legacy heartbeat changed admitted child: %+v, %v", fresh, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"wrong execution", `{"execution_id":"ex_other","generation":2}`},
		{"wrong generation", `{"execution_id":"ex_child","generation":3}`},
		{"wrong session", `{"execution_id":"ex_child","generation":2,"session_id":"foreign"}`},
		{"broken classification", `{"execution_id":"ex_child","generation":2,"capabilities":["succession_proof"],"hook_seen":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if fresh, err := mergeCandidate(b, decodeBindingCandidate(t, tc.raw)); fresh != (Bound{}) || !errors.Is(err, errInvalidCandidate) {
				t.Fatalf("invalid heartbeat supplied a binding: %+v, %v", fresh, err)
			}
		})
	}
	child.HookSeen.Value = true
	if fresh, err := mergeCandidate(b, child); err != nil || !fresh.Class.HookSeen || !fresh.HookSeen {
		t.Fatalf("classified hook registration did not promote the same child: %+v, %v", fresh, err)
	}
}

func TestUnclassifiedFullHeartbeatRefreshesPresentMetadata(t *testing.T) {
	b := Bound{
		ExecutionID: "ex_retained", Generation: 2, SessionID: "native", Rung: 2, HookSeen: true,
		RepositoryID: "rp_original", Agent: hubclient.Agent{ID: "ag_stable", Name: "before"},
		Worktree: "/repo", Capabilities: "shell_scope", Outside: true,
	}
	candidate := decodeBindingCandidate(t, `{"execution_id":"ex_retained","generation":2,"repository_id":"rp_after","agent":{"name":"after"},"capabilities":[]}`)
	want := b
	want.RepositoryID, want.Agent.Name, want.Capabilities, want.Outside = "rp_after", "after", "", false
	if fresh, err := mergeCandidate(b, candidate); err != nil || fresh != want {
		t.Fatalf("legacy reply failed to update named metadata while retaining omitted identity: %+v, %v", fresh, err)
	}
}
