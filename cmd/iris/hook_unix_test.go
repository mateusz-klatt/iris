//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
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
	readNames := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	beforeHome := readNames(home)
	beforeCfgBase := readNames(cfgBase)

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
	// The FIFO fixture requires irisDir to exist beforehand (absent-directory creation with
	// 0700 is covered by TestIntegrateCreatesResolvedTokenDirectoryWith0700 in cli_test.go).
	// Verify integrate left irisDir intact and created only codex and state under home (no
	// relocated directory under home or os.UserConfigDir()).
	if info, err := os.Stat(irisDir); err != nil || !info.IsDir() {
		t.Fatalf("default fallback iris directory missing: info=%v err=%v", info, err)
	}
	wantHome := append(slices.Clone(beforeHome), "codex", "state")
	slices.Sort(wantHome)
	if gotHome := readNames(home); !slices.Equal(gotHome, wantHome) {
		t.Fatalf("unexpected directories under home after stalled integrate: got %v, want %v", gotHome, wantHome)
	}
	if gotCfg := readNames(cfgBase); !slices.Equal(gotCfg, beforeCfgBase) {
		t.Fatalf("unexpected directories under config base after stalled integrate: got %v, want %v", gotCfg, beforeCfgBase)
	}
}

func TestHookReadsClientJSONSuccessionToggleWithinRun(t *testing.T) {
	root := t.TempDir()
	for _, cmdArgs := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", append([]string{"-C", root}, cmdArgs...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", cmdArgs, err, out)
		}
	}

	t.Run("client.json session_succession true advertises succession_proof", func(t *testing.T) {
		home := t.TempDir()
		irisDir := filepath.Join(home, ".iris")
		if err := os.MkdirAll(irisDir, 0o700); err != nil {
			t.Fatal(err)
		}
		var sawOptIn bool
		var gotExecPath bool
		tokenPath := filepath.Join(irisDir, "hub.token")
		if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Mock hub responds with classified SessionResponse so admission & exact publication succeed.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/sessions" {
				raw, _ := io.ReadAll(r.Body)
				sawOptIn = strings.Contains(string(raw), `"succession_proof":true`)
				rawRoot, _ := json.Marshal(root)
				_, _ = w.Write([]byte(`{"execution_id":"ex_00000000000000000000000000000001","session_id":"cli-s1","generation":1,"worktree":` + string(rawRoot) + `,"capabilities":["shell_scope","file_claims","succession_proof"],"prior_execution_id":null,"creation_source":"hook","creation_proof":"initial","hook_seen":true}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()
		rawRoot, _ := json.Marshal(root)
		rawHub, _ := json.Marshal(srv.URL)
		cfg := `{"hub_url":` + string(rawHub) + `,"session_succession":true}`
		if err := os.WriteFile(filepath.Join(irisDir, "client.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"HOME": home, "IRIS_TOKEN_FILE": tokenPath}
		getenv := func(k string) string { return env[k] }

		payload := `{"session_id":"cli-s1","cwd":` + string(rawRoot) + `,"hook_event_name":"SessionStart"}`
		var outBuf, errBuf bytes.Buffer
		code := runHookIO([]string{"--client", "claude", "--event", "SessionStart"}, strings.NewReader(payload), &outBuf, &errBuf, getenv, time.Second)
		if code != 0 || !sawOptIn {
			t.Fatalf("code=%d sawOptIn=%t stdout=%q stderr=%q", code, sawOptIn, outBuf.String(), errBuf.String())
		}
		entries, err := os.ReadDir(filepath.Join(irisDir, "sessions"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".execution") {
				gotExecPath = true
			}
		}
		if !gotExecPath {
			t.Fatalf("expected .execution file in sessions dir, got %v", entries)
		}
	})

	t.Run("corrupt client.json defaults to OFF and allows under D3", func(t *testing.T) {
		home := t.TempDir()
		irisDir := filepath.Join(home, ".iris")
		if err := os.MkdirAll(irisDir, 0o700); err != nil {
			t.Fatal(err)
		}
		tokenPath := filepath.Join(irisDir, "hub.token")
		if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(irisDir, "client.json"), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		rawRoot, _ := json.Marshal(root)
		env := map[string]string{"HOME": home, "IRIS_TOKEN_FILE": tokenPath}
		payload := `{"session_id":"cli-s2","cwd":` + string(rawRoot) + `,"hook_event_name":"SessionStart"}`
		var outBuf, errBuf bytes.Buffer
		code := runHookIO([]string{"--client", "claude", "--event", "SessionStart"}, strings.NewReader(payload), &outBuf, &errBuf, func(k string) string { return env[k] }, time.Second)
		if code != 0 || !json.Valid(outBuf.Bytes()) {
			t.Fatalf("expected D3 exit 0, got %d stdout=%q stderr=%q", code, outBuf.String(), errBuf.String())
		}
	})
}
