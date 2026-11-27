package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPSuccessionReloadChecksExactEvidence(t *testing.T) {
	const nativeSession = "mcp-succession-reload"
	const credential = "test-mcp-credential"
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.Call(t.Context(), "sessions", store.Request{
		Client: "claude", Host: host, SessionID: nativeSession, Source: "hook", Cwd: cwd, Worktree: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := row["execution_id"].(string)
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(token, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.Mkdir(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("claude\x00" + nativeSession))
	genPath := filepath.Join(sessionsDir, fmt.Sprintf("%x", sum[:16]))
	if err := os.WriteFile(genPath, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exact, err := sessionproof.EncodeRecord(sessionproof.Record{
		Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: 1, ExecutionID: execution,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(genPath+".execution", exact, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var beats []string
	server := httptest.NewUnstartedServer(nil)
	h := hub.New(db, credential, []string{server.Listener.Addr().String()})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/executions/"+execution+"/heartbeat":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			mu.Lock()
			beats = append(beats, string(raw))
			mu.Unlock()
		case r.URL.Path != "/v1/bind":
			t.Errorf("unexpected MCP request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})
	server.Start()
	t.Cleanup(server.Close)
	t.Setenv("IRIS_TOKEN_FILE", token)
	t.Setenv("IRIS_HUB_URL", "")
	t.Setenv("IRIS_EXECUTION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", nativeSession)
	configPath := filepath.Join(dir, "client.json")
	writeConfig := func(enabled bool, hubURL string) {
		t.Helper()
		if err := hubclient.WriteConfig(configPath, hubclient.FileConfig{Hub: hubURL, SessionSuccession: &enabled}); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(false, server.URL)
	session, _ := connectMCPCommand(t, []string{"--client", "claude"}, "test")
	whoami := func(wantSuccess bool) {
		t.Helper()
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if wantSuccess {
			out, ok := res.StructuredContent.(map[string]any)
			if res.IsError || !ok || out["execution_id"] != execution {
				t.Fatalf("healthy local evidence did not bind: %+v", res)
			}
		} else if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"code":"lost"`) {
			t.Fatalf("damaged exact evidence did not veto whoami: %+v", res)
		}
	}
	whoami(true)
	writeConfig(true, "http://127.0.0.1:1")
	whoami(true)
	if err := os.WriteFile(genPath+".execution", []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	whoami(false)
	writeConfig(false, "http://127.0.0.1:1")
	whoami(true)

	off := `{"generation":1,"source":"shim"}`
	on := `{"generation":1,"source":"shim","succession_proof":true}`
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(beats, []string{off, on, off}) {
		t.Fatalf("heartbeat bytes through OFF → ON → damage → OFF: %v", beats)
	}
}
