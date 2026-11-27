//go:build compat

package hook

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCompatM2dOffParityOnV0115Hub(t *testing.T) {
	root := compatRepoRoot(t)
	ref := compatHistoricalRefs["v0.1.15"]
	old := compatBuildBinary(t, root, ref)
	current := compatBuildCurrentBinary(t)
	claude := compatNamedParentBinary(t, "claude")
	oldWake := compatBuildWakeDriver(t, root, ref)
	currentWake := compatBuildWakeDriver(t, root, compatHistoricalRef{name: "current"})
	hub := startLiveCompatHub(t, old)
	seed := compatClient(compatHookEnv(t, hub), nil)
	cwd := gitRepo(t)
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	ancestor, ok := proc.Lookup(os.Getpid())
	if !ok || ancestor.StartMS <= 0 {
		t.Fatal("could not identify the vendor process fixture")
	}
	for _, tc := range []struct {
		name, client, session string
		wake                  bool
	}{
		{name: "resolver", client: "claude", session: "m2d-resolver"},
		{name: "watch", client: "claude", session: "m2d-watch"},
		{name: "Kimi wake", client: "kimi", session: "d61fc495-94af-4b60-aa43-53cd6e124980", wake: true},
		{name: "Antigravity wake", client: "antigravity", session: "97f91e70-13a3-4cd9-9522-e1211dc5e90a", wake: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registered, err := seed.RegisterSession(t.Context(), hubclient.SessionRequest{
				Client: tc.client, SessionID: tc.session, Host: host, CWD: cwd, Worktree: cwd,
				Source: "hook", ClientPID: ancestor.PID, ClientPIDStartMS: ancestor.StartMS, SingleSession: tc.wake,
			})
			if err != nil || registered.Generation != 1 {
				t.Fatalf("register %s: %+v %v", tc.client, registered, err)
			}
			run := func(bin, wakeBin string) []compatExchange {
				trace := newCompatTrace(t, hub)
				dir := compatWakeConfig(t, hub, trace.url, false)
				memory := fileMemory{dir: filepath.Join(dir, "sessions")}
				if err := os.MkdirAll(memory.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				proof := memory.name(tc.client, tc.session) + ".execution"
				const damaged = "incomplete record\n"
				if err := os.WriteFile(proof, []byte(damaged), 0o600); err != nil {
					t.Fatal(err)
				}
				switch {
				case tc.wake:
					worker := compatStartWakeDriver(t, wakeBin, dir, cwd, tc.client, tc.session)
					trace.wait(t, func(got []compatExchange) bool {
						return compatSawRequest(got, http.MethodGet, "/v1/watch?", http.StatusSwitchingProtocols)
					})
					if out := worker.stop(t); strings.Contains(out, "compat Kimi wake outcome 14") {
						t.Fatalf("OFF wake refused the damaged exact record: %s", out)
					}
				case tc.name == "resolver":
					compatClaudeWhoami(t, claude, bin, cwd, dir, trace.url, tc.session, registered)
				case tc.name == "watch":
					compatWatch(t, bin, cwd, dir, tc.session, trace)
				}
				if raw, err := os.ReadFile(proof); err != nil || string(raw) != damaged {
					t.Fatalf("OFF changed the exact record: %q %v", raw, err)
				}
				got := trace.snapshot()
				assertCompatNoSucceed(t, got)
				for _, req := range got {
					if strings.Contains(req.uri, "process_bound=") || strings.Contains(req.body, `"succession_proof"`) {
						t.Errorf("OFF sent opt-in evidence: %+v", req)
					}
				}
				return got
			}
			want := run(old, oldWake)
			got := run(current, currentWake)
			if !slices.Equal(got, want) {
				t.Fatalf("live request traces differ\nv0.1.15: %+v\nM2 OFF:  %+v", want, got)
			}
		})
	}
	t.Run("process registration", func(t *testing.T) {
		trace := newCompatTrace(t, hub)
		getenv := compatHookEnv(t, hub)
		client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: func(k string) string {
			if k == "IRIS_HUB_URL" {
				return trace.url
			}
			return getenv(k)
		}}}
		req := hubclient.SessionRequest{
			Client: "cursor", SessionID: "m2d-process-flag", Host: host, CWD: cwd, Worktree: cwd,
			Source: "hook", ClientPID: ancestor.PID, ClientPIDStartMS: ancestor.StartMS, SingleSession: true,
		}
		registered, err := seed.RegisterSession(t.Context(), req)
		if err != nil || registered.Generation != 1 {
			t.Fatalf("seed process session: %+v %v", registered, err)
		}
		req.Source, req.Generation = "shim", registered.Generation
		reattached, err := binding.RegisterProcessWake(t.Context(), client, req)
		if err != nil || reattached.ExecutionID != registered.ExecutionID || reattached.Generation != registered.Generation {
			t.Fatalf("matching process registration on v0.1.15: %+v %v", reattached, err)
		}
		// v0.1.15 already recognizes the tuple when supplied. A changed tuple
		// is refused, but the new query flag does not make the tuple mandatory there.
		req.ClientPIDStartMS++
		if _, err := binding.RegisterProcessWake(t.Context(), client, req); !hubclient.IsCode(err, "lost") {
			t.Fatalf("changed process registration on v0.1.15: %v", err)
		}
		req.ClientPID, req.ClientPIDStartMS = 0, 0
		reattached, err = binding.RegisterProcessWake(t.Context(), client, req)
		if err != nil || reattached.ExecutionID != registered.ExecutionID || reattached.Generation != registered.Generation {
			t.Fatalf("tuple omitted with additive query on v0.1.15: %+v %v", reattached, err)
		}
		got := trace.snapshot()
		assertCompatNoSucceed(t, got)
		status := []int{http.StatusOK, http.StatusConflict, http.StatusOK}
		if len(got) != len(status) {
			t.Fatalf("unexpected process registration trace: %+v", got)
		}
		for i, call := range got {
			if call.method != http.MethodPost || call.uri != "/v1/sessions?process_bound=true" || call.status != status[i] {
				t.Errorf("unexpected process registration %d: %+v", i, call)
			}
		}
	})
}

func compatSawRequest(trace []compatExchange, method, uri string, status int) bool {
	return slices.ContainsFunc(trace, func(call compatExchange) bool {
		return call.method == method && strings.HasPrefix(call.uri, uri) && call.status == status
	})
}

func compatClaudeCommand(ctx context.Context, bin, cwd, dir, session string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	for _, env := range cmd.Environ() {
		if !strings.HasPrefix(env, "IRIS_") && !strings.HasPrefix(env, "CLAUDE_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "IRIS_TOKEN_FILE="+filepath.Join(dir, "hub.token"), "CLAUDE_CODE_SESSION_ID="+session)
	return cmd
}

func compatClaudeWhoami(t *testing.T, parent, bin, cwd, dir, url, session string, registered hubclient.SessionResponse) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := compatClaudeCommand(ctx, parent, cwd, dir, session, "-test.run=^TestCompatM2dShimParent$")
	cmd.Env = append(cmd.Env, "IRIS_COMPAT_M2D_SHIM="+bin, "IRIS_COMPAT_M2D_CLIENT=claude", "IRIS_COMPAT_M2D_URL="+url,
		"IRIS_COMPAT_M2D_TOKEN="+filepath.Join(dir, "hub.token"))
	cmd.Env = append(cmd.Env, compatTempEnv(t)...)
	client := mcp.NewClient(&mcp.Implementation{Name: "iris-compat", Version: "1"}, nil)
	peer, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	compatAssertWhoami(t, peer, registered, 2)
}

func compatWatch(t *testing.T, bin, cwd, dir, session string, trace *compatTrace) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := compatClaudeCommand(ctx, bin, cwd, dir, session, "watch", "--client", "claude")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	trace.wait(t, func(got []compatExchange) bool {
		return compatSawRequest(got, http.MethodGet, "/v1/watch?", http.StatusSwitchingProtocols) &&
			slices.ContainsFunc(got, func(call compatExchange) bool {
				return call.method == http.MethodPost && strings.HasSuffix(call.uri, "/heartbeat") && call.status == http.StatusOK
			})
	})
}

func TestCompatM2dV0115DecodesLiveStage1bReplies(t *testing.T) {
	root := compatRepoRoot(t)
	bin := compatBuildPackageDriver(t, root, compatHistoricalRefs["v0.1.15"], "internal/hubclient", "compat_m2d_decoder_test.go")
	hub := startLiveCompatHub(t, compatBuildCurrentBinary(t))
	trace := newCompatTrace(t, hub)
	dir := compatWakeConfig(t, hub, trace.url, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestCompatM2dDecoderDriver$", "-test.v")
	cmd.Dir = gitRepo(t)
	cmd.Env = append(cmd.Environ(), "IRIS_COMPAT_DECODER_TOKEN="+filepath.Join(dir, "hub.token"))
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS: TestCompatM2dDecoderDriver") {
		t.Fatalf("v0.1.15 decoder driver: %v\n%s", err, out)
	}
	got := trace.snapshot()
	assertCompatNoSucceed(t, got)
	if len(got) != 2 || !compatSawRequest(got, http.MethodPost, "/v1/sessions", http.StatusOK) ||
		!compatSawRequest(got, http.MethodPost, "/v1/shell/reconcile", http.StatusOK) {
		t.Fatalf("expected a live classified session and combined shell reply, got %+v", got)
	}
}
