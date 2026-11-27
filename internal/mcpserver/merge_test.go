package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWhoamiUsesAuthoritativeHeartbeatBinding(t *testing.T) {
	hub := &fakeHub{
		bind:  `{"execution_id":"ex","generation":3,"repository_id":"retired","agent":{"id":"agent","name":"original-name"}}`,
		reply: `{"execution_id":"ex","generation":3,"repository_id":"merged","agent":{"id":"agent","name":"original-name"},"lease_ms":60000}`,
	}
	session, _ := lostSession(t, hub, "claude", nil)
	out, failed := use(t, session, "whoami", map[string]any{"execution": "ex"})
	if failed || out["repository_id"] != "merged" || out["execution_id"] != "ex" || out["generation"] != float64(3) || out["binding_rung"] != float64(4) || out["agent"].(map[string]any)["name"] != "original-name" {
		t.Fatal("whoami returned its cached identity", out)
	}
}

func TestWhoamiRejectsInvalidHeartbeatBindings(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply string
	}{
		{"different Execution", `{"execution_id":"other","generation":3,"session_id":"s","repository_id":"wrong"}`},
		{"different generation", `{"execution_id":"ex","generation":4,"session_id":"s","repository_id":"wrong"}`},
		{"incomplete capable provenance", `{"execution_id":"ex","generation":3,"session_id":"s","repository_id":"wrong","capabilities":["succession_proof"],"hook_seen":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub := &fakeHub{
				bind:  `{"execution_id":"ex","generation":3,"session_id":"s","repository_id":"rp"}`,
				reply: test.reply,
			}
			session, resolver := lostSession(t, hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"})
			out, failed := use(t, session, "whoami", nil)
			if !failed || out["execution_id"] != nil {
				t.Fatalf("invalid renewal reached MCP: %+v", out)
			}
			if request := hub.last(t); request.method != http.MethodPost || request.path != "/v1/executions/ex/heartbeat" || request.body["generation"] != float64(3) {
				t.Fatalf("unexpected renewal request: %+v", request)
			}
			if resolver.Lost() {
				t.Fatal("an invalid reply confirmed hub loss")
			}
		})
	}
}

func TestWhoamiDiscardsRenewalAfterBindingReplacement(t *testing.T) {
	started := make(chan struct{})
	respond := make(chan struct{})
	hub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/bind":
			var in struct {
				ExecutionID string `json:"execution"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "generation": 3, "repository_id": "rp"})
		case "/v1/executions/first/heartbeat":
			close(started)
			select {
			case <-respond:
				_, _ = w.Write([]byte(`{"execution_id":"first","generation":3,"repository_id":"retired","lease_ms":60000}`))
			case <-req.Context().Done():
			}
		case "/v1/executions/second/heartbeat":
			_, _ = w.Write([]byte(`{"lease_ms":60000}`))
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	session, resolver := lostSession(t, hub, "claude", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{"execution": "first"}})
		done <- outcome{result, err}
	}()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("whoami ended before renewal: %+v, %v", result.result, result.err)
	case <-ctx.Done():
		t.Fatal("whoami did not start renewal")
	}
	if b, err := resolver.Resolve(ctx, "second"); err != nil || b.ExecutionID != "second" {
		t.Fatalf("replace binding during renewal: %+v, %v", b, err)
	}
	close(respond)
	select {
	case late := <-done:
		if late.err != nil {
			t.Fatal(late.err)
		}
		assertLostResult(t, late.result, nil)
	case <-ctx.Done():
		t.Fatal("whoami did not discard its late renewal")
	}
	if out, failed := use(t, session, "whoami", nil); failed || out["execution_id"] != "second" || out["repository_id"] != "rp" {
		t.Fatalf("late renewal clobbered the replacement: %+v", out)
	}
}
