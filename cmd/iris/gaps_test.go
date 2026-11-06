package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/service"
)

func TestExportedCredentialsStayOutOfTheTemporaryHome(t *testing.T) {
	if os.Getenv("IRIS_HUB_URL") != "" || os.Getenv("IRIS_TOKEN_FILE") != "" {
		t.Fatalf("leaked hub %q token %q", os.Getenv("IRIS_HUB_URL"), os.Getenv("IRIS_TOKEN_FILE"))
	}
}

func TestRunDispatchesTheRemainingCommands(t *testing.T) {
	if err := run([]string{"guard", "--help"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"watch"}); err == nil {
		t.Fatal("watch without a client")
	}
	if err := run([]string{"ui", "extra"}); err == nil {
		t.Fatal("ui usage")
	}
	if err := run([]string{"mcp", "--bogus"}); err == nil {
		t.Fatal("mcp flag")
	}
	if err := runWatch([]string{"--bogus"}); err == nil {
		t.Fatal("watch flag")
	}
}

func TestServeRejectsATokenThatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.Mkdir(token, 0700); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"serve", "--listen", "127.0.0.1:0", "--db", filepath.Join(dir, "hub.db"), "--token-file", token})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatal(err)
	}
}

func TestDoctorReportsAMissingConfigDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	_, errText := capture(t, func() {
		if code := runDoctor(nil); code != 1 {
			t.Fatalf("code %d", code)
		}
	})
	if errText == "" {
		t.Fatal("doctor dropped the config error")
	}
}

func TestIntegrateAllAndAWriteFailure(t *testing.T) {
	fakeClaude(t)
	out, errText := capture(t, func() {
		if code := dispatch([]string{"integrate", "--bogus"}); code != 2 {
			t.Fatalf("flag %d", code)
		}
		if code := dispatch([]string{"integrate", "--all"}); code != 0 {
			t.Fatalf("all %d", code)
		}
	})
	if !strings.Contains(errText, "flag provided but not defined") || !strings.Contains(out, "integrated claude") || !strings.Contains(out, "integrated kimi") {
		t.Fatalf("out %q err %q", out, errText)
	}

	file := filepath.Join(t.TempDir(), "not-a-home")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", file)
	t.Setenv("USERPROFILE", file)
	_, errText = capture(t, func() {
		if code := dispatch([]string{"integrate", "codex"}); code != 1 {
			t.Fatalf("write %d", code)
		}
	})
	if !strings.Contains(errText, "codex:") {
		t.Fatal(errText)
	}
}

func TestServicePrintsStatusDetailAndAbsFailure(t *testing.T) {
	origHost, origAbs := hostService, absPath
	t.Cleanup(func() { hostService, absPath = origHost, origAbs })
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "iris"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	hostService = func(string, string, []string) service.Manager {
		return scriptedService{status: service.Status{Installed: true, Active: true, Detail: "unit iris"}}
	}
	out, _ := capture(t, func() {
		if code := runService([]string{"status"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(out, "unit iris") || !strings.Contains(out, "active: true") {
		t.Fatal(out)
	}

	absPath = func(string) (string, error) { return "", errors.New("cwd gone") }
	_, errText := capture(t, func() {
		if code := runService([]string{"status"}); code != 1 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(errText, "cwd gone") {
		t.Fatal(errText)
	}
}

func TestGuardWarnsWhenTheHubFailsAndBlocksOnAConflict(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.WriteFile(filepath.Join(repo, "held.go"), []byte("package held\n"), 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("git", "add", "held.go")
	add.Dir = repo
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Chdir(repo)

	mode := "down"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/guard/check" {
			http.NotFound(w, r)
			return
		}
		if mode == "down" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"held by someone","conflicts":[]}`))
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_HUB_URL", srv.URL)
	t.Setenv("IRIS_TOKEN_FILE", token)

	_, errText := capture(t, func() {
		if err := runGuard([]string{"check"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(errText, "allowing commit") {
		t.Fatal(errText)
	}
	mode = "conflict"
	if err := runGuard([]string{"check"}); err == nil || !strings.Contains(err.Error(), "held by someone") {
		t.Fatal(err)
	}
}

type failPlugins struct{}

func (failPlugins) MarketplaceAdd(string) error    { return errors.New("market down") }
func (failPlugins) Install(string) error           { return nil }
func (failPlugins) Uninstall(string) error         { return nil }
func (failPlugins) MarketplaceRemove(string) error { return nil }

func TestSetupRejectsBadInputAndSurvivesEachFailure(t *testing.T) {
	_, errText := capture(t, func() {
		if code := setup([]string{"--bogus"}, setupEnv{}); code != 2 {
			t.Fatalf("nil writers %d", code)
		}
	})
	if !strings.Contains(errText, "flag provided but not defined") {
		t.Fatal(errText)
	}

	var out bytes.Buffer
	base := func() setupEnv {
		out.Reset()
		return setupEnv{
			out: &out, errw: &out,
			getenv:         func(string) string { return "" },
			tokenFile:      filepath.Join(t.TempDir(), "hub.token"),
			detect:         func(func(string) (string, error)) []string { return nil },
			installService: func(string, string) error { return nil },
			runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
				return doctor.Result{HubUp: true, TokenOK: true}, nil
			},
			layout: integrate.Layout{Home: t.TempDir(), Getenv: func(string) string { return "" }},
		}
	}

	env := base()
	if code := setup([]string{"--yes", "extra"}, env); code != 2 || !strings.Contains(out.String(), "usage: iris setup") {
		t.Fatalf("extra %d %s", code, out.String())
	}
	env = base()
	if code := setup([]string{"--hub-url", "http://example.com", "--yes"}, env); code != 2 || !strings.Contains(out.String(), "cleartext") {
		t.Fatalf("url %d %s", code, out.String())
	}
	env = base()
	if code := setup([]string{"--clients", "nope", "--yes"}, env); code != 2 || !strings.Contains(out.String(), "unknown client") {
		t.Fatalf("client %d %s", code, out.String())
	}

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	out.Reset()
	if code := setup(nil, setupEnv{out: &out, errw: &out, getenv: func(string) string { return "" }}); code != 1 || out.Len() == 0 {
		t.Fatalf("token path %d %s", code, out.String())
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	env = base()
	env.detect = func(func(string) (string, error)) []string { return []string{"not-a-client"} }
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 0 || !strings.Contains(out.String(), "Files: none") {
		t.Fatalf("unknown detect %d %s", code, out.String())
	}

	env = base()
	env.tokenFile = filepath.Join(t.TempDir(), "hub.token")
	env.layout = integrate.Layout{Getenv: func(string) string { return "" }}
	env.detect = func(func(string) (string, error)) []string { return []string{integrate.Claude} }
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("AppData", "")
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 1 || !strings.Contains(out.String(), "claude:") {
		t.Fatalf("files %d %s", code, out.String())
	}
	t.Setenv("HOME", t.TempDir())

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	env = base()
	env.tokenFile = filepath.Join(parent, "hub.token")
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 1 {
		t.Fatalf("write config %d %s", code, out.String())
	}

	env = base()
	env.detect = func(func(string) (string, error)) []string { return []string{integrate.Claude} }
	env.plugins = failPlugins{}
	env.layout = integrate.Layout{Home: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), Getenv: func(string) string { return "" }}
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 1 || !strings.Contains(out.String(), "market down") {
		t.Fatalf("install %d %s", code, out.String())
	}

	env = base()
	env.runDoctor = func(context.Context, doctor.Options) (doctor.Result, error) {
		return doctor.Result{}, errors.New("doctor broke")
	}
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 1 || !strings.Contains(out.String(), "doctor broke") {
		t.Fatalf("doctor %d %s", code, out.String())
	}

	for _, hub := range []struct{ url, want string }{
		{"http://127.0.0.1", "127.0.0.1:80"},
		{"http://[::1]:9090", "[::1]:9090"},
	} {
		env = base()
		if code := setup([]string{"--yes", "--hub-url", hub.url}, env); code != 0 || !strings.Contains(out.String(), hub.want) {
			t.Fatalf("%s code %d\n%s", hub.url, code, out.String())
		}
	}
	if listen, ok := loopbackListen("http://10.1.2.3:9"); ok || listen != "" {
		t.Fatalf("public http %q %v", listen, ok)
	}
	if listen, ok := loopbackListen("http://[::1"); ok || listen != "" {
		t.Fatalf("broken url %q %v", listen, ok)
	}
	if listen, ok := loopbackListen("https://iris.example"); ok || listen != "" {
		t.Fatalf("https %q %v", listen, ok)
	}
}

func TestSetupUsesTheRealServiceBuilderWhenNoneIsInjected(t *testing.T) {
	orig := hostService
	t.Cleanup(func() { hostService = orig })
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "iris"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	installed := false
	hostService = func(home, _ string, argv []string) service.Manager {
		if home == "" || !strings.Contains(strings.Join(argv, " "), "serve") {
			t.Errorf("home %q argv %v", home, argv)
		}
		installed = true
		return scriptedService{}
	}
	var out bytes.Buffer
	env := setupEnv{
		out: &out, errw: &out,
		tokenFile: filepath.Join(t.TempDir(), "hub.token"),
		detect:    func(func(string) (string, error)) []string { return nil },
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			return doctor.Result{HubUp: true, TokenOK: true}, nil
		},
		layout: integrate.Layout{Home: t.TempDir(), Getenv: func(string) string { return "" }},
	}
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 0 || !installed {
		t.Fatalf("code %d installed %v\n%s", code, installed, out.String())
	}

	hostService = func(string, string, []string) service.Manager {
		return scriptedService{err: errors.New("unit failed")}
	}
	out.Reset()
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 1 || !strings.Contains(out.String(), "unit failed") {
		t.Fatalf("unit %d %s", code, out.String())
	}

	t.Setenv("PATH", t.TempDir())
	out.Reset()
	if code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, env); code != 1 || !strings.Contains(out.String(), "not on PATH") {
		t.Fatalf("path %d %s", code, out.String())
	}
}
