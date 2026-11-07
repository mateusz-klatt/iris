package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// iris doctor --live points a client at a scratch hub with IRIS_HUB_URL and
// IRIS_TOKEN_FILE alone. That is sound only if both variables win over every
// client.json, for the hook and for iris mcp, which read configuration separately.
func TestHubVariablesOverrideClientJSON(t *testing.T) {
	var trapped atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trapped.Add(1)
		http.Error(w, "configured hub", http.StatusTeapot)
	}))
	defer trap.Close()
	writeTrap := func(dir string) {
		t.Helper()
		if err := hubclient.WriteConfig(filepath.Join(dir, "client.json"), hubclient.FileConfig{Hub: trap.URL, TokenFile: filepath.Join(dir, "trap.token")}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "trap.token"), []byte("trap"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	writeTrap(filepath.Join(configDir, "iris"))
	defer func() { _ = os.RemoveAll(filepath.Join(configDir, "iris")) }()
	// A client.json beside the token file is read too, so it must lose as well.
	tokenDir := t.TempDir()
	writeTrap(tokenDir)
	token := filepath.Join(tokenDir, "hub.token")
	if err = os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	scratch := httptest.NewUnstartedServer(nil)
	scratch.Config.Handler = hub.New(s, "secret", []string{scratch.Listener.Addr().String()})
	scratch.Start()
	defer scratch.Close()

	repo := t.TempDir()
	if err = os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": repo, "tool_name": "Edit", "tool_use_id": "t", "tool_input": map[string]string{"file_path": filepath.Join(repo, "a.go")}})
	runEdit := func(vars map[string]string) []byte {
		t.Helper()
		var out, errText bytes.Buffer
		if code := runHookIO([]string{"--client", "claude", "--event", "PreToolUse"}, bytes.NewReader(payload), &out, &errText, func(key string) string { return vars[key] }, hook.DefaultDeadline); code != 0 {
			t.Fatalf("hook exit %d: %s", code, errText.String())
		}
		return out.Bytes()
	}
	runEdit(map[string]string{"IRIS_HUB_URL": scratch.URL, "IRIS_TOKEN_FILE": token})
	if trapped.Load() != 0 {
		t.Fatal("the hook reached the configured hub")
	}
	claims, err := s.Call(context.Background(), "status", store.Request{})
	if err != nil || claims["counts"].(map[string]int)["Claim"] != 1 {
		t.Fatalf("the hook did not claim on the scratch hub: %v %v", claims, err)
	}
	// Control: without the URL, the client.json beside the token file is used.
	runEdit(map[string]string{"IRIS_TOKEN_FILE": token})
	if trapped.Load() == 0 {
		t.Fatal("the trap was never live, so the first check proves nothing")
	}
	trapped.Store(0)

	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestMCPStdio$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_MCP_CHILD=1", "IRIS_EXECUTION=", "CLAUDE_CODE_SESSION_ID=env-override-test", "IRIS_CHILD_HUB_URL="+scratch.URL, "IRIS_CHILD_TOKEN_FILE="+token)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("whoami: %+v %v", result, err)
	}
	if trapped.Load() != 0 {
		t.Fatal("iris mcp reached the configured hub")
	}
	status, _ := s.Call(ctx, "status", store.Request{})
	if status["counts"].(map[string]int)["Execution"] < 2 {
		t.Fatalf("whoami was not bound on the scratch hub: %v", status)
	}
}
