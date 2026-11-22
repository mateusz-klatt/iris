package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCrossLoadedMCPStaysUpButOnlyAnswersClientMismatch(t *testing.T) {
	token, requests := mcpWakeTrap(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "inherited-from-an-outer-claude")
	original := mcpAncestor
	t.Cleanup(func() { mcpAncestor = original })
	mcpAncestor = func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: 42, StartMS: 7, Name: "cursor-agent"}, nil
	}
	session, done := connectMCPCommand(t, []string{"--client", "claude"}, "2026.11.22-e9042b2")
	for range 2 {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError {
			t.Fatalf("cross-loaded whoami succeeded: %+v", result.StructuredContent)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Error hubclient.Error `json:"error"`
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out.Error.Code != "client_mismatch" || !strings.Contains(out.Error.Message, "native Iris MCP server (`iris mcp --client cursor`)") {
			t.Fatalf("tool error: %+v", out.Error)
		}
	}
	_ = session.Close()
	waitMCPCommand(t, done)
	if requests.Load() != 0 {
		t.Fatalf("cross-loaded MCP made %d hub requests", requests.Load())
	}
	want := binding.ClientMismatchObservation("claude", "cursor")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		entries, err := hook.ReadRecentObservations(token, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 && entries[0].Client == "claude" && entries[0].Event == "mcp" && entries[0].Error == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup observations: %+v; want exactly %q", entries, want)
		}
	}
}
