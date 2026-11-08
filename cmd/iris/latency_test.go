package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
)

// TestHookLatency measures process startup, decoding, repository discovery and real
// hub transactions. Opt in because this builds a binary and launches 100 processes.
func TestHookLatency(t *testing.T) {
	if os.Getenv("IRIS_MEASURE_LATENCY") != "1" {
		t.Skip("set IRIS_MEASURE_LATENCY=1 to measure 100 real pre-edit hooks")
	}
	binary := filepath.Join(t.TempDir(), "iris")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	repo := t.TempDir()
	init := exec.Command("git", "init", repo)
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(s, "latency-token", []string{server.Listener.Addr().String()})
	server.Start()
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(token, []byte("latency-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "IRIS_") || upper == "COPILOT_AGENT_SESSION_ID" || upper == "GROK_SESSION_ID" || upper == "CURSOR_CONVERSATION_ID" || upper == "ANTIGRAVITY_CONVERSATION_ID" {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "IRIS_HUB_URL="+server.URL, "IRIS_TOKEN_FILE="+token)
	samples := make([]time.Duration, 0, 100)
	for i := range 100 {
		payload, err := json.Marshal(map[string]any{"session_id": "latency", "cwd": repo, "tool_name": "Edit", "tool_use_id": fmt.Sprintf("latency-%d", i), "tool_input": map[string]string{"file_path": filepath.Join(repo, "src", fmt.Sprintf("file%d.go", i))}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "hook", "--client", "claude", "--event", "PreToolUse")
		cmd.Env = env
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(payload)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		elapsed := time.Since(start)
		if err != nil || strings.TrimSpace(string(out)) != "{}" {
			t.Fatalf("hook %d: %v: %s", i, err, out)
		}
		samples = append(samples, elapsed)
	}
	// Fail-open output alone would give misleading numbers: prove every edit reached SQLite.
	status, err := s.Call(context.Background(), "status", store.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if count := status["counts"].(map[string]int)["Claim"]; count != 100 {
		t.Fatalf("measured fallback instead of real grants: %d claims", count)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("hook latency os=%s arch=%s n=100 p50=%s p95=%s max=%s", runtime.GOOS, runtime.GOARCH, samples[49], samples[94], samples[99])
}
