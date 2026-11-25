package hook

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type hookCompatClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func (c *hookCompatClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *hookCompatClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}

func (c *hookCompatClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.tick += int64(d / time.Millisecond)
}

func TestV0115HookRunnerUnlocksHooklessSuccessionChildOnStage1bHub(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--initial-branch=main", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	homeDir := t.TempDir()

	clock := &hookCompatClock{wall: time.Unix(1800000000, 0), tick: 1000}
	s, err := store.Open(filepath.Join(t.TempDir(), "hook.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = hub.New(s, "secret", []string{srv.Listener.Addr().String()})
	srv.Start()
	defer srv.Close()

	tokenFile := filepath.Join(homeDir, "hub.token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		switch k {
		case "IRIS_HUB_URL":
			return srv.URL
		case "IRIS_TOKEN_FILE":
			return tokenFile
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "LOCALAPPDATA":
			return homeDir
		}
		return ""
	}
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}}

	rawFixture, err := os.ReadFile(filepath.Join("testdata", "claude", "PreToolUse-write.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rawFixture, &payload); err != nil {
		t.Fatal(err)
	}
	sessionID := payload["session_id"].(string)
	payload["cwd"] = root
	toolInput := payload["tool_input"].(map[string]any)
	toolInput["file_path"] = filepath.Join(root, "allowed.txt")
	rewritten, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	// Register initial parent as hook, expire it, then create a hookless shim-succession child.
	parent, err := client.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: string(ClientClaude), Host: "testhost", SessionID: sessionID,
		CWD: root, Worktree: root, Branch: "main", Source: "hook",
		ClientPID: 901, ClientPIDStartMS: 9901,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	child, err := client.Succeed(t.Context(), hubclient.SucceedRequest{
		Client: string(ClientClaude), Host: "testhost", SessionID: sessionID, Source: "shim",
		PriorGeneration: parent.Generation, PriorExecutionID: parent.ExecutionID,
		ClientPID: 901, ClientPIDStartMS: 9901,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Before hook fires, bind without succession_proof is refused.
	if _, err := client.Bind(t.Context(), hubclient.BindRequest{
		Client: string(ClientClaude), Host: "testhost", Execution: child.ExecutionID,
	}); !hubclient.IsCode(err, "lost") {
		t.Fatalf("expected lost before hook arrival, got %v", err)
	}

	// Genuine v0.1.15 hook execution runs cleanly against stage-1b hub, attaches to the child,
	// sets hook_seen=true, and claims allowed.txt.
	never := func(time.Duration) <-chan time.Time { return nil }
	out := Runner{
		ClientName: string(ClientClaude),
		EventName:  "PreToolUse",
		Stdin:      bytes.NewReader(rewritten),
		Getenv:     getenv,
		Hub:        client,
		Host:       func() string { return "testhost" },
		Ancestor:   func() (proc.Ancestor, error) { return proc.Ancestor{PID: 901, StartMS: 9901, Name: "claude"}, nil },
		after:      never,
		logAfter:   never,
	}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "deny") {
		t.Fatalf("unexpected hook output: %+v", out)
	}

	bound, err := client.Bind(t.Context(), hubclient.BindRequest{
		Client: string(ClientClaude), Host: "testhost", Execution: child.ExecutionID,
	})
	if err != nil {
		t.Fatalf("bind after hook arrival failed: %v", err)
	}
	cls, err := bound.Classify()
	if err != nil {
		t.Fatal(err)
	}
	if cls.Kind != hubclient.CandidateUnclassified || !cls.HookSeen {
		t.Fatalf("unexpected un-opted unlocked classification: %+v", cls)
	}

	optedBound, err := client.Bind(t.Context(), hubclient.BindRequest{
		Client: string(ClientClaude), Host: "testhost", Execution: child.ExecutionID, SuccessionProof: true,
	})
	if err != nil {
		t.Fatalf("opted bind after hook arrival failed: %v", err)
	}
	optedCls, err := optedBound.Classify()
	if err != nil {
		t.Fatal(err)
	}
	if optedCls.Kind != hubclient.CandidateShimSuccession || !optedCls.HookSeen || optedCls.PriorExecutionID != parent.ExecutionID {
		t.Fatalf("unexpected opted unlocked classification: %+v", optedCls)
	}
}
