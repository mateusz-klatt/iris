package hook

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

// Windows takes no reduced snapshot and keeps no breaker (ADR 018). A full status past its
// budget leaves nothing behind, a hub's word that it takes reduced snapshots is not kept, and
// every reconcile runs full scope with nothing sent on the scoped route.
func TestWindowsKeepsNoBreakerAndSendsNoReducedSnapshot(t *testing.T) {
	root := gitRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("DIRC"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &scopeHub{generation: 2}
	m := fileMemory{dir: t.TempDir(), now: time.Now, restrict: func(string) error { return nil }}
	r := Runner{
		Hub:    h,
		Memory: m,
		Find: func(string) (gitinfo.Repo, error) {
			return gitinfo.Repo{Worktree: root, GitDir: filepath.Join(root, ".git"), WorktreeID: "1:2"}, nil
		},
		Getenv: func(key string) string {
			if key == "IRIS_HUB_URL" {
				return "http://127.0.0.1:7878"
			}
			return ""
		},
	}
	ev := Event{Client: ClientClaude, Kind: KindPreTool, SessionID: "s", CWD: root, ToolClass: ClassShell}
	if _, err := r.register(context.Background(), ev, gitinfo.Repo{Worktree: root}); err != nil {
		t.Fatal(err)
	}
	slow := r
	// The budget runs out inside git status, where this test needs it, however loaded the machine.
	slow.ShellDeadline, slow.shellTimeout = 20*time.Millisecond, handTimeout
	slow.Status = func(ctx context.Context, _ string, scope gitinfo.Scope, a *gitinfo.Attempt) (map[string]string, error) {
		h.note(statusEntry(scope))
		deadline, _ := ctx.Deadline()
		a.Begin(ctx, deadline.Add(-slow.ShellDeadline))
		passDeadline(ctx)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if out := slow.reconcile(context.Background(), ev, &runState{}); !isAllow(out) {
		t.Fatalf("%s", out.Stdout)
	}
	quick := r
	quick.ShellDeadline = time.Hour
	quick.Status = func(_ context.Context, _ string, scope gitinfo.Scope, _ *gitinfo.Attempt) (map[string]string, error) {
		h.note(statusEntry(scope))
		return map[string]string{"a.txt": "??"}, nil
	}
	if out := quick.reconcile(context.Background(), ev, &runState{}); !isAllow(out) {
		t.Fatalf("%s", out.Stdout)
	}
	if calls := h.calls(); !slices.Equal(calls, []string{"register", "status full", "status full", "one-call"}) {
		t.Fatalf("calls %v", calls)
	}
	// Only the session's own entries are kept, beside the sessions directory's lock (ADR 017).
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if name := filepath.Join(m.dir, e.Name()); name != m.name("claude", "s") && name != m.counts("claude", "s") && e.Name() != sessionsLock {
			t.Fatalf("kept %s", e.Name())
		}
	}
}
