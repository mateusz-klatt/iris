//go:build compat

package hubclient

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
)

// TestCompatM2dDecoderDriver is built in the v0.1.15 hubclient package by the
// live M2d tests. The test requests the classified form so the old response
// types decode the extra fields of an actual stage-1b response.
func TestCompatM2dDecoderDriver(t *testing.T) {
	tokenFile := os.Getenv("IRIS_COMPAT_DECODER_TOKEN")
	if tokenFile == "" {
		t.Skip("invoked by the live M2d tests")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	h := &Client{Config: EnvConfig{
		Getenv:           func(string) string { return "" },
		DefaultTokenFile: tokenFile,
	}}
	request := map[string]any{
		"client": "claude", "host": "compat-host", "session_id": "m2d-legacy-decoder",
		"cwd": cwd, "worktree": cwd, "source": "hook", "succession_proof": true,
	}
	var raw json.RawMessage
	if err := h.Do(t.Context(), http.MethodPost, "/v1/sessions", request, &raw); err != nil {
		t.Fatalf("classified session response: %v", err)
	}
	assertCompatM2dProvenance(t, raw)
	var registered SessionResponse
	if err := json.Unmarshal(raw, &registered); err != nil {
		t.Fatalf("legacy session decoder: %v", err)
	}
	if registered.Generation != 1 || registered.ExecutionID == "" || registered.Handle != registered.ExecutionID ||
		registered.RepositoryID == "" || registered.LeaseMS <= 0 || !slices.Contains(registered.Capabilities, "succession_proof") {
		t.Fatalf("legacy session decoder lost required fields or the classified capability: %+v", registered)
	}
	request["generation"], request["kind"], request["status"] = registered.Generation, "before", map[string]string{}
	if err := h.Do(t.Context(), http.MethodPost, "/v1/shell/reconcile", request, &raw); err != nil {
		t.Fatalf("classified combined response: %v", err)
	}
	var envelope struct {
		Session json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	assertCompatM2dProvenance(t, envelope.Session)
	var reconciled ShellResponse
	if err := json.Unmarshal(raw, &reconciled); err != nil {
		t.Fatalf("legacy combined decoder: %v", err)
	}
	if reconciled.Session.ExecutionID != registered.ExecutionID || reconciled.Session.Generation != registered.Generation ||
		!slices.Contains(reconciled.Session.Capabilities, "succession_proof") || reconciled.Attention == nil || len(reconciled.Conflicts) != 0 {
		t.Fatalf("legacy combined decoder lost required fields or the classified capability: %+v", reconciled)
	}
}

func assertCompatM2dProvenance(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"session_id": `"m2d-legacy-decoder"`, "prior_execution_id": "null", "creation_source": `"hook"`,
		"creation_proof": `"initial"`, "hook_seen": "true",
	} {
		if got := string(fields[key]); got != want {
			t.Errorf("classified reply %s = %q, want %q: %s", key, got, want, raw)
		}
	}
}
