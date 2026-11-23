//go:build linux

package binding

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestCodexExecIDModeRootSucceedsWithVerifiedProcessIdentity(t *testing.T) {
	anc := codexExecProcess(t)
	if !proc.SingleSession(anc, "codex") {
		t.Fatal("helper process was not classified as codex exec")
	}
	f := newSuccessionBindingFixture(t, "codex", "SessionStart", "S", anc)
	assertRegisteredProcess(t, f.hub.file, anc)
	f.hub.clock.Advance(16 * time.Minute)
	r := f.resolver("codex", "", anc)
	b, primary, err := r.ResolveCall(t.Context(), "", "S", "S")
	if err != nil || !primary || b.Generation != 2 || b.Rung != 2 || b.SessionID != "S" {
		t.Fatalf("codex exec succession: bound=%+v primary=%t err=%v", b, primary, err)
	}
	requests := f.hub.successions()
	if len(requests) != 1 || requests[0]["client_pid"] != float64(anc.PID) || requests[0]["client_pid_start_ms"] != float64(anc.StartMS) {
		t.Fatalf("succeed request did not carry the verified codex exec process: %+v", requests)
	}
	assertNoGenerationZeroRegistration(t, f.hub.sessions())
}

// codexExecProcess starts this test binary with argv `codex exec` and returns that live process.
// proc.SingleSession reads its real /proc command line; the ancestor walk in the test is the
// shim's injected walk, which returns this process and nothing from the environment.
func codexExecProcess(t *testing.T) proc.Ancestor {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin)
	child.Args = []string{"codex", "exec"}
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
			t.Errorf("codex exec helper: %v", err)
		}
	})
	snap, ok := proc.Lookup(child.Process.Pid)
	if !ok || snap.PID != child.Process.Pid || snap.StartMS <= 0 {
		t.Fatalf("codex exec process was not live: ok=%t snap=%+v", ok, snap)
	}
	return proc.Ancestor{PID: snap.PID, StartMS: snap.StartMS, Name: "codex"}
}
