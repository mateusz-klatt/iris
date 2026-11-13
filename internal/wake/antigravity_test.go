package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type fakeLSRequest struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

type fakeLSResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	SleepMS  int    `json:"sleep_ms"`
}

func TestMain(m *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == "watch" {
		if len(os.Args) == 5 && slices.Equal(os.Args[1:5], []string{"watch", "--client", "antigravity", "--wake"}) && os.Getenv("ANTIGRAVITY_CSRF_TOKEN") == "sleep-for-spawn-test" {
			time.Sleep(10 * time.Second)
			os.Exit(0)
		}
		os.Exit(2)
	}
	if len(os.Args) >= 4 && os.Args[1] == "agentapi" && os.Args[2] == "send-message" {
		addr := os.Getenv("ANTIGRAVITY_LS_ADDRESS")
		body, _ := json.Marshal(fakeLSRequest{Args: os.Args[1:], Env: os.Environ()})
		resp, err := http.Post("http://"+addr+"/send-message", "application/json", bytes.NewReader(body))
		if err != nil {
			_, _ = fmt.Fprintf(os.Stdout, `{"response":{},"error":"rpc error: code = Unavailable desc = connect: connection refused"}`)
			os.Exit(1)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var scripted fakeLSResponse
		_ = json.Unmarshal(raw, &scripted)
		if scripted.SleepMS > 0 {
			time.Sleep(time.Duration(scripted.SleepMS) * time.Millisecond)
		}
		if scripted.Stdout != "" {
			_, _ = io.WriteString(os.Stdout, scripted.Stdout)
		}
		if scripted.Stderr != "" {
			_, _ = io.WriteString(os.Stderr, scripted.Stderr)
		}
		os.Exit(scripted.ExitCode)
	}
	os.Exit(m.Run())
}

type agyTestHarness struct {
	t          *testing.T
	configDir  string
	tokenPath  string
	hubURL     string
	lsAddr     string
	store      *store.Store
	hubSrv     *httptest.Server
	lsSrv      *httptest.Server
	agyPID     int
	agyStartMS int64
	selfExe    string

	mu       sync.Mutex
	lsCalls  []fakeLSRequest
	nextResp func(fakeLSRequest) fakeLSResponse
}

func newAgyTestHarness(t *testing.T, convID string) *agyTestHarness {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "iris-cfg")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	hubSrv := httptest.NewUnstartedServer(nil)
	hubSrv.Config.Handler = hub.New(s, "secret-hub-token", []string{hubSrv.Listener.Addr().String()})
	hubSrv.Start()
	t.Cleanup(hubSrv.Close)

	tokenPath := filepath.Join(configDir, "hub.token")
	if err := os.WriteFile(tokenPath, []byte("secret-hub-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientJSON, _ := json.Marshal(map[string]string{"hub_url": hubSrv.URL})
	if err := os.WriteFile(filepath.Join(configDir, "client.json"), append(clientJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook.AntigravityWakePath(configDir), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := &agyTestHarness{
		t:         t,
		configDir: configDir,
		tokenPath: tokenPath,
		hubURL:    hubSrv.URL,
		store:     s,
		hubSrv:    hubSrv,
	}
	h.nextResp = func(req fakeLSRequest) fakeLSResponse {
		recipient := ""
		if len(req.Args) >= 3 {
			recipient = req.Args[2]
		}
		out, _ := json.Marshal(map[string]any{
			"response": map[string]any{
				"sendMessage": map[string]any{
					"recipientId": recipient,
				},
			},
		})
		return fakeLSResponse{ExitCode: 0, Stdout: string(out)}
	}

	lsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lsSrv := &httptest.Server{
		Listener: lsLn,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var req fakeLSRequest
			_ = json.Unmarshal(raw, &req)
			h.mu.Lock()
			h.lsCalls = append(h.lsCalls, req)
			resp := h.nextResp(req)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)
		})},
	}
	lsSrv.Start()
	t.Cleanup(lsSrv.Close)
	u, err := url.Parse(lsSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	h.lsAddr = u.Host
	h.lsSrv = lsSrv

	selfSnap, ok := proc.Lookup(os.Getpid())
	if !ok {
		t.Fatal("proc.Lookup(self) failed")
	}
	h.agyPID = selfSnap.PID
	h.agyStartMS = selfSnap.StartMS

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h.selfExe = exe

	if convID != "" {
		h.emitHook(convID, "SessionStart", map[string]any{"conversationId": convID})
	}
	return h
}

func (h *agyTestHarness) emitHook(convID, event string, payload map[string]any) {
	h.t.Helper()
	if _, ok := payload["workspacePaths"]; !ok {
		payload["workspacePaths"] = []string{"/repo"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	(hook.Runner{
		ClientName:      "antigravity",
		EventName:       event,
		Stdin:           bytes.NewReader(raw),
		PinnedConfigDir: func() (string, error) { return h.configDir, nil },
		Hub: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: func(k string) string {
			switch k {
			case "IRIS_HUB_URL":
				return h.hubURL
			case "IRIS_TOKEN_FILE":
				return h.tokenPath
			default:
				return ""
			}
		}}},
		Getenv: func(k string) string {
			switch k {
			case "IRIS_HUB_URL":
				return h.hubURL
			case "IRIS_TOKEN_FILE":
				return h.tokenPath
			case "ANTIGRAVITY_CONVERSATION_ID":
				return convID
			default:
				return ""
			}
		},
		Host: func() string { return "test-host" },
		Find: func(string) (gitinfo.Repo, error) {
			return gitinfo.Repo{
				CommonDir:  "/repo/.git",
				Worktree:   "/repo",
				WorktreeID: "wt-1",
				RemoteURL:  "https://example.invalid/iris.git",
				RootCommit: "deadbeef",
				Branch:     "main",
			}, nil
		},
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: h.agyPID, StartMS: h.agyStartMS, Name: "agy"}, nil
		},
		IrisPath: func() string { return "/usr/local/bin/iris" },
	}).Run()
}

func (h *agyTestHarness) baseOptions(convID, csrf string) antigravityOptions {
	opts := defaultAntigravityOptions()
	opts.args = []string{"watch", "--client", "antigravity", "--wake"}
	env := map[string]string{
		"ANTIGRAVITY_CONVERSATION_ID": convID,
		"ANTIGRAVITY_LS_ADDRESS":      h.lsAddr,
		"ANTIGRAVITY_CSRF_TOKEN":      csrf,
	}
	opts.getenv = func(k string) string { return env[k] }
	opts.pinned = hubclient.PinnedConfig{ConfigDir: func() (string, error) { return h.configDir, nil }}
	covDir := h.t.TempDir()
	opts.runtimeEnv = func() ([]string, error) { return []string{"HOME=" + h.configDir, "GOCOVERDIR=" + covDir}, nil }
	opts.ancestor = func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: h.agyPID, StartMS: h.agyStartMS, Name: "agy"}, nil
	}
	opts.executable = func(int, int64) (string, error) { return h.selfExe, nil }
	opts.irisExecutable = func() (string, error) { return h.selfExe, nil }
	opts.host = "test-host"
	opts.cwd = "/repo"
	opts.handshakeTimeout = 100 * time.Millisecond
	opts.handshakePoll = 5 * time.Millisecond
	opts.maintainInterval = 25 * time.Millisecond
	opts.targetPoll = 10 * time.Millisecond
	opts.settle = 10 * time.Millisecond
	opts.filePoll = 10 * time.Millisecond
	opts.reconnectBase = 10 * time.Millisecond
	opts.reconnectMax = 50 * time.Millisecond
	return opts
}

func TestAntigravityRunEntrypointAndSelfExecutableValidation(t *testing.T) {
	// Under go test, os.Args[1:] are test flags, so RunAntigravity fails closed immediately.
	if err := RunAntigravity(); err == nil {
		t.Fatal("RunAntigravity accepted go test os.Args")
	}

	exe, err := verifiedSelfExecutable()
	if err != nil || exe == "" {
		t.Fatalf("verifiedSelfExecutable = %q, %v", exe, err)
	}

	origExe := osExecutable
	t.Cleanup(func() { osExecutable = origExe })
	osExecutable = func() (string, error) { return "relative/iris", nil }
	if _, err := verifiedSelfExecutable(); err == nil {
		t.Fatal("accepted relative iris path")
	}
	dir := t.TempDir()
	osExecutable = func() (string, error) { return dir, nil }
	if _, err := verifiedSelfExecutable(); err == nil {
		t.Fatal("accepted directory as iris executable")
	}
}

func TestAntigravitySpawnDetachedProcessAndLoopbackValidation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pid, _, kill, err := spawnDetachedProcess(exe, []string{"watch", "--client", "antigravity", "--wake"}, []string{"ANTIGRAVITY_CSRF_TOKEN=sleep-for-spawn-test"})
	if err != nil || pid <= 0 {
		t.Fatalf("spawnDetachedProcess = %d, %v", pid, err)
	}
	if _, ok := proc.Lookup(pid); !ok {
		kill()
		t.Fatal("spawned detached child not found")
	}
	kill()

	// Also exercise release() on a second short-lived child and kill via proc.Terminate.
	pid2, release2, _, err := spawnDetachedProcess(exe, []string{"watch", "--client", "antigravity", "--wake"}, []string{"ANTIGRAVITY_CSRF_TOKEN=sleep-for-spawn-test"})
	if err != nil {
		t.Fatal(err)
	}
	snap2, ok := proc.Lookup(pid2)
	release2()
	if ok {
		_ = proc.Terminate(pid2, snap2.StartMS)
	}

	// Missing binary error.
	if _, _, _, err := spawnDetachedProcess(filepath.Join(t.TempDir(), "missing"), nil, nil); err == nil {
		t.Fatal("missing executable succeeded")
	}
	// openDevNull error.
	origNull := openDevNull
	openDevNull = func() (*os.File, error) { return nil, errors.New("devnull err") }
	_, _, _, err = spawnDetachedProcess(exe, nil, nil)
	openDevNull = origNull
	if err == nil {
		t.Fatal("openDevNull error ignored")
	}

	// Loopback validation rejects non-host:port, non-localhost hostnames, non-loopback IPs, IPv4-in-IPv6, zones, bad ports, and closed ports,
	// while pinning localhost:<port> directly to a literal loopback IP without DNS.
	dial := (&net.Dialer{}).DialContext
	for _, bad := range []string{
		"127.0.0.1",
		"example.invalid:5000",
		"8.8.8.8:5000",
		"[::ffff:127.0.0.1]:5000",
		"[::1%lo]:5000",
		"127.0.0.1:0",
		"127.0.0.1:70000",
		"127.0.0.1:abc",
		"127.0.0.1:1",
		"localhost:1",
	} {
		if _, err := validateLoopbackLS(context.Background(), bad, dial); err == nil {
			t.Fatalf("accepted invalid LS address %q", bad)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	gotCanon, err := validateLoopbackLS(context.Background(), "LOCALHOST:"+portStr, dial)
	if err != nil || gotCanon != "127.0.0.1:"+portStr {
		t.Fatalf("validateLoopbackLS(LOCALHOST:%s) = %q, %v", portStr, gotCanon, err)
	}
}

func TestAntigravityParentPreflightOwnershipScrubbingAndFailureLatch(t *testing.T) {
	const convID = "e28a6164-6409-4810-b8ee-df8202a16a0a"
	const secret = "SUPER_SECRET_CSRF_TOKEN_DO_NOT_LEAK"
	h := newAgyTestHarness(t, convID)

	// Poison caller environment to prove parent never passes caller env or reads overrides.
	t.Setenv("IRIS_HUB_URL", "http://127.0.0.1:1")
	t.Setenv("IRIS_TOKEN_FILE", "/nonexistent/hub.token")
	t.Setenv("IRIS_EXECUTION", "ex_poison")
	t.Setenv("GOTRACEBACK", "all")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/evil.dylib")
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", "/tmp/evil-agy")

	// 1. Extra argument fails closed before detaching.
	badArgs := h.baseOptions(convID, secret)
	badArgs.args = []string{"watch", "--client", "antigravity", "--wake", "--extra"}
	if err := runAntigravity(context.Background(), badArgs); err == nil {
		t.Fatal("accepted extra flag")
	}

	// 2. Each preflight failure latches failed on (agyPID, agyStartMS) without leaking secret.
	checkPreflightFail := func(name string, mutate func(*antigravityOptions)) {
		t.Helper()
		// Re-publish active session cleanly before each case.
		h.emitHook(convID, "SessionStart", map[string]any{"conversationId": convID})
		opts := h.baseOptions(convID, secret)
		mutate(&opts)
		err := runAntigravity(context.Background(), opts)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: leaked CSRF secret in error: %v", name, err)
		}
	}

	checkPreflightFail("bad config", func(o *antigravityOptions) {
		o.pinned = hubclient.PinnedConfig{ConfigDir: func() (string, error) { return "", errors.New("no dir") }}
	})
	checkPreflightFail("runtimeEnv error", func(o *antigravityOptions) {
		o.runtimeEnv = func() ([]string, error) { return nil, errors.New("no runtime env") }
	})
	if err := os.WriteFile(hook.AntigravityWakePath(h.configDir), []byte("off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkPreflightFail("wake off", func(*antigravityOptions) {})
	if err := os.WriteFile(hook.AntigravityWakePath(h.configDir), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkPreflightFail("non-agy ancestor", func(o *antigravityOptions) {
		o.ancestor = func() (proc.Ancestor, error) { return proc.Ancestor{PID: 1, StartMS: 1, Name: "bash"}, nil }
	})
	checkPreflightFail("uppercase convID", func(o *antigravityOptions) {
		o.getenv = func(k string) string {
			if k == "ANTIGRAVITY_CONVERSATION_ID" {
				return strings.ToUpper(convID)
			}
			return secret
		}
	})
	checkPreflightFail("executable missing", func(o *antigravityOptions) {
		o.executable = func(int, int64) (string, error) { return "", proc.ErrNotFound }
	})
	checkPreflightFail("empty csrf", func(o *antigravityOptions) {
		base := o.getenv
		o.getenv = func(k string) string {
			if k == "ANTIGRAVITY_CSRF_TOKEN" {
				return "bad\ntoken"
			}
			return base(k)
		}
	})
	checkPreflightFail("inactive conversation", func(o *antigravityOptions) {
		base := o.getenv
		o.getenv = func(k string) string {
			if k == "ANTIGRAVITY_CONVERSATION_ID" {
				return "11111111-2222-3333-4444-555555555555"
			}
			return base(k)
		}
	})
	checkPreflightFail("bad LS address", func(o *antigravityOptions) {
		base := o.getenv
		o.getenv = func(k string) string {
			if k == "ANTIGRAVITY_LS_ADDRESS" {
				return "127.0.0.1:1"
			}
			return base(k)
		}
	})
	checkPreflightFail("self executable error", func(o *antigravityOptions) {
		o.irisExecutable = func() (string, error) { return "", errors.New("no iris") }
	})
	checkPreflightFail("spawn error", func(o *antigravityOptions) {
		o.spawnDetached = func(string, []string, []string) (int, func(), func(), error) {
			return 0, nil, nil, errors.New("spawn failed")
		}
	})
	checkPreflightFail("post-spawn lookup miss", func(o *antigravityOptions) {
		killed := false
		o.spawnDetached = func(string, []string, []string) (int, func(), func(), error) {
			return 999999, func() {}, func() { killed = true }, nil
		}
		o.lookup = func(pid int) (proc.Snapshot, bool) {
			if pid == 999999 {
				return proc.Snapshot{}, false
			}
			return proc.Lookup(pid)
		}
		_ = killed
	})
	checkPreflightFail("publish error when session switched mid-spawn", func(o *antigravityOptions) {
		o.spawnDetached = func(string, []string, []string) (int, func(), func(), error) {
			h.emitHook("22222222-3333-4444-5555-666666666666", "SessionStart", map[string]any{"conversationId": "22222222-3333-4444-5555-666666666666"})
			return 8888, func() {}, func() {}, nil
		}
		o.lookup = func(pid int) (proc.Snapshot, bool) {
			if pid == 8888 {
				return proc.Snapshot{PID: 8888, StartMS: 50000}, true
			}
			return proc.Lookup(pid)
		}
		o.terminate = func(int, int64) error { return nil }
	})
	// Restore convID as active session.
	h.emitHook(convID, "SessionStart", map[string]any{"conversationId": convID})
	checkPreflightFail("post-publish child exit", func(o *antigravityOptions) {
		checks := 0
		o.spawnDetached = func(string, []string, []string) (int, func(), func(), error) {
			return 8889, func() {}, func() {}, nil
		}
		o.lookup = func(pid int) (proc.Snapshot, bool) {
			if pid == 8889 {
				checks++
				if checks == 1 {
					return proc.Snapshot{PID: 8889, StartMS: 50001}, true
				}
				return proc.Snapshot{}, false
			}
			return proc.Lookup(pid)
		}
	})

	// 3. Successful parent spawn: verifies scrubbed childEnv, exact argv, no CSRF on disk,
	// alreadyLive preservation, and concurrent-publish race handling!
	var capturedArgs, capturedEnv []string
	released := false
	liveChildren := map[int]int64{7777: 70000}
	lookup := func(pid int) (proc.Snapshot, bool) {
		if start, ok := liveChildren[pid]; ok {
			return proc.Snapshot{PID: pid, StartMS: start}, true
		}
		return proc.Lookup(pid)
	}

	okOpts := h.baseOptions(convID, secret)
	okOpts.lookup = lookup
	okOpts.spawnDetached = func(_ string, args, env []string) (int, func(), func(), error) {
		capturedArgs = append([]string(nil), args...)
		capturedEnv = append([]string(nil), env...)
		return 7777, func() { released = true }, func() { t.Error("unexpected kill on healthy spawn") }, nil
	}
	if err := runAntigravity(context.Background(), okOpts); err != nil {
		t.Fatalf("healthy parent spawn failed: %v", err)
	}
	if !released || !slices.Equal(capturedArgs, []string{"watch", "--client", "antigravity", "--wake"}) {
		t.Fatalf("released=%v args=%v", released, capturedArgs)
	}
	for _, entry := range capturedEnv {
		if strings.HasPrefix(entry, "GOTRACEBACK=") || strings.HasPrefix(entry, "LD_") || strings.HasPrefix(entry, "DYLD_") || strings.HasPrefix(entry, "IRIS_HUB_URL=") {
			t.Fatalf("scrubbed childEnv leaked caller variable: %s", entry)
		}
	}
	// Verify CSRF token never reached any file under configDir.
	_ = filepath.WalkDir(h.configDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(p)
			if strings.Contains(string(raw), secret) {
				t.Errorf("CSRF token written to disk at %s", p)
			}
		}
		return nil
	})

	// Duplicate call while 7777 is live exits 0 without spawning!
	spawnedAgain := false
	dupOpts := okOpts
	dupOpts.spawnDetached = func(string, []string, []string) (int, func(), func(), error) {
		spawnedAgain = true
		return 0, nil, nil, errors.New("should not spawn")
	}
	if err := runAntigravity(context.Background(), dupOpts); err != nil || spawnedAgain {
		t.Fatalf("duplicate live call = %v, spawned=%v", err, spawnedAgain)
	}

	// A launcher running with an unseen, non-current conversation UUID must reject it without latching failed over convID's live helper 7777!
	unseenOpts := dupOpts
	baseEnv := unseenOpts.getenv
	unseenOpts.getenv = func(k string) string {
		if k == "ANTIGRAVITY_CONVERSATION_ID" {
			return "99999999-8888-7777-6666-555555555555"
		}
		return baseEnv(k)
	}
	if err := runAntigravity(context.Background(), unseenOpts); err == nil {
		t.Fatal("expected inactive conversation rejection for unseen UUID")
	}
	if curSess, live := hook.ReadAntigravitySession(h.configDir, h.agyPID, h.agyStartMS, lookup); curSess != convID || !live {
		t.Fatalf("unseen conversation launcher clobbered active live helper: cur=%q live=%v", curSess, live)
	}

	// Concurrent publish race: another launcher published 7777 after preflight read, so this launcher kills its redundant child 7778 and exits 0.
	delete(liveChildren, 7777)
	_ = hook.FailAntigravityHelper(h.configDir, h.agyPID, h.agyStartMS, convID, lookup)
	raceKilled := false
	raceOpts := okOpts
	raceOpts.terminate = func(int, int64) error { return nil }
	raceOpts.spawnDetached = func(string, []string, []string) (int, func(), func(), error) {
		liveChildren[7777] = 70000
		liveChildren[7778] = 70008
		_, _ = hook.PublishAntigravityHelper(h.configDir, hook.AntigravityHelperOwner{
			AgyPID: h.agyPID, AgyStartMS: h.agyStartMS, HelperPID: 7777, HelperStartMS: 70000, SessionID: convID, AttemptID: "0123456789abcdef0123456789abcdef",
		}, lookup)
		return 7778, func() {}, func() { raceKilled = true }, nil
	}
	if err := runAntigravity(context.Background(), raceOpts); err != nil || !raceKilled {
		t.Fatalf("concurrent publish race = %v, raceKilled=%v", err, raceKilled)
	}
}

func TestAntigravityChildHandshakeValidationSpoofRejectionAndEndToEndWakeAndContinuation(t *testing.T) {
	const (
		convID   = "e28a6164-6409-4810-b8ee-df8202a16a0a"
		peerSess = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		secret   = "CHILD_CSRF_TOKEN_NEVER_ON_DISK"
		attID    = "00112233445566778899aabbccddeeff"
	)
	h := newAgyTestHarness(t, convID)

	// Handshake parser & child preflight rejections (including spoofed IRIS_AGY_HELPER).
	for _, bad := range []string{
		"bad",
		"0:100:" + attID,
		"10:-1:" + attID,
		"10:100:nothex",
		"10:100:0011",
		"10:100:" + strings.ToUpper(attID),
	} {
		if _, _, _, ok := parseAntigravityHandshake(bad); ok {
			t.Fatalf("accepted invalid handshake %q", bad)
		}
	}

	validHS := fmt.Sprintf("%d:%d:%s", h.agyPID, h.agyStartMS, attID)
	childReject := func(name string, hs string, mutate func(*antigravityOptions)) {
		t.Helper()
		opts := h.baseOptions(convID, secret)
		base := opts.getenv
		opts.getenv = func(k string) string {
			if k == antigravityHelperEnv {
				return hs
			}
			return base(k)
		}
		opts.handshakeTimeout = 15 * time.Millisecond
		mutate(&opts)
		if err := runAntigravity(context.Background(), opts); err == nil {
			t.Fatalf("%s: expected child rejection", name)
		}
	}
	childReject("malformed handshake", "bad", func(*antigravityOptions) {})
	childReject("bad config", validHS, func(o *antigravityOptions) {
		o.pinned = hubclient.PinnedConfig{ConfigDir: func() (string, error) { return "", errors.New("cfg err") }}
	})
	childReject("runtimeEnv err", validHS, func(o *antigravityOptions) {
		o.runtimeEnv = func() ([]string, error) { return nil, errors.New("env err") }
	})
	childReject("missing csrf", validHS, func(o *antigravityOptions) {
		base := o.getenv
		o.getenv = func(k string) string {
			if k == "ANTIGRAVITY_CSRF_TOKEN" {
				return ""
			}
			return base(k)
		}
	})
	childReject("unreachable LS", validHS, func(o *antigravityOptions) {
		base := o.getenv
		o.getenv = func(k string) string {
			if k == "ANTIGRAVITY_LS_ADDRESS" {
				return "127.0.0.1:1"
			}
			return base(k)
		}
	})
	childReject("self lookup miss", validHS, func(o *antigravityOptions) {
		o.selfPID = func() int { return 999999 }
		o.lookup = func(pid int) (proc.Snapshot, bool) {
			if pid == 999999 {
				return proc.Snapshot{}, false
			}
			return proc.Lookup(pid)
		}
	})
	// Spoofed IRIS_AGY_HELPER without parent PublishAntigravityHelper fails closed within handshakeTimeout!
	childReject("spoofed unpublished handshake", validHS, func(*antigravityOptions) {})

	// Now prepare a real transcript file, emit Stop (fullyIdle: true) so convState is Idle with (transcript_path, file_id, idle_offset),
	// publish genuine helper ownership, start the detached child loop, and send real Iris mail!
	transcriptPath := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	initialStep := "{\"step_index\":0,\"source\":\"USER_EXPLICIT\",\"type\":\"USER_INPUT\",\"content\":\"hi\"}\n"
	if err := os.WriteFile(transcriptPath, []byte(initialStep), 0o600); err != nil {
		t.Fatal(err)
	}
	// Publish helper ownership BEFORE Stop so Stop sees the live helper and records Idle=true without asking to arm.
	owner := hook.AntigravityHelperOwner{
		AgyPID:        h.agyPID,
		AgyStartMS:    h.agyStartMS,
		HelperPID:     h.agyPID,
		HelperStartMS: h.agyStartMS,
		SessionID:     convID,
		AttemptID:     attID,
	}
	if _, err := hook.PublishAntigravityHelper(h.configDir, owner, nil); err != nil {
		t.Fatal(err)
	}
	h.emitHook(convID, "Stop", map[string]any{
		"conversationId":     convID,
		"transcriptPath":     transcriptPath,
		"executionNum":       0,
		"continuationActive": false,
		"fullyIdle":          true,
		"terminationReason":  "NO_TOOL_CALL",
	})

	opts := h.baseOptions(convID, secret)
	baseEnv := opts.getenv
	opts.getenv = func(k string) string {
		if k == antigravityHelperEnv {
			return validHS
		}
		return baseEnv(k)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childDone := make(chan error, 1)
	go func() {
		childDone <- runAntigravity(ctx, opts)
	}()

	// Register a peer agent in the same repository and send a message to the Antigravity agent!
	peerReg, err := h.store.Call(context.Background(), "sessions", store.Request{
		Client:        "claude",
		SessionID:     peerSess,
		Host:          "test-host",
		Cwd:           "/repo2",
		Worktree:      "/repo2",
		CommonDir:     "/repo/.git",
		RemoteURL:     "https://example.invalid/iris.git",
		RootCommit:    "deadbeef",
		Branch:        "main",
		Source:        "hook",
		SingleSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Find Antigravity agent's name via agents.
	peersOut, err := h.store.Call(context.Background(), "agents", store.Request{
		All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var agyAgentName string
	for _, p := range peersOut["agents"].([]map[string]any) {
		if p["client"] == "antigravity" {
			agyAgentName = p["name"].(string)
		}
	}
	if agyAgentName == "" {
		t.Fatal("antigravity agent not found in agents")
	}

	if _, err := h.store.Call(context.Background(), "messages", store.Request{
		ExecutionID: peerReg["execution_id"].(string),
		Generation:  peerReg["generation"].(int64),
		To:          []string{agyAgentName},
		Subject:     "ping",
		Body:        "wake up",
		Key:         "send-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Wait for the helper to execute <agy-exe> agentapi send-message <convID> <wakePrompt>!
	var wakeCall fakeLSRequest
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.lsCalls)
		if n > 0 {
			wakeCall = h.lsCalls[0]
		}
		h.mu.Unlock()
		if len(wakeCall.Args) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(wakeCall.Args) != 4 || wakeCall.Args[0] != "agentapi" || wakeCall.Args[1] != "send-message" || wakeCall.Args[2] != convID {
		t.Fatalf("unexpected send-message args: %v", wakeCall.Args)
	}
	wakePrompt := wakeCall.Args[3]
	if !strings.HasPrefix(wakePrompt, promptText+" ") || strings.Contains(wakePrompt, "wake up") {
		t.Fatalf("unexpected prompt content: %q", wakePrompt)
	}
	// Verify scrubbed subprocess environment on <agy-exe>.
	hasCSRF := false
	for _, kv := range wakeCall.Env {
		if kv == "ANTIGRAVITY_CSRF_TOKEN="+secret {
			hasCSRF = true
		}
		if strings.HasPrefix(kv, "GOTRACEBACK=") || strings.HasPrefix(kv, "LD_") || strings.HasPrefix(kv, antigravityHelperEnv+"=") {
			t.Fatalf("<agy-exe> env leaked %s", kv)
		}
	}
	if !hasCSRF {
		t.Fatal("<agy-exe> did not receive ANTIGRAVITY_CSRF_TOKEN in scrubbed Env")
	}

	// Simulate busy-race transcript growth exceeding one pass before <SYSTEM_MESSAGE>, while session is idle:
	// the helper's pollTargetFile continuation loop must advance across multiple ticks and consume the guard!
	pad1 := `{"step_index":1,"source":"MODEL","type":"GENERIC","content":"` + strings.Repeat("P", 4<<20+500) + "\"}\n"
	sysContent := "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-13T14:50:00Z sender=" + convID + " priority=MESSAGE_PRIORITY_HIGH content=" + wakePrompt + "\n</SYSTEM_MESSAGE>"
	sysRaw, _ := json.Marshal(map[string]any{"step_index": 2, "source": "SYSTEM", "type": "SYSTEM_MESSAGE", "content": sysContent})
	f, err := os.OpenFile(transcriptPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(pad1 + string(sysRaw) + "\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	// Wait for helper continuation passes to consume the outstanding guard!
	consumed := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		guards, err := Inspect(h.tokenPath)
		if err == nil && len(guards) == 1 && !guards[0].Outstanding && guards[0].Outcome == "submitted" && guards[0].ConsumedMS > 0 && guards[0].Slot0 == 2 {
			consumed = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !consumed {
		guards, _ := Inspect(h.tokenPath)
		t.Fatalf("helper continuation scanner did not consume guard: %+v", guards)
	}

	// Now simulate /clear switching current_session_id to a new conversation:
	// the helper must stop immediately without renewing the old Execution's lease and clean up its helper record!
	h.emitHook("99999999-8888-7777-6666-555555555555", "SessionStart", map[string]any{"conversationId": "99999999-8888-7777-6666-555555555555"})
	select {
	case err := <-childDone:
		if err != nil {
			t.Fatalf("child exited with error after /clear: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not exit promptly after /clear switched current_session_id")
	}
	if _, live := hook.ReadAntigravitySession(h.configDir, h.agyPID, h.agyStartMS, nil); live {
		t.Fatal("superseded helper record remained live after exit")
	}
}

func TestAntigravityClassificationFinalAdmissionDeferralsAndStateEdges(t *testing.T) {
	const convID = "e28a6164-6409-4810-b8ee-df8202a16a0a"
	h := newAgyTestHarness(t, convID)

	// 1. Direct classifyAntigravitySend unit table: submitted vs narrow failed vs unknown.
	cmdExit1 := exec.Command(h.selfExe, "agentapi", "send-message", convID, "msg")
	cmdExit1.Env = []string{"ANTIGRAVITY_LS_ADDRESS=127.0.0.1:1"}
	errCode1 := cmdExit1.Run()

	goodOut := []byte(`{"response":{"sendMessage":{"recipientId":"` + convID + `"}}}`)
	if ans, det := classifyAntigravitySend(convID, goodOut, nil, nil); ans != "submitted" || det != "" {
		t.Fatalf("good send = %q, %q", ans, det)
	}
	if ans, _ := classifyAntigravitySend(convID, goodOut, []byte("warn\n"), nil); ans != "unknown" {
		t.Fatalf("non-empty stderr = %q", ans)
	}
	if ans, _ := classifyAntigravitySend(convID, []byte("{not-json"), nil, nil); ans != "unknown" {
		t.Fatalf("invalid JSON = %q", ans)
	}
	if ans, _ := classifyAntigravitySend(convID, []byte(`{"response":{"sendMessage":{"recipientId":"other"}}}`), nil, nil); ans != "unknown" {
		t.Fatalf("wrong recipientId = %q", ans)
	}
	for _, detErr := range []string{
		`{"response":{},"error":"rpc error: code = Unauthenticated desc = invalid CSRF token"}`,
		`{"response":{},"error":"rpc error: code = InvalidArgument desc = bad conv"}`,
		`{"response":{},"error":"rpc error: code = NotFound desc = conversation not found"}`,
		`{"response":{},"error":"rpc error: code = Unavailable desc = dial tcp 127.0.0.1:9: connect: connection refused"}`,
	} {
		if ans, det := classifyAntigravitySend(convID, []byte(detErr), nil, errCode1); ans != "failed" || det != antigravityRejectedWake {
			t.Fatalf("deterministic rejection %s = %q, %q", detErr, ans, det)
		}
	}
	for _, ambErr := range []string{
		`{"response":{},"error":"rpc error: code = DeadlineExceeded desc = context deadline exceeded"}`,
		`{"response":{},"error":"rpc error: code = Internal desc = stream terminated"}`,
		`{"response":{},"error":"rpc error: code = Unavailable desc = transport is closing"}`,
	} {
		if ans, _ := classifyAntigravitySend(convID, []byte(ambErr), nil, errCode1); ans != "unknown" {
			t.Fatalf("ambiguous error %s = %q, want unknown", ambErr, ans)
		}
	}

	// 2. antigravityCommands check, idle, initialScan, and queue branches (active deferral, missing exe deferral, alive=false deferral, start failure, timeout/overflow).
	cmds := &antigravityCommands{
		configDir:    h.configDir,
		owner:        hook.AntigravityHelperOwner{AgyPID: h.agyPID, AgyStartMS: h.agyStartMS, SessionID: convID},
		agentEnv:     []string{"ANTIGRAVITY_LS_ADDRESS=" + h.lsAddr, "ANTIGRAVITY_CSRF_TOKEN=tok", "GOCOVERDIR=" + t.TempDir()},
		executable:   func(int, int64) (string, error) { return "", proc.ErrNotFound },
		now:          func() time.Time { return time.UnixMilli(1000) },
		queueTimeout: 40 * time.Millisecond,
		waitDelay:    10 * time.Millisecond,
	}
	if _, det, differs := cmds.check(context.Background(), ""); det != antigravityExeUnavailable || differs {
		t.Fatalf("missing exe check = %q, %v", det, differs)
	}
	cmds.executable = func(int, int64) (string, error) { return h.selfExe, nil }
	if info, det, differs := cmds.check(context.Background(), ""); det != "" || differs || info.Path != h.selfExe {
		t.Fatalf("healthy check = %+v, %q, %v", info, det, differs)
	}
	// Session is not idle yet -> active.
	if idle, det := cmds.idle(context.Background(), convID); idle || det != antigravityActive {
		t.Fatalf("busy idle = %v, %q", idle, det)
	}
	// initialScan when FileID is empty sets ScannedOffset=0.
	if cur := cmds.initialScan(convID); cur.ScannedOffset != 0 {
		t.Fatalf("initialScan without FileID = %+v", cur)
	}
	// Final admission when conv is not idle -> aborted with antigravityActive.
	ans := cmds.queue(context.Background(), convID, "msg", func(start func() error) (bool, error) {
		err := start()
		return err == nil, err
	})
	if ans.answer != "aborted" || ans.detail != antigravityActive {
		t.Fatalf("busy final admission = %+v", ans)
	}

	// Mark session idle via Stop (with live helper published so Stop doesn't arm).
	owner := hook.AntigravityHelperOwner{
		AgyPID: h.agyPID, AgyStartMS: h.agyStartMS, HelperPID: h.agyPID, HelperStartMS: h.agyStartMS, SessionID: convID, AttemptID: "00112233445566778899aabbccddeeff",
	}
	if _, err := hook.PublishAntigravityHelper(h.configDir, owner, nil); err != nil {
		t.Fatal(err)
	}
	h.emitHook(convID, "Stop", map[string]any{
		"conversationId": convID, "executionNum": 0, "continuationActive": false, "fullyIdle": true, "terminationReason": "NO_TOOL_CALL",
	})

	// Final admission when executable disappeared -> aborted with antigravityExeUnavailable.
	cmds.executable = func(int, int64) (string, error) { return "", proc.ErrNotFound }
	ans = cmds.queue(context.Background(), convID, "msg", func(start func() error) (bool, error) {
		err := start()
		return err == nil, err
	})
	if ans.answer != "aborted" || ans.detail != antigravityExeUnavailable {
		t.Fatalf("missing exe final admission = %+v", ans)
	}

	// Final admission when StartIfPrimary rejects binding change (or alive=false via antigravityTargetResolver).
	var canceled bool
	baseRes := binding.NewAntigravityWake(nil, convID, proc.Ancestor{PID: h.agyPID, StartMS: h.agyStartMS, Name: "agy"})
	tr := &antigravityTargetResolver{base: baseRes, alive: func() bool { return false }, cancel: func() { canceled = true }}
	if _, ok := tr.Primary(); ok || !canceled {
		t.Fatal("Primary with alive=false did not cancel")
	}
	// Test StartIfPrimary alive=false callback inside base.StartIfPrimary by seeding a bound target via hub.
	reg, err := h.store.Call(context.Background(), "sessions", store.Request{
		Client: "antigravity", SessionID: convID, Host: "test-host", Cwd: "/repo", Worktree: "/repo",
		CommonDir: "/repo/.git", RemoteURL: "https://example.invalid/iris.git", RootCommit: "deadbeef",
		Source: "hook", ClientPID: int64(h.agyPID), ClientPIDStartMS: h.agyStartMS, SingleSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = reg
	boundRes := binding.NewAntigravityWake(&hubclient.Client{Config: hubclient.EnvConfig{Getenv: func(k string) string {
		if k == "IRIS_HUB_URL" {
			return h.hubURL
		}
		if k == "IRIS_TOKEN_FILE" {
			return h.tokenPath
		}
		return ""
	}}}, convID, proc.Ancestor{PID: h.agyPID, StartMS: h.agyStartMS, Name: "agy"})
	boundRes.Host = "test-host"
	boundRes.CWD = "/repo"
	target, ok := boundRes.LookupWake(context.Background())
	if !ok {
		t.Fatal("LookupWake failed")
	}
	canceled = false
	tr2 := &antigravityTargetResolver{base: boundRes, alive: func() bool { return false }, cancel: func() { canceled = true }}
	ans = cmds.queue(context.Background(), convID, "msg", func(start func() error) (bool, error) {
		return tr2.StartIfPrimary(target, start)
	})
	if ans.answer != "aborted" || !canceled {
		t.Fatalf("alive=false at StartIfPrimary = %+v, canceled=%v", ans, canceled)
	}

	// cmd.Start OS error (non-existent binary path) -> failed with antigravitySendStartFailed.
	cmds.executable = func(int, int64) (string, error) { return filepath.Join(t.TempDir(), "missing-agy"), nil }
	ans = cmds.queue(context.Background(), convID, "msg", func(start func() error) (bool, error) {
		err := start()
		return err == nil, err
	})
	if ans.answer != "failed" || ans.detail != antigravitySendStartFailed {
		t.Fatalf("cmd.Start OS failure = %+v", ans)
	}

	// Timeout / bound exceeded -> unknown.
	cmds.executable = func(int, int64) (string, error) { return h.selfExe, nil }
	h.mu.Lock()
	h.nextResp = func(fakeLSRequest) fakeLSResponse { return fakeLSResponse{SleepMS: 250} }
	h.mu.Unlock()
	ans = cmds.queue(context.Background(), convID, "msg", func(start func() error) (bool, error) {
		err := start()
		return err == nil, err
	})
	if ans.answer != "unknown" {
		t.Fatalf("timeout classification = %+v", ans)
	}

	// Engine aborted retry delays for antigravityActive (15s) and antigravityExeUnavailable (60s),
	// canonicalTargetSession uppercase rejection, state.go Scan validation & clearing on failed/aborted,
	// and inspect.go Antigravity failure reasons.
	if _, ok := canonicalTargetSession(binding.WakeTarget{Client: "antigravity", SessionID: strings.ToUpper(convID)}); ok {
		t.Fatal("canonicalTargetSession accepted uppercase Antigravity UUID")
	}
	for _, reason := range []string{antigravityExeUnavailable, antigravitySendStartFailed, antigravityRejectedWake} {
		if got := inspectionFailureReason(reason); got != reason {
			t.Fatalf("inspectionFailureReason(%q) = %q", reason, got)
		}
	}

	sessions := hook.NewSessions(h.tokenPath)
	st := newClientStateStore(sessions, "antigravity", "state-edge-conv")
	badScanAttempt := testAttempt(1, "queue", "")
	badScanAttempt.Scan = &hook.AntigravityScanCursor{TranscriptPath: "/t", ScannedOffset: -1}
	if testDocument(badScanAttempt).valid() {
		t.Fatal("negative ScannedOffset accepted")
	}
	binCheckWithScan := attempt{
		ID: "00112233445566778899aabbccddeeff", Kind: "binary_check", Stream: "s", Cursor: 1,
		StartedMS: 10, AnsweredMS: 11, Answer: "failed", Scan: &hook.AntigravityScanCursor{},
	}
	if testDocument(binCheckWithScan).valid() {
		t.Fatal("binary_check with Scan accepted")
	}

	goodScanAttempt := testAttempt(1, "queue", "")
	goodScanAttempt.Scan = &hook.AntigravityScanCursor{TranscriptPath: "/t", ScannedOffset: 42}
	snap := mustSnapshot(t, st)
	mustPublish(t, st, snap, testDocument(goodScanAttempt))
	if err := st.answer(goodScanAttempt.ID, "aborted", 200, antigravityExeUnavailable); err != nil {
		t.Fatal(err)
	}
	afterSnap := mustSnapshot(t, st)
	if afterSnap.Document.Attempts[0].Scan != nil {
		t.Fatal("aborted attempt retained Scan cursor")
	}

	// Exercise engine.step aborted delay for antigravityExeUnavailable (60s) and idle active delay (15s).
	engStore := newClientStateStore(sessions, "antigravity", "22222222-3333-4444-5555-666666666666")
	engTarget := binding.WakeTarget{
		Client:    "antigravity",
		SessionID: "22222222-3333-4444-5555-666666666666",
		Host:      "test-host",
		Bound:     binding.Bound{ExecutionID: "ex_eng", Generation: 1, SessionID: "22222222-3333-4444-5555-666666666666", Rung: 3},
	}
	nowTS := time.UnixMilli(50_000)
	fakeRes := &fakeEngineResolver{target: engTarget}
	fakeSrc := &fakeEngineSource{}
	fakeCmds := &fakeAgyEngineCmds{idleVal: false, idleDetail: antigravityActive}
	var engObserved []string
	eng := engine{
		target:        engTarget,
		resolver:      fakeRes,
		source:        fakeSrc,
		store:         engStore,
		commands:      fakeCmds,
		clientVersion: func() string { return "" },
		now:           func() time.Time { return nowTS },
		observe:       func(text string) { engObserved = append(engObserved, text) },
	}
	resBusy := eng.step(context.Background())
	_ = eng.step(context.Background())
	if !resBusy.Due.Equal(nowTS.Add(15*time.Second)) || !slices.Equal(engObserved, []string{deferredSessionActive}) {
		t.Fatalf("antigravityActive idle delay = %v, observed=%q", resBusy.Due, engObserved)
	}
	fakeCmds.idleVal, fakeCmds.idleDetail = true, ""
	fakeCmds.queueAns = commandAnswer{answer: "aborted", detail: antigravityExeUnavailable}
	resExe := eng.step(context.Background())
	if !resExe.Due.Equal(nowTS.Add(60 * time.Second)) {
		t.Fatalf("antigravityExeUnavailable abort delay = %v, want +60s", resExe.Due)
	}
	fakeCmds.queueAns = commandAnswer{answer: "aborted", detail: antigravityActive}
	resAct := eng.step(context.Background())
	_ = eng.step(context.Background())
	if !resAct.Due.Equal(nowTS.Add(15*time.Second)) || !slices.Equal(engObserved, []string{deferredSessionActive, abortedSessionActive}) {
		t.Fatalf("antigravityActive abort delay = %v, observed=%q", resAct.Due, engObserved)
	}
}

type fakeEngineResolver struct {
	target binding.WakeTarget
}

func (f *fakeEngineResolver) Primary() (binding.WakeTarget, bool) { return f.target, true }
func (f *fakeEngineResolver) StartIfPrimary(_ binding.WakeTarget, start func() error) (bool, error) {
	if err := start(); err != nil {
		return false, err
	}
	return true, nil
}

type fakeEngineSource struct{}

func (*fakeEngineSource) HookSeen(context.Context, binding.WakeTarget) (bool, error) {
	return true, nil
}
func (*fakeEngineSource) Attention(context.Context, binding.WakeTarget, int64, string) (attentionRead, error) {
	return attentionRead{
		Attention: hubclient.Attention{Identity: hubclient.Identity{Known: true, Pending: true, Stream: "st", Cursor: 1}},
		Stream:    "st",
		HighWater: 1,
	}, nil
}
func (*fakeEngineSource) Connect(context.Context, binding.WakeTarget, int64, string) (eventConnection, error) {
	return nil, errors.New("unused")
}

type fakeAgyEngineCmds struct {
	idleVal    bool
	idleDetail string
	queueAns   commandAnswer
}

func (*fakeAgyEngineCmds) check(context.Context, string) (binaryInfo, string, bool) {
	return binaryInfo{Path: "/bin/agy", CheckedMS: 1}, "", false
}
func (f *fakeAgyEngineCmds) idle(context.Context, string) (bool, string) {
	return f.idleVal, f.idleDetail
}
func (f *fakeAgyEngineCmds) initialScan(string) *hook.AntigravityScanCursor {
	return &hook.AntigravityScanCursor{TranscriptPath: "/t", FileID: "f1", ScannedOffset: 10}
}
func (f *fakeAgyEngineCmds) queue(context.Context, string, string, func(func() error) (bool, error)) commandAnswer {
	return f.queueAns
}
