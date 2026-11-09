//go:build linux

package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type codexSessionHub struct {
	root   string
	client *hubclient.Client
	getenv func(string) string
	anc    proc.Ancestor
}

func newCodexSessionHub(t *testing.T, command string) *codexSessionHub {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin)
	child.Args = []string{"codex", command}
	child.Env = append(os.Environ(), "IRIS_TEST_CODEX_PROCESS=1")
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("Codex process helper: %v", err)
		}
	})
	database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	t.Cleanup(server.Close)
	token := tokenFile(t)
	getenv := func(key string) string {
		switch key {
		case "IRIS_HUB_URL":
			return server.URL
		case "IRIS_TOKEN_FILE":
			return token
		}
		return ""
	}
	root := t.TempDir()
	if output, err := exec.Command("git", "init", "--initial-branch=main", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return &codexSessionHub{
		root: root, client: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}}, getenv: getenv,
		// Both surfaces receive the same controlled process identity. Its live
		// command line, rather than a mocked single-session decision, is checked.
		anc: proc.Ancestor{PID: child.Process.Pid, StartMS: 77, Name: "codex"},
	}
}

func (h *codexSessionHub) hook(t *testing.T, fixture, event, session string) Output {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex", fixture))
	if err != nil {
		t.Fatal(err)
	}
	captured, err := Decode(ClientCodex, event, raw)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	// Captures remain untouched on disk. Shell and edit captures came from
	// different runs, so give them this test session's identity in memory.
	payload = rewriteCapturedPaths(payload, captured.CWD, filepath.ToSlash(h.root)).(map[string]any)
	payload["session_id"] = session
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	out := (Runner{
		ClientName: "codex", EventName: event, Stdin: bytes.NewReader(raw), Hub: h.client, Getenv: h.getenv,
		Host: func() string { return "testhost" }, Ancestor: func() (proc.Ancestor, error) { return h.anc, nil },
		after: never, logAfter: never, ShellDeadline: time.Minute,
	}).Run()
	if failures, err := ReadRecentFailures(h.getenv("IRIS_TOKEN_FILE"), 20); err != nil || len(failures) != 0 {
		t.Fatalf("hook failed open: %+v %v; stdout=%s stderr=%s", failures, err, out.Stdout, out.Stderr)
	}
	return out
}

func (h *codexSessionHub) mcp() *binding.Resolver {
	return &binding.Resolver{
		Hub: h.client, Client: "codex", Host: "testhost", CWD: h.root, Getenv: h.getenv,
		Ancestor: func() (proc.Ancestor, error) { return h.anc, nil },
		// Skip only the startup grace, so the regression is bounded. The same
		// resolve/register/adopt code runs as after the production 15 s grace.
		Grace: 0,
	}
}

func (h *codexSessionHub) start(t *testing.T, session string) {
	t.Helper()
	out := h.hook(t, "SessionStart.json", "SessionStart", session)
	if out.Exit != 0 || len(out.Stderr) != 0 || !bytes.Contains(out.Stdout, []byte("Iris handle: ex_")) {
		t.Fatalf("session start did not register: stdout=%s stderr=%s exit=%d", out.Stdout, out.Stderr, out.Exit)
	}
}

func (h *codexSessionHub) registered(t *testing.T, session string) binding.Bound {
	t.Helper()
	var b binding.Bound
	if err := h.client.Do(context.Background(), "POST", "/v1/bind", map[string]any{"client": "codex", "host": "testhost", "session_id": session}, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *codexSessionHub) claims(t *testing.T, repo string) []hubclient.Claim {
	t.Helper()
	var out struct{ Claims []hubclient.Claim }
	if err := h.client.Do(context.Background(), "GET", "/v1/claims?repository_id="+url.QueryEscape(repo), nil, &out); err != nil {
		t.Fatal(err)
	}
	return out.Claims
}

func (h *codexSessionHub) active(t *testing.T, repo string) int {
	t.Helper()
	var out struct{ Agents []hubclient.Peer }
	if err := h.client.Do(context.Background(), "GET", "/v1/agents?repository_id="+url.QueryEscape(repo), nil, &out); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range out.Agents {
		if a.State == "active" {
			n++
		}
	}
	return n
}

func TestCodexExecMCPClaimIsItsHooksOwnClaim(t *testing.T) {
	h := newCodexSessionHub(t, "exec")
	h.start(t, "session")
	hook := h.registered(t, "session")
	mcp, err := h.mcp().Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if mcp.ExecutionID != hook.ExecutionID || mcp.Agent.ID != hook.Agent.ID || mcp.Rung != 3 {
		t.Errorf("MCP %+v differs from its hook %+v", mcp, hook)
	}
	var claimed struct{ Fence int64 }
	if err = h.client.Do(context.Background(), "POST", "/v1/claims", map[string]any{
		"execution_id": mcp.ExecutionID, "generation": mcp.Generation, "key": "planned", "kind": "exact", "path": "allowed.txt",
	}, &claimed); err != nil {
		t.Fatal(err)
	}
	out := h.hook(t, "PreToolUse-apply_patch.json", "PreToolUse", "session")
	if denied(ClientCodex, out) {
		t.Errorf("MCP's explicit claim refused its own edit: %s", out.Stdout)
	}
	claims := h.claims(t, hook.RepositoryID)
	if len(claims) != 1 || claims[0].Fence != claimed.Fence || claims[0].Origin != "explicit" || claims[0].Holder != hook.Agent.Name {
		t.Errorf("own edit changed the explicit claim: %+v; fence=%d", claims, claimed.Fence)
	}
	if n := h.active(t, hook.RepositoryID); n != 1 {
		t.Errorf("one Codex session registered %d active agents", n)
	}
	// A split also suppresses shell attribution as though two unrelated sessions
	// shared the worktree. One session must still claim its Code Mode write.
	assertAllow(t, ClientCodex, h.hook(t, "PreToolUse-node_repl_js.json", "PreToolUse", "session"))
	if err = os.WriteFile(filepath.Join(h.root, "free.txt"), []byte("viajs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assertAllow(t, ClientCodex, h.hook(t, "PostToolUse-node_repl_js.json", "PostToolUse", "session"))
	claims = h.claims(t, hook.RepositoryID)
	found := false
	for _, c := range claims {
		if c.Path == "free.txt" && c.Origin == "auto" && c.Holder == hook.Agent.Name {
			found = true
		}
	}
	if !found {
		t.Errorf("single-session shell edit was not attributed: %+v", claims)
	}
}

func TestCodexAppServerRequiresExplicitSessionIdentity(t *testing.T) {
	h := newCodexSessionHub(t, "app-server")
	h.start(t, "session")
	hook := h.registered(t, "session")
	r := h.mcp()
	if b, err := r.Resolve(context.Background(), ""); !binding.IsCode(err, "unbound") || b.ExecutionID != "" {
		t.Fatalf("multiplexed app-server bound without a session handle: %+v %v", b, err)
	}
	if b, err := r.Resolve(context.Background(), hook.ExecutionID); err != nil || b.ExecutionID != hook.ExecutionID || b.Rung != 4 {
		t.Fatalf("explicit app-server handle: %+v %v", b, err)
	}
	if n := h.active(t, hook.RepositoryID); n != 1 {
		t.Fatalf("unbound app-server allocated a fallback: %d active agents", n)
	}
}

func TestCodexAmbiguousProcessSessionsAreNotMerged(t *testing.T) {
	h := newCodexSessionHub(t, "exec")
	h.start(t, "one")
	h.start(t, "two")
	one, two := h.registered(t, "one"), h.registered(t, "two")
	if one.ExecutionID == two.ExecutionID || one.Agent.ID == two.Agent.ID {
		t.Fatal("distinct session ids merged")
	}
	if b, err := h.mcp().Resolve(context.Background(), ""); !binding.IsCode(err, "ambiguous") || b.ExecutionID != "" {
		t.Fatalf("ambiguous process sessions should stay unbound: %+v %v", b, err)
	}
	if n := h.active(t, one.RepositoryID); n != 2 {
		t.Fatalf("ambiguous sessions allocated a fallback: %d active agents", n)
	}
}

func TestCodexLateHookAdoptsFallbackExecution(t *testing.T) {
	h := newCodexSessionHub(t, "exec")
	r := h.mcp()
	fallback, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	h.start(t, "session")
	hook := h.registered(t, "session")
	if hook.ExecutionID == fallback.ExecutionID {
		t.Fatal("test did not establish a late registration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Maintain(ctx, "shim", 5*time.Millisecond, func() bool { return true }, func() {})
	}()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := r.Resolve(context.Background(), "")
		if err == nil && b.ExecutionID == hook.ExecutionID && h.active(t, hook.RepositoryID) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("late hook was never adopted: bound=%+v err=%v hook=%+v fallback=%+v", b, err, hook, fallback)
		}
	}
}
