//go:build compat

package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The parent is an actual OS process with the client's executable name so Iris
// uses its normal process lookup. It forwards stdio to the selected Iris CLI.
func TestCompatM2dShimParent(t *testing.T) {
	bin := os.Getenv("IRIS_COMPAT_M2D_SHIM")
	if bin == "" {
		t.Skip("invoked as the parent of an Iris shim")
	}
	cmd := exec.Command(bin, "mcp", "--client", os.Getenv("IRIS_COMPAT_M2D_CLIENT"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(cmd.Environ(), "IRIS_HUB_URL="+os.Getenv("IRIS_COMPAT_M2D_URL"),
		"IRIS_TOKEN_FILE="+os.Getenv("IRIS_COMPAT_M2D_TOKEN"))
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func compatNamedParentBinary(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	from, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer from.Close()
	path := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	to, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(to, from)
	closeErr := to.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy %s parent executable: %v, close: %v", name, copyErr, closeErr)
	}
	return path
}

func compatConnectCursor(t *testing.T, parent, bin, cwd, proxyURL, tokenFile string) (*mcp.ClientSession, proc.Snapshot) {
	t.Helper()
	cmd := exec.Command(parent, "-test.run=^TestCompatM2dShimParent$")
	cmd.Dir, cmd.Stderr = cwd, os.Stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "IRIS_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"IRIS_COMPAT_M2D_SHIM="+bin,
		"IRIS_COMPAT_M2D_CLIENT=cursor",
		"IRIS_COMPAT_M2D_URL="+proxyURL,
		"IRIS_COMPAT_M2D_TOKEN="+tokenFile,
	)
	cmd.Env = append(cmd.Env, compatTempEnv(t)...)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "cursor", Version: "compat"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatalf("start Cursor shim %s: %v", bin, err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("stop Cursor shim: %v", err)
		}
	})
	ancestor, ok := proc.Lookup(cmd.Process.Pid)
	if !ok || ancestor.StartMS <= 0 {
		t.Fatalf("lookup the Cursor parent process %d: %+v", cmd.Process.Pid, ancestor)
	}
	return session, ancestor
}

// TestMain in the named shim parent cannot clean up after its MCP stdio exit.
func compatTempEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{"TMPDIR=" + dir, "TMP=" + dir, "TEMP=" + dir}
}

func compatAssertWhoami(t *testing.T, session *mcp.ClientSession, expected hubclient.SessionResponse, rung int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("whoami: %+v, %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var bound binding.Bound
	if err := json.Unmarshal(raw, &bound); err != nil {
		t.Fatal(err)
	}
	if bound.Rung != rung || bound.ExecutionID != expected.ExecutionID || bound.Generation != expected.Generation {
		t.Fatalf("whoami after binding = %s, want Rung %d %s G%d", raw, rung, expected.ExecutionID, expected.Generation)
	}
}

func compatSeedProcess(t *testing.T, live *liveCompatHub, client, host, cwd, session string, ancestor proc.Snapshot) hubclient.SessionResponse {
	t.Helper()
	h := compatClient(compatHookEnv(t, live), nil)
	registered, err := h.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: client, SessionID: session, Host: host, CWD: cwd, Worktree: cwd, Source: "hook",
		ClientPID: ancestor.PID, ClientPIDStartMS: ancestor.StartMS, SingleSession: true,
	})
	if err != nil || registered.ExecutionID == "" || registered.Generation != 1 {
		t.Fatalf("register process with live hub: %+v, %v", registered, err)
	}
	return registered
}

func compatRequireHubBinding(t *testing.T, live *liveCompatHub, host, client string, registered hubclient.SessionResponse, recovering bool) {
	t.Helper()
	h := compatClient(compatHookEnv(t, live), nil)
	var got binding.Bound
	err := h.Do(t.Context(), http.MethodPost, "/v1/bind", map[string]any{"host": host, "client": client, "execution": registered.ExecutionID}, &got)
	if recovering {
		if !hubclient.IsCode(err, "lost") {
			t.Fatalf("%s should still be recovering: %+v, %v", registered.ExecutionID, got, err)
		}
	} else if err != nil || got.ExecutionID != registered.ExecutionID || got.Generation != registered.Generation {
		t.Fatalf("expected the exact binding to be live: %+v, %v", got, err)
	}
}

func TestCompatM2dOFFCursorRefreshAndTickAcrossRestart(t *testing.T) {
	old := compatBuildBinary(t, compatRepoRoot(t), compatHistoricalRefs["v0.1.15"])
	current := compatBuildCurrentBinary(t)
	parent := compatNamedParentBinary(t, "cursor-agent")
	dir, cwd := t.TempDir(), gitRepo(t)
	live := startLiveCompatHubAt(t, current, dir)
	trace := newCompatTrace(t, live)
	clientConfig := compatHookEnv(t, live)
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	restart := func() {
		live.stop()
		live = startLiveCompatHubAt(t, current, dir)
		trace.useHub(t, live)
	}
	var registered hubclient.SessionResponse
	var historical []compatExchange
	for _, variant := range []struct{ name, bin string }{{"v0.1.15", old}, {"M2 OFF", current}} {
		t.Run(variant.name, func(t *testing.T) {
			shim, ancestor := compatConnectCursor(t, parent, variant.bin, cwd, trace.url, clientConfig("IRIS_TOKEN_FILE"))
			seed := compatSeedProcess(t, live, "cursor", host, cwd, "cursor-historical-restart", ancestor)
			if registered.ExecutionID != "" && seed.ExecutionID != registered.ExecutionID {
				t.Fatalf("same session moved from %s to %s", registered.ExecutionID, seed.ExecutionID)
			}
			registered = seed
			compatAssertWhoami(t, shim, registered, 3)
			restart()
			initial := len(trace.snapshot())
			compatAssertWhoami(t, shim, registered, 3)
			bind := compatExchange{http.MethodPost, "/v1/bind",
				fmt.Sprintf(`{"client":"cursor","execution":%q,"host":%q}`, registered.ExecutionID, host), http.StatusOK}
			recoveringBind := bind
			recoveringBind.status = http.StatusConflict
			beat := compatExchange{http.MethodPost, "/v1/executions/" + registered.ExecutionID + "/heartbeat", `{"generation":1,"source":"shim"}`, http.StatusOK}
			want := []compatExchange{recoveringBind, beat, bind, beat}
			if got := trace.snapshot()[initial:]; !slices.Equal(got, want) {
				t.Fatalf("Rung-3 refresh trace = %+v, want %+v", got, want)
			}
			restart()
			compatRequireHubBinding(t, live, host, "cursor", registered, true)
			got := trace.wait(t, func(xs []compatExchange) bool { return len(xs) >= initial+len(want)+1 })[initial:]
			want = append(want, beat)
			if !slices.Equal(got, want) {
				t.Fatalf("Rung-3 tick added traffic beyond its heartbeat: got %+v, want %+v", got, want)
			}
			compatRequireHubBinding(t, live, host, "cursor", registered, false)
			if variant.bin == old {
				historical = got
			} else if !slices.Equal(got, historical) {
				t.Fatalf("restart traffic differs by release: M2=%+v v0.1.15=%+v", got, historical)
			}
		})
	}
	assertCompatNoSucceed(t, trace.snapshot())
}

func TestCompatM2dOFFWakeAcrossRestart(t *testing.T) {
	root := compatRepoRoot(t)
	old := compatBuildWakeDriver(t, root, compatHistoricalRefs["v0.1.15"])
	current := compatBuildWakeDriver(t, root, compatHistoricalRef{name: "current"})
	hubBin := compatBuildCurrentBinary(t)
	ancestor, ok := proc.Lookup(os.Getpid())
	if !ok || ancestor.StartMS <= 0 {
		t.Fatalf("lookup the wake parent fixture: %+v", ancestor)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ client, session string }{
		{"kimi", "session_b45a8fae-2681-451d-9add-4efcadc379b1"},
		{"antigravity", "e28a6164-6409-4810-b8ee-df8202a16a0a"},
	} {
		t.Run(tc.client, func(t *testing.T) {
			t.Parallel()
			dir, cwd := t.TempDir(), gitRepo(t)
			live := startLiveCompatHubAt(t, hubBin, dir)
			registered := compatSeedProcess(t, live, tc.client, host, cwd, tc.session, ancestor)
			restart := func() {
				live.stop()
				live = startLiveCompatHubAt(t, hubBin, dir)
			}
			var historical []compatExchange
			for _, variant := range []struct{ name, bin string }{{"v0.1.15", old}, {"M2 OFF", current}} {
				t.Run(variant.name, func(t *testing.T) {
					release := make(chan struct{})
					trace := newCompatTraceWithBindGate(t, live, release)
					cfg := compatWakeConfig(t, live, trace.url, false)
					m := NewSessions(filepath.Join(cfg, "hub.token")).memory
					m.Remember(tc.client, tc.session, registered.Generation)
					if err := os.WriteFile(m.name(tc.client, tc.session)+".execution", []byte("damaged exact record"), 0o600); err != nil {
						t.Fatal(err)
					}
					driver := compatStartWakeDriver(t, variant.bin, cfg, cwd, tc.client, tc.session)
					trace.wait(t, func(xs []compatExchange) bool {
						return slices.ContainsFunc(xs, func(x compatExchange) bool { return x.uri == "/v1/bind" && x.status == http.StatusOK })
					})
					// Use the real initial binding reply as a barrier. At release the wake
					// process has a pinned exact pair and its next call sees the new boot.
					restart()
					trace.useHub(t, live)
					compatRequireHubBinding(t, live, host, tc.client, registered, true)
					close(release)
					beat := compatExchange{http.MethodPost, "/v1/executions/" + registered.ExecutionID + "/heartbeat", `{"generation":1,"source":"shim"}`, http.StatusOK}
					xs := trace.wait(t, func(xs []compatExchange) bool { return slices.Contains(xs, beat) })
					at := slices.Index(xs, beat)
					compatRequireHubBinding(t, live, host, tc.client, registered, false)
					output := driver.stop(t)
					// ADR 030's Disabled status (10) is the one-shot watcher's cancellation outcome.
					if tc.client == "kimi" && !strings.Contains(output, "compat Kimi wake outcome 10\n") {
						t.Fatalf("Kimi wake should remain alive until asked to stop: %s", output)
					}
					all := trace.snapshot()
					assertCompatNoSucceed(t, all)
					var posts []compatExchange
					for _, x := range all {
						if x.method == http.MethodPost {
							posts = append(posts, x)
						}
					}
					if len(posts) != 2 || posts[0].uri != "/v1/bind" || posts[0].status != http.StatusOK || posts[1] != beat {
						t.Fatalf("wake should look up once and resume by heartbeat without extra binds: %+v", all)
					}
					if variant.bin == old {
						historical = xs[:at+1]
					} else if !slices.Equal(xs[:at+1], historical) {
						t.Fatalf("wake restart traffic differs by release: M2=%+v v0.1.15=%+v", xs[:at+1], historical)
					}
				})
			}
		})
	}
}
