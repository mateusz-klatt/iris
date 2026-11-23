package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
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

func TestMCPVetoesEndedGenerationOnRestoredBackup(t *testing.T) {
	const nativeSession = "ended-restore-e2e"
	const credential = "restore-test-credential"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	clock := &mcpRestoreClock{base: time.Now()}
	dbPath := filepath.Join(dir, "hub.db")
	live, err := store.Open(dbPath, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	call := func(s *store.Store, op string, req store.Request) map[string]any {
		t.Helper()
		out, err := s.Call(ctx, op, req)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}
	registered := call(live, "sessions", store.Request{Client: "claude", Host: "test-host", SessionID: nativeSession, Cwd: dir, Worktree: dir, Source: "hook"})
	identity := store.Request{ExecutionID: registered["execution_id"].(string), Generation: registered["generation"].(int64)}
	claimsRequest := store.Request{RepositoryID: registered["repository_id"].(string)}
	claimRequest := identity
	claimRequest.Key, claimRequest.Kind, claimRequest.Path = "before-backup", "exact", "held.go"
	oldFence := call(live, "claim", claimRequest)["fence"]
	backupPath := filepath.Join(dir, "backup.db")
	if err := adminCommand(ctx, nil, []string{"backup", dbPath, backupPath}, io.Discard); err != nil {
		t.Fatal(err)
	}
	call(live, "end", identity)
	if claims := call(live, "claims", claimsRequest)["claims"].([]map[string]any); len(claims) != 0 {
		t.Fatal("SessionEnd did not revoke the original claim")
	}
	tokenPath := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(tokenPath, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.Mkdir(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The only local memory left after SessionEnd is the end marker.
	sum := sha256.Sum256([]byte("claude\x00" + nativeSession))
	marker := filepath.Join(sessionsDir, hex.EncodeToString(sum[:16])+".ended")
	if err := os.WriteFile(marker, fmt.Appendf(nil, "%d\n", identity.Generation), 0o600); err != nil {
		t.Fatal(err)
	}

	restored, err := store.Open(backupPath, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if !restored.Restored() {
		t.Fatal("hub did not start on a marked backup")
	}
	assertRecoveryReservation := func() {
		t.Helper()
		_, err := restored.Call(ctx, "validate", identity)
		var refusal *store.Error
		if !errors.As(err, &refusal) || refusal.Code != "lost" {
			t.Fatalf("old Execution usable during recovery: %v", err)
		}
		if claims := call(restored, "claims", claimsRequest)["claims"].([]map[string]any); len(claims) != 1 || claims[0]["fence"] != oldFence {
			t.Fatalf("old claim did not remain a recovery reservation: %v", claims)
		}
	}
	assertRecoveryReservation()

	server := httptest.NewUnstartedServer(nil)
	h := hub.New(restored, credential, []string{server.Listener.Addr().String()})
	var binds, mutations atomic.Int64
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/bind":
			binds.Add(1)
		case strings.HasPrefix(r.URL.Path, "/v1/sessions"), strings.HasSuffix(r.URL.Path, "/heartbeat"):
			mutations.Add(1)
		}
		h.ServeHTTP(w, r)
	})
	server.Start()
	t.Cleanup(server.Close)
	env := map[string]string{"IRIS_HUB_URL": server.URL, "IRIS_TOKEN_FILE": tokenPath, "CLAUDE_CODE_SESSION_ID": nativeSession}
	getenv := func(key string) string { return env[key] }
	config := (hubclient.EnvConfig{Getenv: getenv, DefaultTokenFile: tokenPath}).Load()
	resolver := binding.New(&hubclient.Client{Config: config}, "claude")
	resolver.Host, resolver.CWD, resolver.Getenv, resolver.Ancestor, resolver.Grace = "test-host", dir, getenv, nil, 0
	if _, err := bindingSessions(resolver, config); err != nil {
		t.Fatal(err)
	}
	// Run one complete idle tick, including both the bind attempt and possible renewal.
	ticks := 0
	resolver.Maintain(ctx, "shim", time.Millisecond, func() bool {
		ticks++
		return ticks == 1
	}, func() {})
	if bound, _, err := resolver.ResolveCall(ctx, "", "", ""); !binding.IsCode(err, "lost") || bound.ExecutionID != "" {
		t.Fatalf("shim resolved the ended Execution: %+v, %v", bound, err)
	}
	if binds.Load() < 2 || mutations.Load() != 0 {
		t.Fatalf("shim made %d binding lookups and %d registration or heartbeat requests", binds.Load(), mutations.Load())
	}
	assertRecoveryReservation()

	clock.elapsed.Store((121 * time.Second).Milliseconds())
	if claims := call(restored, "claims", claimsRequest)["claims"].([]map[string]any); len(claims) != 0 {
		t.Fatalf("old claim survived the recovery deadline: %v", claims)
	}
}

type mcpRestoreClock struct {
	base    time.Time
	elapsed atomic.Int64
}

func (c *mcpRestoreClock) Tick() int64 { return c.elapsed.Load() }
func (c *mcpRestoreClock) Now() time.Time {
	return c.base.Add(time.Duration(c.Tick()) * time.Millisecond)
}
