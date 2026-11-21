//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mateusz-klatt/iris/internal/hook"
)

// A hook whose client.json never answers still exits within its deadline with the client's allow
// (D3). It runs as a real `iris hook` process, because the work the deadline abandons keeps going
// until the process exits: run in the test's own process, it went on writing into the test's
// directory while that was being removed.
func TestStalledClientConfigCannotOutliveTheHookDeadline(t *testing.T) {
	if os.Getenv("IRIS_TEST_HOOK_CHILD") == "1" {
		token := os.Getenv("IRIS_CHILD_TOKEN_FILE")
		os.Exit(runHookIO(
			[]string{"--client", "claude", "--event", "SessionStart"},
			os.Stdin, os.Stdout, os.Stderr,
			func(key string) string {
				if key == "IRIS_TOKEN_FILE" {
					return token
				}
				return ""
			},
			80*time.Millisecond,
		))
	}
	dir := t.TempDir()
	// Nothing ever opens this FIFO for writing, so reading client.json blocks for good.
	if err := unix.Mkfifo(filepath.Join(dir, "client.json"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestStalledClientConfigCannotOutliveTheHookDeadline$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_HOOK_CHILD=1", "IRIS_CHILD_TOKEN_FILE="+filepath.Join(dir, "hub.token"))
	// Outside git the hook registers at session start, so it needs the hub's address (ADR 034).
	cmd.Stdin = strings.NewReader(`{"session_id":"s","cwd":"/workspace/project","hook_event_name":"SessionStart"}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("stalled client.json escaped the hook deadline")
	}
	if err != nil {
		t.Fatal(err)
	}
	if want := hook.FallbackAllow(hook.ClientClaude).Stdout; !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("stdout %q, want the allow %q", out.Bytes(), want)
	}
}
