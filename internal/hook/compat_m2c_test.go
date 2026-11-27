//go:build compat

package hook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type compatHistoricalRef struct {
	name   string
	commit string
}

var (
	compatHostHome       = os.Getenv("HOME")
	compatHostGOPATH     = os.Getenv("GOPATH")
	compatHostGoModCache = os.Getenv("GOMODCACHE")
)

var compatHistoricalRefs = map[string]compatHistoricalRef{
	"v0.1.15": {name: "v0.1.15", commit: "6deecd0c26d9bfdd9bf01dfd376acc23739cd26f"},
	"stage1a": {name: "stage1a", commit: "02ed40ec73102fb178298dbc5057b9729d2da592"},
	"m1":      {name: "m1", commit: "90546a36ce0ea340d5b47776be1574851f6f45cd"},
}

type compatRoundTripFunc func(*http.Request) (*http.Response, error)

func (f compatRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// compatSplitWrapper hides ReconcileSession (sessionShellHub) so shellFull exercises the split route.
type compatSplitWrapper struct {
	c *hubclient.Client
}

func (w compatSplitWrapper) RegisterSession(ctx context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	return w.c.RegisterSession(ctx, r)
}

func (w compatSplitWrapper) Touch(ctx context.Context, r hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return w.c.Touch(ctx, r)
}

func (w compatSplitWrapper) EndExecution(ctx context.Context, id string, gen int) error {
	return w.c.EndExecution(ctx, id, gen)
}

func (w compatSplitWrapper) Reconcile(ctx context.Context, r hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	return w.c.Reconcile(ctx, r)
}

func compatRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func compatBuildBinary(t *testing.T, root string, ref compatHistoricalRef) string {
	t.Helper()
	wt, removeWT := compatHistoricalWorktree(t, root, ref)
	defer removeWT()
	return compatBuildIris(t, wt, ref.name)
}

func compatHistoricalWorktree(t *testing.T, root string, ref compatHistoricalRef) (string, func()) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt-"+ref.name)
	var removeOnce sync.Once
	removeWT := func() {
		removeOnce.Do(func() {
			_, _ = exec.Command("git", "-C", root, "worktree", "remove", "--force", wt).CombinedOutput()
		})
	}
	t.Cleanup(removeWT)

	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", wt, ref.commit).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s (%s): %v\n%s", ref.name, ref.commit, err, out)
	}
	return wt, removeWT
}

func compatBuildCurrentBinary(t *testing.T) string {
	t.Helper()
	return compatBuildIris(t, compatRepoRoot(t), "current")
}

func compatBuildIris(t *testing.T, dir, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "iris-"+name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-modcacherw", "-buildvcs=false", "-trimpath", "-o", bin, "./cmd/iris")
	cmd.Dir = dir
	cmd.Env = compatGoEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build iris %s: %v\n%s", name, err, out)
	}
	return bin
}

func compatGoEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CGO_ENABLED=") && !strings.HasPrefix(kv, "GOMODCACHE=") && !strings.HasPrefix(kv, "GOPATH=") {
			env = append(env, kv)
		}
	}
	if compatHostGOPATH != "" {
		env = append(env, "GOPATH="+compatHostGOPATH)
	} else if compatHostHome != "" {
		env = append(env, "GOPATH="+filepath.Join(compatHostHome, "go"))
	}
	if compatHostGoModCache != "" {
		env = append(env, "GOMODCACHE="+compatHostGoModCache)
	}
	return append(env, "CGO_ENABLED=0")
}

type liveCompatHub struct {
	baseURL   string
	tokenFile string
	stop      func()
}

func startLiveCompatHub(t *testing.T, bin string) *liveCompatHub {
	t.Helper()
	return startLiveCompatHubAt(t, bin, t.TempDir())
}

func startLiveCompatHubAt(t *testing.T, bin, hubDir string) *liveCompatHub {
	t.Helper()
	dbPath := filepath.Join(hubDir, "iris.db")
	tokenPath := filepath.Join(hubDir, "hub.token")
	cmd := exec.Command(bin, "serve", "--listen", "127.0.0.1:0", "--allowed-hosts", "historical.iris.test", "--db", dbPath, "--token-file", tokenPath)
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "IRIS_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}

	listenCh := make(chan string, 1)
	var logs bytes.Buffer
	var logsMu sync.Mutex
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			logsMu.Lock()
			logs.WriteString(line)
			logs.WriteByte('\n')
			logsMu.Unlock()
			if strings.HasPrefix(line, "Iris listening on ") {
				select {
				case listenCh <- strings.TrimSpace(strings.TrimPrefix(line, "Iris listening on ")):
				default:
				}
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() {
		<-drainDone
		waitCh <- cmd.Wait()
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if cmd.Process == nil {
				return
			}
			if runtime.GOOS == "windows" {
				_ = cmd.Process.Kill()
			} else if err := cmd.Process.Signal(os.Interrupt); err != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case err := <-waitCh:
				if runtime.GOOS != "windows" && err != nil {
					t.Errorf("historical iris %s exited with error: %v", bin, err)
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-waitCh
			}
		})
	}
	t.Cleanup(stop)

	var addr string
	select {
	case addr = <-listenCh:
	case err := <-waitCh:
		logsMu.Lock()
		msg := logs.String()
		logsMu.Unlock()
		t.Fatalf("historical iris exited early (%v):\n%s", err, msg)
	case <-time.After(15 * time.Second):
		stop()
		logsMu.Lock()
		msg := logs.String()
		logsMu.Unlock()
		t.Fatalf("timed out waiting for historical iris listen line:\n%s", msg)
	}

	return &liveCompatHub{
		baseURL:   "http://" + addr,
		tokenFile: tokenPath,
		stop:      stop,
	}
}

func compatHookEnv(t *testing.T, h *liveCompatHub) func(string) string {
	t.Helper()
	rawToken, err := os.ReadFile(h.tokenFile)
	if err != nil {
		t.Fatalf("read hub token: %v", err)
	}
	clientToken := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(clientToken, rawToken, 0o600); err != nil {
		t.Fatal(err)
	}
	return func(k string) string {
		switch k {
		case "IRIS_HUB_URL":
			return h.baseURL
		case "IRIS_TOKEN_FILE":
			return clientToken
		default:
			return ""
		}
	}
}

func compatClient(getenv func(string) string, rt http.RoundTripper) *hubclient.Client {
	base := rt
	if base == nil {
		base = http.DefaultTransport
	}
	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: compatRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.Host = "historical.iris.test"
			return base.RoundTrip(r2)
		}),
	}
	return &hubclient.Client{
		Config: hubclient.EnvConfig{Getenv: getenv},
		HTTP:   hc,
	}
}

func TestCompatM2c_OFF_V0115_SessionEndErrorKeepsProof(t *testing.T) {
	root := compatRepoRoot(t)
	bin := compatBuildBinary(t, root, compatHistoricalRefs["v0.1.15"])
	liveHub := startLiveCompatHub(t, bin)

	repoDir := gitRepo(t)
	getenv := compatHookEnv(t, liveHub)
	m := sessionsFor(getenv)
	repo, err := gitinfo.Find(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	// Advance the session once cleanly on the hub without leaving a local .ended marker so G2
	// is active on the subsequent OFF SessionStart before SessionEnd fails.
	seedClient := compatClient(getenv, nil)
	g1, err := seedClient.RegisterSession(t.Context(), hubclient.SessionRequest{
		CommonDir: repo.CommonDir, Client: "claude", SessionID: "s1", Host: "compat-host",
		CWD: repoDir, Worktree: repo.Worktree, WorktreeID: repo.WorktreeID, Branch: repo.Branch, Source: "hook",
	})
	if err != nil || g1.Generation != 1 {
		t.Fatalf("seed RegisterSession = (%+v, %v), want G1", g1, err)
	}
	if err := seedClient.EndExecution(t.Context(), g1.ExecutionID, g1.Generation); err != nil {
		t.Fatalf("seed EndExecution: %v", err)
	}

	outStart := Runner{
		ClientName: "claude", EventName: "SessionStart",
		Stdin:             strings.NewReader(`{"session_id":"s1","cwd":` + quote(repoDir) + `,"hook_event_name":"SessionStart"}`),
		Hub:               seedClient,
		Memory:            m,
		Getenv:            getenv,
		Host:              func() string { return "compat-host" },
		SuccessionEnabled: func(context.Context) bool { return false },
		after:             never,
		logAfter:          never,
	}.Run()
	if outStart.Exit != 0 || !strings.Contains(string(outStart.Stdout), "Iris handle:") {
		t.Fatalf("SessionStart exit=%d out=%s", outStart.Exit, outStart.Stdout)
	}
	if got := m.Recall("claude", "s1"); got != 2 {
		t.Fatalf("SessionStart remembered G%d, want G2", got)
	}

	failEndClient := compatClient(getenv, compatRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/end") {
			return nil, errors.New("simulated /end transport failure")
		}
		return http.DefaultTransport.RoundTrip(r)
	}))
	outEnd := Runner{
		ClientName: "claude", EventName: "SessionEnd",
		Stdin:             strings.NewReader(`{"session_id":"s1","cwd":` + quote(repoDir) + `,"hook_event_name":"SessionEnd"}`),
		Hub:               failEndClient,
		Memory:            m,
		Getenv:            getenv,
		Host:              func() string { return "compat-host" },
		SuccessionEnabled: func(context.Context) bool { return false },
		after:             never,
		logAfter:          never,
	}.Run()
	assertAllow(t, ClientClaude, outEnd)
	if got := m.Recall("claude", "s1"); got != 2 {
		t.Fatalf("OFF SessionEnd failure dropped <hash>: got G%d want G2", got)
	}
	if _, err := os.Stat(endedMarkerPath(m.name("claude", "s1"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OFF SessionEnd failure wrote .ended: %v", err)
	}
	if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
		t.Fatal("OFF SessionEnd wrote .execution")
	}
}

func TestCompatM2c_ON_AdmitsV0115AndStage1aWithoutExactProof(t *testing.T) {
	root := compatRepoRoot(t)
	for _, refName := range []string{"v0.1.15", "stage1a"} {
		t.Run(refName, func(t *testing.T) {
			bin := compatBuildBinary(t, root, compatHistoricalRefs[refName])
			liveHub := startLiveCompatHub(t, bin)

			repoDir := gitRepo(t)
			getenv := compatHookEnv(t, liveHub)
			m := sessionsFor(getenv)
			client := compatClient(getenv, nil)
			on := func(context.Context) bool { return true }

			outStart := Runner{
				ClientName: "claude", EventName: "SessionStart",
				Stdin:             strings.NewReader(`{"session_id":"s1","cwd":` + quote(repoDir) + `,"hook_event_name":"SessionStart"}`),
				Hub:               client,
				Memory:            m,
				Getenv:            getenv,
				SuccessionEnabled: on,
				after:             never,
				logAfter:          never,
			}.Run()
			if outStart.Exit != 0 || !strings.Contains(string(outStart.Stdout), "Iris handle:") {
				t.Fatalf("%s ON SessionStart exit=%d out=%s", refName, outStart.Exit, outStart.Stdout)
			}

			outEdit := Runner{
				ClientName: "claude", EventName: "PreToolUse",
				Stdin:             strings.NewReader(editPayload(ClientClaude, repoDir)),
				Hub:               client,
				Memory:            m,
				Getenv:            getenv,
				SuccessionEnabled: on,
				after:             never,
				logAfter:          never,
			}.Run()
			assertAllow(t, ClientClaude, outEdit)

			outShell := Runner{
				ClientName: "claude", EventName: "PreToolUse",
				Stdin:             strings.NewReader(`{"session_id":"s1","cwd":` + quote(repoDir) + `,"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo ok"}}`),
				Hub:               client,
				Memory:            m,
				Getenv:            getenv,
				SuccessionEnabled: on,
				Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
					return map[string]string{}, nil
				},
				ShellDeadline: 2 * time.Second,
				after:         never,
				logAfter:      never,
			}.Run()
			assertAllow(t, ClientClaude, outShell)

			if got := m.Recall("claude", "s1"); got != 1 {
				t.Fatalf("%s ON hook <hash> = G%d, want G1", refName, got)
			}
			if _, ok := m2cReadExact(t, m, "claude", "s1"); ok {
				t.Fatalf("%s ON hook unexpectedly wrote .execution for ordinary unclassified reply", refName)
			}
			if fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); err != nil || len(fails) != 0 {
				t.Fatalf("%s ON hook recorded unexpected failures: %+v err=%v", refName, fails, err)
			}
		})
	}
}

func TestCompatM2c_OFF_M1_RejectsThreeKeyCombinedAndAllowsSplit(t *testing.T) {
	root := compatRepoRoot(t)
	bin := compatBuildBinary(t, root, compatHistoricalRefs["m1"])
	liveHub := startLiveCompatHub(t, bin)
	off := func(context.Context) bool { return false }

	t.Run("combined three-key reply rejected with zero writes", func(t *testing.T) {
		repoDir := gitRepo(t)
		getenv := compatHookEnv(t, liveHub)
		m := sessionsFor(getenv)
		client := compatClient(getenv, nil)

		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin:             strings.NewReader(`{"session_id":"s-comb","cwd":` + quote(repoDir) + `,"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo ok"}}`),
			Hub:               client,
			Memory:            m,
			Getenv:            getenv,
			SuccessionEnabled: off,
			Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{}, nil
			},
			ShellDeadline: 2 * time.Second,
			after:         never,
			logAfter:      never,
		}.Run()
		assertAllow(t, ClientClaude, out)
		if got := m.Recall("claude", "s-comb"); got != 0 {
			t.Fatalf("OFF M1 combined wrote <hash> G%d", got)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s-comb"); ok {
			t.Fatal("OFF M1 combined wrote .execution")
		}
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) == 0 || fails[0].Stage != "shell session" || fails[0].Degraded() {
			t.Fatalf("expected non-degraded shell session failure, got %+v err=%v", fails, err)
		}
	})

	t.Run("split route succeeds cleanly against live M1 hub", func(t *testing.T) {
		repoDir := gitRepo(t)
		getenv := compatHookEnv(t, liveHub)
		m := sessionsFor(getenv)
		client := compatClient(getenv, nil)

		outStart := Runner{
			ClientName: "claude", EventName: "SessionStart",
			Stdin:             strings.NewReader(`{"session_id":"s-split","cwd":` + quote(repoDir) + `,"hook_event_name":"SessionStart"}`),
			Hub:               client,
			Memory:            m,
			Getenv:            getenv,
			SuccessionEnabled: off,
			after:             never,
			logAfter:          never,
		}.Run()
		if outStart.Exit != 0 || !strings.Contains(string(outStart.Stdout), "Iris handle:") {
			t.Fatalf("OFF M1 SessionStart exit=%d out=%s", outStart.Exit, outStart.Stdout)
		}

		outShell := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin:             strings.NewReader(`{"session_id":"s-split","cwd":` + quote(repoDir) + `,"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo ok"}}`),
			Hub:               compatSplitWrapper{c: client},
			Memory:            m,
			Getenv:            getenv,
			SuccessionEnabled: off,
			Status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{}, nil
			},
			ShellDeadline: 2 * time.Second,
			after:         never,
			logAfter:      never,
		}.Run()
		assertAllow(t, ClientClaude, outShell)

		outEdit := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin:             strings.NewReader(`{"session_id":"s-split","cwd":` + quote(repoDir) + `,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":` + quote(filepath.Join(repoDir, "a.go")) + `,"content":"x"}}`),
			Hub:               client,
			Memory:            m,
			Getenv:            getenv,
			SuccessionEnabled: off,
			after:             never,
			logAfter:          never,
		}.Run()
		assertAllow(t, ClientClaude, outEdit)

		if got := m.Recall("claude", "s-split"); got != 1 {
			t.Fatalf("OFF M1 split <hash> = G%d, want G1", got)
		}
		if _, ok := m2cReadExact(t, m, "claude", "s-split"); ok {
			t.Fatal("OFF M1 split wrote .execution")
		}
		if fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10); err != nil || len(fails) != 0 {
			t.Fatalf("OFF M1 split recorded failures: %+v err=%v", fails, err)
		}
	})
}
