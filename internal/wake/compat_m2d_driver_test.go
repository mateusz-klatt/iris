//go:build compat

package wake

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// TestCompatM2dWakeDriver runs unchanged against this tree and the v0.1.15 tree.
// The caller supplies a test-owned configuration and stands in for the vendor
// process. The two wake workers keep their own clocks, HTTP clients and loops.
func TestCompatM2dWakeDriver(t *testing.T) {
	dir := os.Getenv("IRIS_COMPAT_WAKE_DIR")
	if dir == "" {
		t.Skip("invoked by the live M2d tests")
	}
	client := os.Getenv("IRIS_COMPAT_WAKE_CLIENT")
	session := os.Getenv("IRIS_COMPAT_WAKE_SESSION")
	parent, ok := proc.Lookup(os.Getppid())
	if !ok || parent.StartMS == 0 {
		t.Fatal("could not identify the vendor process fixture")
	}
	pinned := hubclient.PinnedConfig{ConfigDir: func() (string, error) { return dir, nil }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	switch client {
	case "kimi":
		root := kimiwake.Root(dir)
		state := &kimiwake.Store{Root: root}
		if err := state.MapSession(parent.PID, parent.StartMS, session, time.Second); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kimiwake.PreferencePath(root), []byte("on\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		nearest := func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: parent.PID, StartMS: parent.StartMS, Name: "kimi"}, nil
		}
		worker, ok := newKimiOnce(os.Stdout, pinned.Load, nearest, proc.Lookup, os.Getpid)
		if !ok {
			t.Fatal("Kimi one-shot watcher failed configuration")
		}
		outcome := worker.run(ctx)
		fmt.Fprintf(os.Stdout, "compat Kimi wake outcome %d\n", outcome)
		if ctx.Err() == nil && outcome != Lost {
			t.Fatalf("Kimi one-shot watcher exited with outcome %d", outcome)
		}
	case "antigravity":
		compatAntigravityChild(t, ctx, pinned, dir, session, parent)
	default:
		t.Fatalf("unexpected wake client %q", client)
	}
}

func compatAntigravityChild(t *testing.T, ctx context.Context, pinned hubclient.PinnedConfig, dir, session string, parent proc.Snapshot) {
	t.Helper()
	if err := os.WriteFile(hook.AntigravityWakePath(dir), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ancestor := func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: parent.PID, StartMS: parent.StartMS, Name: "agy"}, nil
	}
	env := map[string]string{
		"IRIS_TOKEN_FILE": filepath.Join(dir, "hub.token"), "ANTIGRAVITY_CONVERSATION_ID": session,
	}
	// SessionStart publishes the local process-to-conversation map before registration.
	// Its Execution has already been created by the caller through the live hub.
	(hook.Runner{
		ClientName: "antigravity", EventName: "SessionStart",
		Stdin:  strings.NewReader(fmt.Sprintf(`{"conversationId":%q}`, session)),
		Getenv: func(k string) string { return env[k] }, Ancestor: ancestor,
	}).Run()
	self, ok := proc.Lookup(os.Getpid())
	if !ok || self.StartMS == 0 {
		t.Fatal("could not identify the wake helper")
	}
	id := attemptID()
	owner := hook.AntigravityHelperOwner{
		AgyPID: parent.PID, AgyStartMS: parent.StartMS, HelperPID: self.PID, HelperStartMS: self.StartMS,
		SessionID: session, AttemptID: id,
	}
	if live, err := hook.PublishAntigravityHelper(dir, owner, proc.Lookup); err != nil || live {
		t.Fatalf("publish Antigravity helper ownership: live=%t err=%v", live, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	env["ANTIGRAVITY_LS_ADDRESS"] = listener.Addr().String()
	env["ANTIGRAVITY_CSRF_TOKEN"] = "compat-wake"
	opts := defaultAntigravityOptions()
	opts.pinned = pinned
	opts.getenv = func(k string) string { return env[k] }
	opts.runtimeEnv = func() ([]string, error) { return nil, nil }
	if err := runAntigravityChild(ctx, opts, fmt.Sprintf("%d:%d:%s", parent.PID, parent.StartMS, id)); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("Antigravity wake helper exited before the test stopped it")
	}
}
