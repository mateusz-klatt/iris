package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPStdioReattachesRecoveringSessionFromHookMemory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	s, err := store.Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	var serving atomic.Pointer[hub.Hub]
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serving.Load().ServeHTTP(w, r)
	}))
	serving.Store(hub.New(s, "secret", []string{httpServer.Listener.Addr().String()}))
	httpServer.Start()
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	reg, err := s.Call(ctx, "sessions", store.Request{Client: "claude", Host: host, SessionID: "reattach-e2e", Cwd: cwd, Worktree: cwd, Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}
	// Restart hub so the hook-registered session is recovering inside its 120 s window.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	serving.Store(hub.New(s, "secret", []string{httpServer.Listener.Addr().String()}))

	// Write the hook's remembered generation beside the token file.
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("claude\x00reattach-e2e"))
	if err := os.WriteFile(filepath.Join(sessDir, hex.EncodeToString(sum[:16])), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestMCPStdio$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_MCP_CHILD=1", "IRIS_EXECUTION=", "CLAUDE_CODE_SESSION_ID=reattach-e2e", "IRIS_CHILD_HUB_URL="+httpServer.URL, "IRIS_CHILD_TOKEN_FILE="+token)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("whoami failed on recovering session: %+v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["execution_id"] != reg["execution_id"] || out["generation"] != float64(1) {
		t.Fatalf("did not reattach recovering execution: %+v", out)
	}
}
