package hook

import (
	"context"
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
			r := Runner{Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Hub: reconcileHub{}, Status: func(context.Context, string) (map[string]string, error) { return map[string]string{"path": "??"}, nil }}
			ev := Event{Client: client, Kind: KindPostTool, ToolClass: ClassShell, SessionID: "shell", CWD: "/repo"}
			out := r.reconcile(context.Background(), ev)
			if out.Exit != 0 || !strings.Contains(string(out.Stdout), "cannot undo") {
				t.Fatalf("missing context: %#v", out)
			}
		})
	}
}
func TestShellDeadlineAndNonShellCost(t *testing.T) {
	r := Runner{ClientName: "claude", EventName: "PostToolUse", Hub: reconcileHub{wait: true}, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Status: func(context.Context, string) (map[string]string, error) { return map[string]string{}, nil }}
	r.Stdin = strings.NewReader(`{"session_id":"shell","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"echo ok"}}`)
	start := time.Now()
	out := r.Run()
	if time.Since(start) > 600*time.Millisecond {
		t.Fatal("shell deadline missed")
	}
	assertAllow(t, ClientClaude, out)
	r.Status = func(context.Context, string) (map[string]string, error) {
		t.Error("non-shell scanned Git status")
		return nil, nil
	}
	r.Stdin = strings.NewReader(`{"session_id":"shell","cwd":"/repo","tool_name":"Edit","tool_input":{"file_path":"/repo/a"}}`)
	assertAllow(t, ClientClaude, r.Run())
}
