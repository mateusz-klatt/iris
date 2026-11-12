package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpWakeFunction func(context.Context)

func (f mcpWakeFunction) Run(ctx context.Context) { f(ctx) }

func connectMCPCommand(t *testing.T, args []string, version string) (*mcp.ClientSession, <-chan error) {
	t.Helper()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	original := mcpStdioTransport
	t.Cleanup(func() { mcpStdioTransport = original })
	mcpStdioTransport = func() mcp.Transport { return serverSide }
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- runMCP(args)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	// Codex's captured handshake uses initialize. The SDK's newer default discovery flow does
	// not send that handshake or its initialized notification.
	session, err := mcp.NewClient(&mcp.Implementation{Name: "codex", Version: version}, nil).Connect(ctx, clientSide, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		waitMCPCommand(t, done)
	})
	return session, done
}

func waitMCPCommand(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MCP stopped with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP did not join its background workers")
	}
}

func mcpWakeTrap(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "an unbound MCP must not contact the hub", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "missing.token")
	t.Setenv("IRIS_HUB_URL", server.URL)
	t.Setenv("IRIS_TOKEN_FILE", token)
	t.Setenv("IRIS_EXECUTION", "")
	return token, &requests
}

func TestMCPWakeRejectsOtherClientsBeforeStartingAnything(t *testing.T) {
	original := newMCPWakeWorker
	t.Cleanup(func() { newMCPWakeWorker = original })
	var workers atomic.Int32
	newMCPWakeWorker = func(*binding.Resolver, *hook.Sessions, func() string, func(string)) mcpWakeWorker {
		workers.Add(1)
		return mcpWakeFunction(func(context.Context) {})
	}
	for _, args := range [][]string{
		{"--client", "claude", "--wake"},
		{"--client", "copilot", "--wake"},
		{"--client", "codex", "--wake", "extra"},
		{"--client", "codex", "--wake=invalid"},
	} {
		if err := runMCP(args); err == nil {
			t.Fatalf("invalid wake arguments were accepted: %v", args)
		}
	}
	if workers.Load() != 0 {
		t.Fatal("invalid wake arguments created a worker")
	}
}

func TestMCPWakeOptOutCreatesNoWorkerAndKeepsInitializationLazy(t *testing.T) {
	for _, args := range [][]string{
		{"--client", "codex", "--wake=false"},
		{"--client", "claude"},
		{"--client", "copilot"},
		{"--client", "cursor"},
		{"--client", "antigravity"},
		{"--client", "grok"},
		{"--client", "kimi"},
		{"--client", "claude", "--wake=false"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			token, requests := mcpWakeTrap(t)
			original := newMCPWakeWorker
			t.Cleanup(func() { newMCPWakeWorker = original })
			var workers atomic.Int32
			newMCPWakeWorker = func(*binding.Resolver, *hook.Sessions, func() string, func(string)) mcpWakeWorker {
				workers.Add(1)
				return mcpWakeFunction(func(context.Context) {})
			}
			session, done := connectMCPCommand(t, args, "0.167.0")
			tools, err := session.ListTools(t.Context(), nil)
			if err != nil || len(tools.Tools) != 13 {
				t.Fatalf("disabled-wake tools: %+v, %v", tools, err)
			}
			_ = session.Close()
			waitMCPCommand(t, done)
			if workers.Load() != 0 || requests.Load() != 0 {
				t.Fatalf("opt-out started %d workers and %d requests", workers.Load(), requests.Load())
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(token), "sessions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("opt-out created session files: %v", err)
			}
		})
	}
}

func TestMCPWakeDefaultsOnForCodexSharesInitializeVersionAndJoinsItsWorker(t *testing.T) {
	token, requests := mcpWakeTrap(t)
	original := newMCPWakeWorker
	t.Cleanup(func() { newMCPWakeWorker = original })
	versionSeen := make(chan string, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	newMCPWakeWorker = func(resolver *binding.Resolver, sessions *hook.Sessions, version func() string, observe func(string)) mcpWakeWorker {
		if version() != "" || resolver.Client != "codex" || resolver.Observe == nil || observe == nil || sessions.Dir() != filepath.Join(filepath.Dir(token), "sessions") {
			t.Error("the worker did not receive the uninitialized version and shared resolver/session paths")
		}
		return mcpWakeFunction(func(ctx context.Context) {
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					if got := version(); got != "" {
						versionSeen <- got
						<-ctx.Done()
						close(canceled)
						<-release
						return
					}
				case <-ctx.Done():
					close(canceled)
					return
				}
			}
		})
	}
	session, done := connectMCPCommand(t, []string{"--client", "codex"}, "0.167.0")
	select {
	case got := <-versionSeen:
		if got != "0.167.0" {
			t.Fatalf("worker version = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the initialize version did not reach the worker")
	}
	_ = session.Close()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP EOF did not cancel its worker")
	}
	select {
	case err := <-done:
		t.Fatalf("MCP returned before its worker stopped: %v", err)
	default:
	}
	// The cleanup closes release before joining the command. It remains blocked above until
	// that release, proving that the command joins rather than abandoning its worker.
	if requests.Load() != 0 {
		t.Fatal("initialize contacted the hub")
	}
}

func TestMCPWakeInvalidTokenPathIsObservedAndLeavesToolsAvailable(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AppData", "XDG_CONFIG_HOME", "IRIS_TOKEN_FILE"} {
		t.Setenv(key, "")
	}
	originalObserver, originalWorker := mcpObserver, newMCPWakeWorker
	t.Cleanup(func() { mcpObserver, newMCPWakeWorker = originalObserver, originalWorker })
	observed := make(chan string, 1)
	mcpObserver = func(_ func(string) string, client string) func(string) {
		if client != "codex" {
			t.Errorf("observation client = %q", client)
		}
		return func(reason string) { observed <- reason }
	}
	var workers atomic.Int32
	newMCPWakeWorker = func(*binding.Resolver, *hook.Sessions, func() string, func(string)) mcpWakeWorker {
		workers.Add(1)
		return mcpWakeFunction(func(context.Context) {})
	}
	session, done := connectMCPCommand(t, []string{"--client", "codex", "--wake"}, "0.167.0")
	select {
	case got := <-observed:
		if got != "wake_invalid_state" {
			t.Fatalf("invalid-state observation = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the unusable token path was not observed")
	}
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 13 || workers.Load() != 0 {
		t.Fatalf("unusable wake state affected tools: %+v, %v; workers %d", tools, err, workers.Load())
	}
	_ = session.Close()
	waitMCPCommand(t, done)
}

func TestMCPWakeRealWorkerWaitsForBindingAfterInitialize(t *testing.T) {
	token, requests := mcpWakeTrap(t)
	session, done := connectMCPCommand(t, []string{"--client", "codex", "--wake"}, "0.167.0")
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 13 {
		t.Fatalf("unbound initialized tools: %+v, %v", tools, err)
	}
	_ = session.Close()
	waitMCPCommand(t, done)
	if requests.Load() != 0 {
		t.Fatal("initialize armed an unbound wake worker")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(token), "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an initialized unbound wake created session files: %v", err)
	}
}

func TestMCPWakeUnboundStdioEOFLeavesNoNetworkOrSessionFiles(t *testing.T) {
	token, requests := mcpWakeTrap(t)
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	original := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = original }()
	if err := runMCP([]string{"--client", "codex"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("an unbound wake contacted the hub before stdin EOF")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(token), "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unbound wake created session files: %v", err)
	}
}
