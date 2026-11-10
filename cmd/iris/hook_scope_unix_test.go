//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
)

// slowStatusGit stands in for git: a full status records its pid and waits far past any
// budget; everything else, the reduced status included, is the real git. Each call is logged.
const slowStatusGit = `#!/bin/sh
printf '%s\n' "$*" >> "$IRIS_FAKE_GIT_LOG"
case " $* " in
*" status "*"--ignore-submodules=untracked"*) echo $$ >> "$IRIS_FAKE_GIT_PIDS"; exec sleep 30 ;;
esac
exec "$IRIS_REAL_GIT" "$@"
`

// A real `iris hook` whose full git status cannot finish in its budget exits on time with valid
// output and leaves no git running, and the trip it wrote outlives it: the next hook process
// runs a reduced status and sends its snapshot on the scoped route (ADR 018).
func TestAHookThatTripsLeavesTheNextOneReduced(t *testing.T) {
	if os.Getenv("IRIS_TEST_HOOK_CHILD") == "1" {
		// TestMain clears inherited credentials; the parent passes its scratch hub under names
		// TestMain does not touch.
		_ = os.Setenv("IRIS_HUB_URL", os.Getenv("IRIS_CHILD_HUB_URL"))
		_ = os.Setenv("IRIS_TOKEN_FILE", os.Getenv("IRIS_CHILD_TOKEN_FILE"))
		os.Exit(runHook([]string{"--client", "claude", "--event", os.Getenv("IRIS_CHILD_EVENT")}))
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(realGit, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sub, super := filepath.Join(base, "sub"), filepath.Join(base, "super")
	for _, dir := range []string{sub, super} {
		if err = os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		git(dir, "init", "-q")
		if err = os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(dir, "add", "file.txt")
		git(dir, "commit", "-qm", "file")
	}
	git(super, "submodule", "-q", "add", sub, "lib")
	git(super, "commit", "-qm", "lib")

	fake := filepath.Join(base, "bin")
	if err = os.Mkdir(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(fake, "git"), []byte(slowStatusGit), 0o700); err != nil {
		t.Fatal(err)
	}
	gitLog, pids := filepath.Join(base, "git.log"), filepath.Join(base, "git.pids")

	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var mu sync.Mutex
	var routes []string
	scratch := httptest.NewUnstartedServer(nil)
	handler := hub.New(s, "secret", []string{scratch.Listener.Addr().String()})
	scratch.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		routes = append(routes, r.Method+" "+r.URL.Path)
		mu.Unlock()
		handler.ServeHTTP(w, r)
	})
	scratch.Start()
	defer scratch.Close()
	token := filepath.Join(t.TempDir(), "hub.token")
	if err = os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(map[string]any{"session_id": "trip", "cwd": super, "tool_name": "Bash", "tool_use_id": "t", "tool_input": map[string]string{"command": "make"}})
	exe, _ := os.Executable()
	runChild := func(event string) (string, time.Duration) {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestAHookThatTripsLeavesTheNextOneReduced$")
		cmd.Env = append(os.Environ(), "IRIS_TEST_HOOK_CHILD=1", "IRIS_CHILD_EVENT="+event,
			"IRIS_CHILD_HUB_URL="+scratch.URL, "IRIS_CHILD_TOKEN_FILE="+token,
			"PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"),
			"IRIS_REAL_GIT="+realGit, "IRIS_FAKE_GIT_LOG="+gitLog, "IRIS_FAKE_GIT_PIDS="+pids,
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		started := time.Now()
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", event, err, stderr.String())
		}
		return stdout.String(), time.Since(started)
	}
	readGitLog := func() string {
		raw, _ := os.ReadFile(gitLog)
		_ = os.Remove(gitLog)
		return string(raw)
	}

	// The cold trip: the full status passes the loopback budget and is killed. The time includes
	// starting the test binary, so it allows a second beyond the hook's own deadline.
	out, took := runChild("PreToolUse")
	if out != "{}\n" || took > hook.DefaultDeadline+time.Second {
		t.Fatalf("first hook: %q in %v", out, took)
	}
	if calls := readGitLog(); !strings.Contains(calls, "--ignore-submodules=untracked") || strings.Contains(calls, "--ignore-submodules=dirty") {
		t.Fatalf("first hook ran:\n%s", calls)
	}
	raw, _ := os.ReadFile(pids)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pids %q", raw)
	}
	if err = syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the killed git %d is still there: %v", pid, err)
	}
	breakers, _ := filepath.Glob(filepath.Join(filepath.Dir(token), "sessions", "breaker-*"))
	if len(breakers) != 1 {
		t.Fatalf("breakers %v", breakers)
	}
	hints := hook.ScopeHints(token, time.Now())
	if len(hints) != 1 || !hints[0].Due || !strings.Contains(hints[0].Reason, "300ms") {
		t.Fatalf("hints %+v", hints)
	}

	// The next hook process runs reduced: it registers to learn the capability, then sends.
	if out, _ = runChild("PostToolUse"); out != "{}\n" {
		t.Fatalf("second hook: %q", out)
	}
	if calls := readGitLog(); !strings.Contains(calls, "ls-files --stage -z") || !strings.Contains(calls, "--ignore-submodules=dirty") || strings.Contains(calls, "--ignore-submodules=untracked") {
		t.Fatalf("second hook ran:\n%s", calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(routes, "\n"), "POST /v1/shell/scoped") {
		t.Fatalf("routes %v", routes)
	}
}
