package hook

import (
	"context"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type reconcileHub struct{ wait bool }

func (reconcileHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	return hubclient.SessionResponse{ExecutionID: "shell", Generation: 1}, nil
}
func (reconcileHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{Action: "allow"}, nil
}
func (reconcileHub) EndExecution(context.Context, string, int) error { return nil }
func (h reconcileHub) Reconcile(ctx context.Context, r hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	if h.wait {
		<-ctx.Done()
		return hubclient.CheckResponse{}, ctx.Err()
	}
	return hubclient.CheckResponse{Message: "Shell edit detected. Iris detects these changes and cannot undo them."}, nil
}

func TestShellContextEveryClient(t *testing.T) {
	for _, client := range Clients {
		t.Run(string(client), func(t *testing.T) {
			r := Runner{Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Hub: reconcileHub{}, Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{"path": "??"}, nil
			}, ShellDeadline: time.Hour}
			ev := Event{Client: client, Kind: KindPostTool, ToolClass: ClassShell, SessionID: "shell", CWD: "/repo"}
			out := r.reconcile(context.Background(), ev, &runState{})
			if out.Exit != 0 || !strings.Contains(string(out.Stdout), "cannot undo") {
				t.Fatalf("missing context: %#v", out)
			}
		})
	}
}
func TestShellDeadlineAndNonShellCost(t *testing.T) {
	r := Runner{ClientName: "claude", EventName: "PostToolUse", Hub: reconcileHub{wait: true}, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		return map[string]string{}, nil
	}}
	r.Stdin = strings.NewReader(`{"session_id":"shell","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"echo ok"}}`)
	// The hub never answers and the hook's own deadline never fires, so only the shell
	// budget can end this call. If it did not, the test would hang until the binary's timeout.
	r.after = never
	out := r.Run()
	assertAllow(t, ClientClaude, out)
	r.after = nil
	r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		t.Error("non-shell scanned Git status")
		return nil, nil
	}
	r.Stdin = strings.NewReader(`{"session_id":"shell","cwd":"/repo","tool_name":"Edit","tool_input":{"file_path":"/repo/a"}}`)
	assertAllow(t, ClientClaude, r.Run())
}

func TestShellStatusAgainstRealHub(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %v: %s", err, out)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: hubURL(server.URL)}
	// Functional integration does not impose a machine-speed assertion. The separate
	// deadline test proves the Runner's 300 ms fail-open boundary under stalled I/O.
	// Setup against the real hub is not timed. The test binary's own timeout bounds a hang.
	ctx := context.Background()
	holder, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "codex", SessionID: "holder", Host: "testhost", CWD: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/shell.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Touch(ctx, hubclient.TouchRequest{ExecutionID: holder.ExecutionID, Generation: holder.Generation, ToolCallID: "held", Paths: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	shell, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "claude", SessionID: "shell", Host: "testhost", CWD: root, Worktree: root, RemoteURL: "https://example.com/team/shell.git"})
	if err != nil {
		t.Fatal(err)
	}
	request := hubclient.ReconcileRequest{ExecutionID: shell.ExecutionID, Generation: shell.Generation, Kind: "before"}
	request.Status, err = gitinfo.Status(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "shared"), []byte("shell edit"), 0600); err != nil {
		t.Fatal(err)
	}
	request.Kind = "after"
	request.Status, err = gitinfo.Status(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.Reconcile(ctx, request)
	if err != nil || !strings.Contains(out.Message, "Shell edit detected") {
		t.Fatalf("missing shell conflict: %#v %v", out, err)
	}
}

// Codex's Code Mode js tool can write any file, and Iris cannot tell what from the code, so a
// real capture of it must take the shell path: reconciled before and after, not passed as an
// MCP call that touches nothing.
func TestCodexJavaScriptToolIsReconciledLikeAShell(t *testing.T) {
	for _, name := range []string{"PreToolUse-node_repl_js.json", "PostToolUse-node_repl_js.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "codex", name))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := codexAdapter{}.Decode(strings.TrimSuffix(strings.SplitN(name, "-", 2)[0], ".json"), raw)
		if err != nil {
			t.Fatal(err)
		}
		if ev.ToolClass != ClassShell || !shellEvent(ev) || !strings.Contains(ev.Command, "fs.appendFile") {
			t.Fatalf("%s: class %q shell %v command %q", name, ev.ToolClass, shellEvent(ev), ev.Command)
		}
	}
}
