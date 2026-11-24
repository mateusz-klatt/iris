package binding

import (
	"os"
	"path/filepath"
	"testing"

	clientconfig "github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestAntigravityHooklessProcessBootstrapsWithAnExistingHelpersDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(clientconfig.SetPinnedForTest(dir))
	if err := os.Mkdir(filepath.Join(dir, "antigravity-helpers"), 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "hub.token")
	f := successionBindingFixture{hub: newRestartableBindingHub(t), token: token, cwd: t.TempDir(), sessions: hook.NewSessions(token)}
	r := f.resolver("antigravity", "", proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"})
	b, err := r.Resolve(t.Context(), "")
	if err != nil || b.SessionID != "mcp-42-100" || b.Generation != 1 || b.Rung != 3 || !r.own {
		t.Fatalf("hookless Antigravity process did not bootstrap: %+v %v", b, err)
	}
	restarted := f.resolver("antigravity", "", proc.Ancestor{PID: 42, StartMS: 100, Name: "agy"})
	again, err := restarted.Resolve(t.Context(), "")
	if err != nil || again.ExecutionID != b.ExecutionID || again.Generation != 1 {
		t.Fatalf("restarted shim lost the durable hookless fallback: bound=%+v err=%v", again, err)
	}
	if err := restarted.Heartbeat(t.Context(), "shim"); err != nil {
		t.Fatalf("restarted shim could not renew the hookless fallback: %v", err)
	}
}
