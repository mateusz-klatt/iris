package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCrossLoadedCodexSuppressesWakeWorker(t *testing.T) {
	_, requests := mcpWakeTrap(t)
	originalAncestor, originalObserver, originalWorker := mcpAncestor, mcpObserver, newMCPWakeWorker
	t.Cleanup(func() {
		mcpAncestor, mcpObserver, newMCPWakeWorker = originalAncestor, originalObserver, originalWorker
	})
	mcpAncestor = func(string) func() (proc.Ancestor, error) {
		return func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 42, StartMS: 7, Name: "cursor-agent"}, nil
		}
	}
	observed := make(chan string, 1)
	mcpObserver = func(_ func(string) string, client string) func(string) {
		if client != "codex" {
			t.Errorf("observation client = %q", client)
		}
		return func(text string) { observed <- text }
	}
	var workers atomic.Int32
	newMCPWakeWorker = func(*binding.Resolver, *hook.Sessions, func() string, func(string)) mcpWakeWorker {
		workers.Add(1)
		return mcpWakeFunction(func(context.Context) {})
	}
	session, done := connectMCPCommand(t, []string{"--client", "codex"}, "0.167.0")
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
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
	if !result.IsError || out.Error.Code != "client_mismatch" || !strings.Contains(out.Error.Message, "iris mcp --client cursor") {
		t.Fatalf("Codex mismatch result: %+v", result)
	}
	_ = session.Close()
	waitMCPCommand(t, done)
	if workers.Load() != 0 || requests.Load() != 0 {
		t.Fatalf("started %d wake workers and made %d hub requests", workers.Load(), requests.Load())
	}
	select {
	case text := <-observed:
		if text != binding.ClientMismatchObservation("codex", "cursor") {
			t.Fatalf("observed %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Codex mismatch was not observed")
	}
}

func TestCrossLoadedMCPStaysUpButOnlyAnswersClientMismatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ancestor  string
		exe       string
		launcher  string
		markers   bool
		toolCalls int
	}{
		{name: "cursor ancestor", ancestor: "cursor-agent", toolCalls: 2},
		{name: "cursor environment only", ancestor: "helper", markers: true, toolCalls: 1},
		{name: "cursor agent executable with empty environment", ancestor: "agent", exe: "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node", toolCalls: 1},
		{name: "cursor launcher argument with empty environment", ancestor: "/Users/user/.local/bin/agent", exe: "/usr/local/bin/node", launcher: "/Users/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js", toolCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, requests := mcpWakeTrap(t)
			t.Setenv("CLAUDE_CODE_SESSION_ID", "inherited-from-an-outer-claude")
			if tc.markers {
				t.Setenv("CURSOR_PROJECT_DIR", "/scratch/project")
				t.Setenv("CURSOR_VERSION", "2026.11.22-e9042b2")
			}
			original := mcpAncestor
			t.Cleanup(func() { mcpAncestor = original })
			mcpAncestor = func(string) func() (proc.Ancestor, error) {
				return func() (proc.Ancestor, error) {
					return proc.Ancestor{PID: 42, StartMS: 7, Name: tc.ancestor, Exe: tc.exe, Launcher: tc.launcher}, nil
				}
			}
			session, done := connectMCPCommand(t, []string{"--client", "claude"}, "2026.11.22-e9042b2")
			mismatches := 0
			for range tc.toolCalls {
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
				mismatches++
			}
			if mismatches != tc.toolCalls {
				t.Fatalf("got %d client_mismatch results, want %d", mismatches, tc.toolCalls)
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
		})
	}
}

func TestUnidentifiedAgentMCPAssignsNoClient(t *testing.T) {
	token, requests := mcpWakeTrap(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "inherited-from-an-outer-claude")
	t.Setenv("IRIS_EXECUTION", "ex_inherited")
	original := mcpAncestor
	t.Cleanup(func() { mcpAncestor = original })
	mcpAncestor = func(string) func() (proc.Ancestor, error) {
		return func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 42, StartMS: 7, Name: "/opt/agent"}, nil
		}
	}
	session, done := connectMCPCommand(t, []string{"--client", "claude"}, "2026.11.22-e9042b2")
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
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
	if !result.IsError || out.Error.Code != "client_mismatch" || strings.Contains(out.Error.Message, "iris mcp --client") || !strings.Contains(out.Error.Message, "unidentified agent") {
		t.Fatalf("%+v", out.Error)
	}
	_ = session.Close()
	waitMCPCommand(t, done)
	if requests.Load() != 0 {
		t.Fatalf("unidentified agent made %d hub requests", requests.Load())
	}
	want := binding.UnidentifiedAgentObservation("claude")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		entries, err := hook.ReadRecentObservations(token, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 && entries[0].Error == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup observations: %+v; want %q", entries, want)
		}
	}
}

func TestGrokAgentAliasContactsTheHub(t *testing.T) {
	token, requests := mcpWakeTrap(t)
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_SESSION_ID", "native-grok")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "inherited-from-an-outer-claude")
	original := mcpAncestor
	t.Cleanup(func() { mcpAncestor = original })
	mcpAncestor = func(string) func() (proc.Ancestor, error) {
		return func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 42, StartMS: 7, Name: "agent", Exe: "/home/user/.grok/downloads/grok-1.0.92-linux-x86_64"}, nil
		}
	}
	session, done := connectMCPCommand(t, []string{"--client", "grok"}, "1.0.92")
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Error hubclient.Error `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if result.IsError && out.Error.Code == "client_mismatch" {
		t.Fatalf("Grok's agent was refused: %+v", out.Error)
	}
	_ = session.Close()
	waitMCPCommand(t, done)
	if requests.Load() == 0 {
		t.Fatalf("Grok's agent alias made no hub request: %+v", result)
	}
}
