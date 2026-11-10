package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/buildinfo"
	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/service"
)

func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	fn()
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	var outBuf, errBuf bytes.Buffer
	_, _ = io.Copy(&outBuf, outR)
	_, _ = io.Copy(&errBuf, errR)
	return outBuf.String(), errBuf.String()
}

func TestMainReportsTheVersionAndRejectsAnUnknownCommand(t *testing.T) {
	origExit, origArgs := exitProcess, os.Args
	t.Cleanup(func() { exitProcess = origExit; os.Args = origArgs })
	exitProcess = func(code int) {
		if code != 0 {
			t.Errorf("exit %d", code)
		}
		panic("exit")
	}
	os.Args = []string{"iris", "version"}
	out, _ := capture(t, func() {
		defer func() {
			if recover() == nil {
				t.Error("main returned")
			}
		}()
		main()
	})
	if strings.TrimSpace(out) != buildinfo.String() {
		t.Fatal(out)
	}

	_, errText := capture(t, func() {
		if code := dispatch(nil); code != 1 {
			t.Fatalf("code %d", code)
		}
	})
	if !strings.Contains(errText, "usage: iris serve") {
		t.Fatal(errText)
	}
	for _, command := range []string{"setup", "doctor", "service", "ui", "token", "admin", "guard", "hook", "mcp", "watch", "integrate", "version"} {
		if !strings.Contains(errText, "| "+command) {
			t.Fatalf("usage does not name %s: %s", command, errText)
		}
	}
	out, _ = capture(t, func() {
		if code := dispatch([]string{"--version"}); code != 0 {
			t.Fatalf("code %d", code)
		}
	})
	if strings.TrimSpace(out) != buildinfo.String() {
		t.Fatalf("--version printed %q", out)
	}
	if code := dispatch([]string{"nope"}); code != 1 {
		t.Fatal(code)
	}
}

func TestDispatchHookIntegrateDoctorAndServiceUsage(t *testing.T) {
	oldIn := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("{"))
	_ = w.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldIn })
	if code := dispatch([]string{"hook", "--client", "kimi", "--event", "PreToolUse"}); code != 0 {
		t.Fatal(code)
	}

	_, errText := capture(t, func() {
		if code := dispatch([]string{"integrate"}); code != 2 {
			t.Fatalf("integrate %d", code)
		}
		if code := dispatch([]string{"integrate", "--all", "extra"}); code != 2 {
			t.Fatalf("integrate all %d", code)
		}
		if code := dispatch([]string{"integrate", "not-a-client"}); code != 2 {
			t.Fatalf("integrate name %d", code)
		}
		if code := dispatch([]string{"doctor", "extra"}); code != 2 {
			t.Fatalf("doctor %d", code)
		}
		if code := dispatch([]string{"doctor", "--bogus"}); code != 2 {
			t.Fatalf("doctor flag %d", code)
		}
		if code := dispatch([]string{"status", "extra"}); code != 2 {
			t.Fatalf("status %d", code)
		}
		if code := dispatch([]string{"service"}); code != 2 {
			t.Fatalf("service %d", code)
		}
		if code := dispatch([]string{"service", "install", "--bogus"}); code != 2 {
			t.Fatalf("service flag %d", code)
		}
		if code := dispatch([]string{"setup", "--bogus"}); code != 2 {
			t.Fatalf("setup %d", code)
		}
	})
	if !strings.Contains(errText, "usage: iris integrate") || !strings.Contains(errText, "unknown client") || !strings.Contains(errText, "usage: iris doctor") || !strings.Contains(errText, "usage: iris service") {
		t.Fatal(errText)
	}
}

// fakeClaude stands in for the Claude CLI, which integrate runs for its plugin.
func fakeClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if _, err := integrate.PlaceFake(dir, "claude"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestIntegrateOneClientThenRemoveIt(t *testing.T) {
	fakeClaude(t)
	out, errText := capture(t, func() {
		if code := dispatch([]string{"integrate", "claude"}); code != 0 {
			t.Fatalf("install %d", code)
		}
		if code := dispatch([]string{"integrate", "--remove", "claude"}); code != 0 {
			t.Fatalf("remove %d", code)
		}
	})
	if !strings.Contains(out, "integrated claude") || !strings.Contains(out, "removed claude") {
		t.Fatalf("out %q err %q", out, errText)
	}
}

func TestDoctorLiveWithNoClientSpendsNothing(t *testing.T) {
	// No client binary can be found, so nothing billable can start even with --yes.
	// A client on the developer's PATH would otherwise run for real.
	t.Setenv("PATH", t.TempDir())
	out, errText := capture(t, func() {
		if code := dispatch([]string{"doctor", "--live", "--yes", "--hub-url", "http://127.0.0.1:1"}); code != 1 {
			t.Fatalf("code %d", code)
		}
	})
	if !strings.Contains(out, "hub: unreachable") || !strings.Contains(errText, "no client is ready for a live run") {
		t.Fatalf("out %q err %q", out, errText)
	}
}

type scriptedService struct {
	err    error
	status service.Status
}

func (s scriptedService) Install(context.Context) error   { return s.err }
func (s scriptedService) Uninstall(context.Context) error { return s.err }
func (s scriptedService) Start(context.Context) error     { return s.err }
func (s scriptedService) Stop(context.Context) error      { return s.err }
func (s scriptedService) Status(context.Context) (service.Status, error) {
	return s.status, s.err
}

func TestServiceReportsEachLifecycleOutcome(t *testing.T) {
	orig := hostService
	t.Cleanup(func() { hostService = orig })
	binDir := t.TempDir()
	if _, err := integrate.PlaceFake(binDir, "iris"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hostService = func(string, string, []string) service.Manager {
		return scriptedService{}
	}
	out, _ := capture(t, func() {
		for _, verb := range []string{"install", "uninstall", "start", "stop"} {
			if code := runService([]string{verb}); code != 0 {
				t.Fatalf("%s %d", verb, code)
			}
		}
		if code := runService([]string{"status"}); code != 4 {
			t.Fatalf("not installed: %d", code)
		}
	})
	if !strings.Contains(out, "installed") || !strings.Contains(out, "uninstalled") || !strings.Contains(out, "installed: false") {
		t.Fatal(out)
	}

	hostService = func(string, string, []string) service.Manager {
		return scriptedService{err: errors.New("refused"), status: service.Status{Detail: "down"}}
	}
	_, errText := capture(t, func() {
		for _, verb := range []string{"install", "uninstall", "start", "stop", "status"} {
			if code := runService([]string{verb}); code != 1 {
				t.Fatalf("%s %d", verb, code)
			}
		}
	})
	if !strings.Contains(errText, "refused") {
		t.Fatal(errText)
	}

	mgr, err := userService("127.0.0.1:9", filepath.Join(t.TempDir(), "token"))
	if err != nil || mgr == nil {
		t.Fatal(err)
	}
}

func TestServiceStatusExitCodesFollowSystemctl(t *testing.T) {
	for _, tc := range []struct {
		st   service.Status
		code int
	}{
		{service.Status{Installed: true, Active: true}, 0},
		{service.Status{Installed: true}, 3},
		{service.Status{}, 4},
		// A hub nobody installed may answer, but the service is still absent.
		{service.Status{Active: true}, 4},
	} {
		if got := statusExit(tc.st); got != tc.code {
			t.Fatalf("%+v: %d, want %d", tc.st, got, tc.code)
		}
	}
}

func TestServiceRejectsAMissingHome(t *testing.T) {
	binDir := t.TempDir()
	if _, err := integrate.PlaceFake(binDir, "iris"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := userServiceDB("", "", ""); err == nil {
		t.Fatal("missing config dir")
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	if _, err := userServiceDB("127.0.0.1:9", filepath.Join(t.TempDir(), "token"), ""); err == nil {
		t.Fatal("missing home")
	}
}

func TestServeRejectsBadFlagsAndStopsWhenSignalled(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	token := filepath.Join(dir, "hub.token")
	if err := run([]string{"serve", "--listen", "0.0.0.0:9"}); err == nil {
		t.Fatal("cleartext non-loopback")
	}
	if err := run([]string{"serve", "extra"}); err == nil {
		t.Fatal("extra")
	}
	if err := run([]string{"serve", "--nope"}); err == nil {
		t.Fatal("flag")
	}
	if err := run([]string{"serve", "--public-url", "http://iris.example"}); err == nil {
		t.Fatal("public url")
	}
	if err := os.Mkdir(db, 0700); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "--db", db, "--token-file", token}); err == nil {
		t.Fatal("directory database")
	}
	_ = os.RemoveAll(db)

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := run([]string{"serve"}); err == nil {
		t.Fatal("missing config")
	}
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))

	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "--listen", "127.0.0.1:0", "--db", filepath.Join(dir, "tls.db"), "--token-file", filepath.Join(dir, "tls.token"), "--tls-cert", cert, "--tls-key", key, "--allowed-hosts", " iris.example:443, ,other.example:443", "--public-url", "https://iris.example"}); err == nil || !strings.Contains(err.Error(), "failed to find any PEM data in certificate input") {
		t.Fatalf("invalid certificate was not rejected by TLS: %v", err)
	}
}

// On Unix TestServeStopsOnSIGTERM proves a signal ends this context. Windows cannot signal
// itself, so here the test ends the context directly and asserts what follows: a clean exit.
func TestServeExitsCleanlyWhenItsContextEnds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	original := serveContext
	t.Cleanup(func() { serveContext = original })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	serveContext = func() (context.Context, context.CancelFunc) { return ctx, stop }

	errCh := make(chan error, 1)
	_, stderr := capture(t, func() {
		go func() {
			errCh <- run([]string{"serve", "--listen", "127.0.0.1:0", "--db", filepath.Join(dir, "hub.db"), "--token-file", filepath.Join(dir, "hub.token")})
		}()
		stop()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("a hub told to stop did not exit cleanly: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the hub did not stop")
		}
	})
	if !strings.Contains(stderr, "Iris listening on 127.0.0.1:") {
		t.Fatalf("%q", stderr)
	}
}

func TestOpenBrowserIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	if openBrowser([]string{filepath.Join(dir, "absent")}) {
		t.Fatal("started a program that does not exist")
	}
	bad := filepath.Join(dir, "not-a-program.exe")
	if err := os.WriteFile(bad, []byte{0}, 0o755); err != nil {
		t.Fatal(err)
	}
	if openBrowser([]string{bad}) {
		t.Fatal("started a file that cannot run")
	}
	fake, err := integrate.PlaceFake(dir, "browser")
	if err != nil {
		t.Fatal(err)
	}
	if !openBrowser([]string{fake, "https://hub.example/ui"}) {
		t.Fatal("did not start the browser")
	}
}

func TestWatchStopsWhenItCannotReportTheRejectedToken(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bind" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected binding request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := io.WriteString(w, `{"error":{"code":"unauthorized","message":"rejected"}}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	t.Setenv("IRIS_HUB_URL", srv.URL)
	t.Setenv("IRIS_TOKEN_FILE", token)
	t.Setenv("IRIS_EXECUTION", "ex_test")
	closed, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = closed
	t.Cleanup(func() { os.Stdout = original })
	if err := runWatch([]string{"--client", "claude"}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("a monitor unable to print kept running or hid the output error: %v", err)
	}
}

func TestGuardHelpCheckAndInstall(t *testing.T) {
	if err := runGuard(nil); err != nil {
		t.Fatal(err)
	}
	if err := runGuard([]string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if err := runGuard([]string{"-h"}); err != nil {
		t.Fatal(err)
	}
	if err := runGuard([]string{"install", "extra"}); err == nil {
		t.Fatal("extra")
	}
	if err := runGuard([]string{"frobnicate"}); err == nil {
		t.Fatal("unknown")
	}

	repo := t.TempDir()
	init := exec.Command("git", "init")
	init.Dir = repo
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Chdir(repo)
	if err := runGuard([]string{"install"}); err != nil {
		t.Fatal(err)
	}
	if err := runGuard([]string{"check"}); err != nil {
		t.Fatal(err)
	}
	if err := runGuard([]string{"remove"}); err != nil {
		t.Fatal(err)
	}
}

func TestGuardTreatsADeletedWorkingDirectoryAsNoRepository(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux reports a deleted working directory: Windows refuses to delete it and macOS still names it")
	}
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := runGuard([]string{"install"}); err == nil || !strings.Contains(err.Error(), "not a git worktree") {
		t.Fatalf("install: %v", err)
	}
	// A check that cannot look warns and allows, the same as outside a repository.
	_, stderr := capture(t, func() {
		if err := runGuard([]string{"check"}); err != nil {
			t.Fatalf("check: %v", err)
		}
	})
	if !strings.Contains(stderr, "not a git worktree") {
		t.Fatalf("%q", stderr)
	}
}

func TestUIPrintsTheSignInPageWithoutCallingTheHubOrReadingAToken(t *testing.T) {
	if err := runUI([]string{"extra"}); err == nil {
		t.Fatal("usage")
	}
	// A directory cannot be read as a bearer credential. Opening sign-in does
	// not need one, and must not mint a session through the machine API.
	token := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("iris ui contacted the hub", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("IRIS_HUB_URL", srv.URL)
	t.Setenv("IRIS_TOKEN_FILE", token)
	t.Setenv("PATH", t.TempDir())
	out, _ := capture(t, func() {
		if err := runUI(nil); err != nil {
			t.Fatal(err)
		}
	})
	if out != srv.URL+"/\n" {
		t.Fatal(out)
	}

	bin := t.TempDir()
	if _, err := integrate.PlaceFake(bin, browserCommand("unused")[0]); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := runUI(nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestWatchAndMCPRejectBadArgumentsAndAMissingParent(t *testing.T) {
	if err := runWatch(nil); err == nil {
		t.Fatal("watch usage")
	}
	if err := runWatch([]string{"--client", "claude", "extra"}); err == nil {
		t.Fatal("watch extra")
	}
	if err := runMCP([]string{"--bogus"}); err == nil {
		t.Fatal("mcp flag")
	}
	if err := runMCP(nil); err == nil {
		t.Fatal("mcp usage")
	}
	orig := watchParent
	t.Cleanup(func() { watchParent = orig })
	watchParent = func() (*proc.ParentWatch, error) { return nil, errors.New("no parent") }
	if err := runWatch([]string{"--client", "claude"}); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatal(err)
	}
	if err := runMCP([]string{"--client", "claude"}); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatal(err)
	}
}

func TestServeAnnouncesNothingWhenThePortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	dir := t.TempDir()
	var serveErr error
	_, stderr := capture(t, func() {
		serveErr = run([]string{"serve", "--listen", taken.Addr().String(), "--db", filepath.Join(dir, "hub.db"), "--token-file", filepath.Join(dir, "hub.token")})
	})
	if serveErr == nil {
		t.Fatal("serve bound a port in use")
	}
	if strings.Contains(stderr, "listening") {
		t.Fatalf("a hub that never bound said %q", stderr)
	}
}

func TestGuardRecordsTheStableIris(t *testing.T) {
	self := func() (string, error) { return "/opt/iris/libexec/iris", nil }
	broken := func() (string, error) { return "", errors.New("no executable") }
	abs, _ := filepath.Abs(filepath.Join("bin", "iris"))
	cases := []struct {
		onPath string
		self   func() (string, error)
		goos   string
		want   string
	}{
		{abs, self, "linux", abs},
		{"iris", self, "darwin", "/opt/iris/libexec/iris"},
		{"iris", broken, "linux", "iris"},
		{"iris.exe", func() (string, error) { return `C:\Program Files\iris\iris.exe`, nil }, "windows", "C:/Program Files/iris/iris.exe"},
	}
	for _, tc := range cases {
		if got := guardIris(tc.onPath, tc.self, tc.goos); got != tc.want {
			t.Errorf("guardIris(%q, %s) = %q, want %q", tc.onPath, tc.goos, got, tc.want)
		}
	}
}
