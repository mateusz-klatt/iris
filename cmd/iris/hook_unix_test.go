//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// A hook whose client.json never answers still exits within its deadline with the client's allow
// (D3) and records the deadline failure for doctor. It runs as a real `iris hook` process, because
// the work the deadline abandons keeps going until the process exits: run in the test's own
// process, it went on writing into the test's directory while that was being removed.
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
	token := filepath.Join(dir, "hub.token")
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
	cmd.Env = append(os.Environ(), "IRIS_TEST_HOOK_CHILD=1", "IRIS_CHILD_TOKEN_FILE="+token)
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
	entries, err := hook.ReadRecentFailures(token, 10)
	if err != nil || len(entries) != 1 || entries[0].Client != "claude" || entries[0].Event != "SessionStart" || entries[0].Stage != "session" || entries[0].Error != "hook deadline exceeded" {
		t.Fatalf("failures = %+v, err = %v", entries, err)
	}
}

// Without IRIS_TOKEN_FILE, the failure recorder still cannot block on a stalled default
// client.json: it falls back to the default token directory and logs the deadline failure.
func TestStalledDefaultClientConfigLogsDeadlineFailure(t *testing.T) {
	if os.Getenv("IRIS_TEST_HOOK_CHILD") == "1" {
		os.Exit(runHookIO(
			[]string{"--client", "claude", "--event", "SessionStart"},
			os.Stdin, os.Stdout, os.Stderr,
			func(key string) string {
				if key == "IRIS_TOKEN_FILE" || key == "IRIS_HUB_URL" {
					return ""
				}
				return os.Getenv(key)
			},
			80*time.Millisecond,
		))
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	cfgBase, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	irisDir := filepath.Join(cfgBase, "iris")
	if err := os.MkdirAll(irisDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(irisDir, "client.json"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestStalledDefaultClientConfigLogsDeadlineFailure$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_HOOK_CHILD=1")
	cmd.Stdin = strings.NewReader(`{"session_id":"s","cwd":"/workspace/project","hook_event_name":"SessionStart"}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("stalled default client.json escaped the hook deadline")
	}
	if err != nil {
		t.Fatal(err)
	}
	if want := hook.FallbackAllow(hook.ClientClaude).Stdout; !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("stdout %q, want the allow %q", out.Bytes(), want)
	}
	entries, err := hook.ReadRecentFailures(filepath.Join(irisDir, "hub.token"), 10)
	if err != nil || len(entries) != 1 || entries[0].Client != "claude" || entries[0].Event != "SessionStart" || entries[0].Stage != "session" || entries[0].Error != "hook deadline exceeded" {
		t.Fatalf("failures = %+v, err = %v", entries, err)
	}
}

// Once iris mcp starts, replacing client.json with an unreadable/stalling FIFO must neither
// divert observations nor leak stalled-read goroutines across repeated observations. It runs
// in a child process so any regressed FIFO reader dies with the child before t.TempDir cleanup.
func TestMCPObserverDoesNotReadStalledClientConfig(t *testing.T) {
	if os.Getenv("IRIS_TEST_HOOK_CHILD") == "1" {
		defaultToken, _, err := (hubclient.EnvConfig{}).BaseTokenPath()
		if err != nil {
			t.Fatal(err)
		}
		relocatedToken := os.Getenv("IRIS_CHILD_RELOCATED_TOKEN")
		cfgPath := filepath.Join(filepath.Dir(defaultToken), "client.json")

		origWorker := newMCPWakeWorker
		t.Cleanup(func() { newMCPWakeWorker = origWorker })
		observeCh := make(chan func(string), 1)
		newMCPWakeWorker = func(_ *binding.Resolver, _ *hook.Sessions, _ func() string, observe func(string)) mcpWakeWorker {
			observeCh <- observe
			return mcpWakeFunction(func(ctx context.Context) { <-ctx.Done() })
		}

		session, done := connectMCPCommand(t, []string{"--client", "codex"}, "0.167.0")
		observe := <-observeCh

		if err := os.Remove(cfgPath); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(cfgPath, 0o600); err != nil {
			t.Fatal(err)
		}

		before := runtime.NumGoroutine()
		const count = 20
		for range count {
			observe("wake_stream_following")
		}
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if entries, err := hook.ReadRecentObservations(relocatedToken, count+5); err == nil && len(entries) == count {
				break
			}
			if time.Now().After(deadline) {
				entries, err := hook.ReadRecentObservations(relocatedToken, count+5)
				t.Fatalf("relocated observations = %d (%v), want %d", len(entries), err, count)
			}
		}
		if entries, err := hook.ReadRecentObservations(defaultToken, 10); err != nil || len(entries) != 0 {
			t.Fatalf("default log unexpectedly written on stalled client.json: %+v, err=%v", entries, err)
		}
		if after := runtime.NumGoroutine(); after >= before+count/2 {
			t.Fatalf("goroutines grew from %d to %d across %d observations", before, after, count)
		}
		_ = session.Close()
		waitMCPCommand(t, done)
		return
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("IRIS_TOKEN_FILE", "")
	t.Setenv("IRIS_EXECUTION", "")

	defaultToken, _, err := (hubclient.EnvConfig{}).BaseTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	relocatedDir := filepath.Join(home, "relocated")
	if err := os.MkdirAll(relocatedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	relocatedToken := filepath.Join(relocatedDir, "hub.token")
	cfgPath := filepath.Join(filepath.Dir(defaultToken), "client.json")
	if err := hubclient.WriteConfig(cfgPath, hubclient.FileConfig{
		Hub:       "http://127.0.0.1:1",
		TokenFile: relocatedToken,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMCPObserverDoesNotReadStalledClientConfig$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_HOOK_CHILD=1", "IRIS_CHILD_RELOCATED_TOKEN="+relocatedToken)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("MCP observer stalled on FIFO client.json:\n%s", out)
	}
	if err != nil {
		t.Fatalf("child failed (%v):\n%s", err, out)
	}
}

// Standalone `iris integrate` must not hang when client.json is a stalled FIFO; it falls back
// to the default token directory after its brief lookup budget.
func TestIntegrateDoesNotHangOnStalledClientConfig(t *testing.T) {
	if os.Getenv("IRIS_TEST_HOOK_CHILD") == "1" {
		os.Exit(runIntegrate([]string{"codex"}))
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("IRIS_TOKEN_FILE", "")
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("IRIS_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("PATH", t.TempDir())

	cfgBase, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	irisDir := filepath.Join(cfgBase, "iris")
	if err := os.MkdirAll(irisDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(irisDir, "client.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestIntegrateDoesNotHangOnStalledClientConfig$")
	cmd.Env = append(os.Environ(), "IRIS_TEST_HOOK_CHILD=1")
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("iris integrate hung on stalled client.json FIFO; stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	if err != nil || !strings.Contains(out.String(), "integrated codex") || errBuf.Len() != 0 {
		t.Fatalf("integrate on stalled client.json = %v, stdout=%q, stderr=%q", err, out.String(), errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(home, "codex", "hooks.json")); err != nil {
		t.Fatalf("codex hooks.json missing: %v", err)
	}
	if info, err := os.Stat(irisDir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("default fallback iris directory not present with 0700: info=%v err=%v", info, err)
	}
	// Only the client, state, and default fallback iris directories exist under home.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "codex,iris,state" {
		t.Fatalf("unexpected directories under home after stalled integrate: %v", names)
	}
}
