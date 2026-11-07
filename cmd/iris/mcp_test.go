package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPStdio(t *testing.T) {
	if os.Getenv("IRIS_TEST_MCP_CHILD") == "1" {
		// TestMain clears inherited credentials. The parent passed the fixture
		// hub through names TestMain does not touch.
		_ = os.Setenv("IRIS_HUB_URL", os.Getenv("IRIS_CHILD_HUB_URL"))
		_ = os.Setenv("IRIS_TOKEN_FILE", os.Getenv("IRIS_CHILD_TOKEN_FILE"))
		if err := runMCP([]string{"--client", "claude"}); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	httpServer := httptest.NewUnstartedServer(nil)
	httpServer.Config.Handler = hub.New(s, "secret", []string{httpServer.Listener.Addr().String()})
	httpServer.Start()
	defer httpServer.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(token, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestMCPStdio$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_MCP_CHILD=1", "IRIS_EXECUTION=", "CLAUDE_CODE_SESSION_ID=mcp-only-test", "COPILOT_AGENT_SESSION_ID=wrong-inherited", "IRIS_CHILD_HUB_URL="+httpServer.URL, "IRIS_CHILD_TOKEN_FILE="+token)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	status, _ := s.Call(ctx, "status", store.Request{})
	if status["counts"].(map[string]int)["Execution"] != 0 {
		t.Fatal("binding was not lazy")
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 10 {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s: %+v", name, result.Content)
		}
		raw, _ := json.Marshal(result.StructuredContent)
		out := map[string]any{}
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	own := call("whoami", map[string]any{})
	if own["binding_rung"] != float64(2) {
		t.Fatal(own)
	}
	name := own["agent"].(map[string]any)["name"].(string)
	call("peers", map[string]any{})
	sent := call("send", map[string]any{"to": []string{name}, "body": "UNTRUSTED secret", "subject": "test"})
	if sent["key"] == nil || sent["thread_id"] == nil {
		t.Fatal(sent)
	}
	inbox := call("inbox", map[string]any{})
	delivery := inbox["deliveries"].([]any)[0].(map[string]any)
	if delivery["body"] != nil || delivery["untrusted_data"] == nil {
		t.Fatal(delivery)
	}
	call("ack", map[string]any{"delivery_id": delivery["id"], "attempt": delivery["attempt"]})
	claim := call("claim", map[string]any{"path": "test.go", "kind": "exact"})
	call("claims", map[string]any{})
	call("release", map[string]any{"fence": claim["fence"], "token": claim["token"]})
	call("watch_ticket", map[string]any{})
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPStdinEOF(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPStdio$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_MCP_CHILD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	if err = stdin.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MCP did not stop on stdin EOF while its parent remained alive")
	}
}
