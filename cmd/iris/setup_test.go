package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

type setupPlugins struct{}

func (setupPlugins) MarketplaceAdd(string) error    { return nil }
func (setupPlugins) Install(string) error           { return nil }
func (setupPlugins) Uninstall(string) error         { return nil }
func (setupPlugins) MarketplaceRemove(string) error { return nil }

func TestSetupDeclineWritesNothing(t *testing.T) {
	home := t.TempDir()
	called := false
	var out bytes.Buffer
	code := setup(nil, setupEnv{
		in:   strings.NewReader("n\n"),
		out:  &out,
		errw: &out,
		layout: integrate.Layout{
			Home:     home,
			StateDir: filepath.Join(home, "state"),
			Getenv:   func(string) string { return "" },
		},
		detect: func(func(string) (string, error)) []string { return []string{integrate.Codex} },
		installService: func(string, string) error {
			called = true
			return nil
		},
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			t.Fatal("doctor ran after a decline")
			return doctor.Result{}, nil
		},
		tokenFile: filepath.Join(home, "hub.token"),
	})
	if code != 0 || called {
		t.Fatalf("code %d service %v\n%s", code, called, out.String())
	}
	if !strings.Contains(out.String(), "Hub: http://127.0.0.1:7878") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("decline wrote client files")
	}
	for _, name := range integrate.Names {
		if !strings.Contains(out.String(), "Iris inactive on "+name+"\n") {
			t.Fatalf("missing inactive %s\n%s", name, out.String())
		}
	}
}

func TestSetupIntegratesOnlyTheDetectedClient(t *testing.T) {
	home := t.TempDir()
	iris, err := integrate.PlaceFake(home, "iris")
	if err != nil {
		t.Fatal(err)
	}
	layout := integrate.Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv:   func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if name == "iris" {
				return iris, nil
			}
			return "", errors.New("not found")
		},
	}
	serviceCalls := 0
	var out bytes.Buffer
	code := setup([]string{"--yes", "--hub-url", "https://hub.example"}, setupEnv{
		out:     &out,
		errw:    &out,
		layout:  layout,
		detect:  func(func(string) (string, error)) []string { return []string{integrate.Codex} },
		plugins: setupPlugins{},
		installService: func(string, string) error {
			serviceCalls++
			return nil
		},
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			return doctor.Result{HubUp: true, TokenOK: true}, nil
		},
		tokenFile: filepath.Join(home, "hub.token"),
	})
	if code != 0 {
		t.Fatal(code, out.String())
	}
	if serviceCalls != 0 {
		t.Fatal("remote hub installed a local service")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); err != nil {
		t.Fatal(err)
	}
	if configured, enabled, err := integrate.CodexWake(layout); err != nil || !configured || !enabled {
		t.Fatalf("fresh setup did not enable Codex idle wake: configured/enabled %v/%v, %v", configured, enabled, err)
	}
	text := out.String()
	if !strings.Contains(text, "codex: Review hooks once in /hooks") {
		t.Fatal(text)
	}
	if strings.Contains(text, "Iris inactive on codex") {
		t.Fatal(text)
	}
	for _, name := range integrate.Names {
		if name == integrate.Codex {
			continue
		}
		if !strings.Contains(text, "Iris inactive on "+name+"\n") {
			t.Fatalf("missing inactive %s\n%s", name, text)
		}
	}
}

func TestSetupPreservesExplicitCodexWakeDisable(t *testing.T) {
	home := t.TempDir()
	customHome := filepath.Join(home, "custom-codex")
	layout := integrate.Layout{
		Home: home, StateDir: filepath.Join(home, "state"),
		Getenv: func(key string) string {
			if key == "CODEX_HOME" {
				return customHome
			}
			return ""
		},
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
	}
	if err := integrate.InstallCodexWake(layout, false); err != nil {
		t.Fatal(err)
	}
	config, err := integrate.CodexConfig(layout)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := setup([]string{"--yes", "--hub", "https://hub.example"}, setupEnv{
		out: &out, errw: &out, layout: layout,
		detect: func(func(string) (string, error)) []string { return []string{integrate.Codex} },
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			return doctor.Result{HubUp: true, TokenOK: true}, nil
		},
		tokenFile: filepath.Join(home, "hub.token"),
	})
	if code != 0 {
		t.Fatalf("setup exit %d: %s", code, out.String())
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(after, before) || !bytes.Contains(after, []byte(`"--wake=false"`)) {
		t.Fatalf("setup changed explicit wake preference: %s, %v", after, err)
	}
	if configured, enabled, err := integrate.CodexWake(layout); err != nil || !configured || enabled {
		t.Fatalf("setup did not preserve disable: configured/enabled %v/%v, %v", configured, enabled, err)
	}
}

func TestSetupDefaultsAntigravityWakeOnAndPreservesExplicitChoices(t *testing.T) {
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "iris")
	layout := integrate.Layout{
		Home:            home,
		StateDir:        filepath.Join(home, "state"),
		Getenv:          func(string) string { return "" },
		LookPath:        func(string) (string, error) { return "", os.ErrNotExist },
		PinnedConfigDir: func() (string, error) { return cfgDir, nil },
		GOOS:            "linux",
	}
	runAgySetup := func() {
		t.Helper()
		var out bytes.Buffer
		code := setup([]string{"--yes", "--hub", "https://hub.example"}, setupEnv{
			out: &out, errw: &out, layout: layout,
			detect: func(func(string) (string, error)) []string { return []string{integrate.Antigravity} },
			runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
				return doctor.Result{HubUp: true, TokenOK: true}, nil
			},
			tokenFile: filepath.Join(cfgDir, "hub.token"),
		})
		if code != 0 {
			t.Fatalf("setup exit %d: %s", code, out.String())
		}
	}

	markerPath := filepath.Join(cfgDir, "antigravity-wake")
	runAgySetup()
	if enabled, err := integrate.AntigravityWake(layout); err != nil || !enabled {
		t.Fatalf("fresh setup: enabled=%v err=%v", enabled, err)
	}
	if raw, err := os.ReadFile(markerPath); err != nil || string(raw) != "default\n" {
		t.Fatalf("fresh setup marker = %q, %v", raw, err)
	}

	if err := integrate.InstallAntigravityWake(layout, false); err != nil {
		t.Fatal(err)
	}
	runAgySetup()
	if enabled, err := integrate.AntigravityWake(layout); err != nil || enabled {
		t.Fatalf("setup after --no-wake: enabled=%v err=%v", enabled, err)
	}
	if raw, err := os.ReadFile(markerPath); err != nil || string(raw) != "off\n" {
		t.Fatalf("setup after --no-wake marker = %q, %v", raw, err)
	}

	if err := integrate.InstallAntigravityWake(layout, true); err != nil {
		t.Fatal(err)
	}
	runAgySetup()
	if enabled, err := integrate.AntigravityWake(layout); err != nil || !enabled {
		t.Fatalf("setup after --wake: enabled=%v err=%v", enabled, err)
	}
	if raw, err := os.ReadFile(markerPath); err != nil || string(raw) != "on\n" {
		t.Fatalf("setup after --wake marker = %q, %v", raw, err)
	}
}

func TestSetupLoopbackInstallsService(t *testing.T) {
	var listen, token string
	var out bytes.Buffer
	code := setup([]string{"--yes", "--hub-url", "http://127.0.0.1:7878"}, setupEnv{
		out:    &out,
		errw:   &out,
		detect: func(func(string) (string, error)) []string { return nil },
		installService: func(l, tok string) error {
			listen, token = l, tok
			return nil
		},
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			return doctor.Result{HubUp: true, TokenOK: true}, nil
		},
		tokenFile: filepath.Join(t.TempDir(), "hub.token"),
		layout: integrate.Layout{
			Home:   t.TempDir(),
			Getenv: func(string) string { return "" },
		},
	})
	if code != 0 {
		t.Fatal(code, out.String())
	}
	if listen != "127.0.0.1:7878" || token == "" {
		t.Fatalf("listen %q token %q", listen, token)
	}
	if !strings.Contains(out.String(), "Files: none") {
		t.Fatal(out.String())
	}
}

func TestSetupPersistsRemoteConfiguration(t *testing.T) {
	home := t.TempDir()
	token := filepath.Join(home, "hub.token")
	var out bytes.Buffer
	code := setup([]string{"--yes", "--hub", "https://iris.klatt.ie", "--clients", "claude"}, setupEnv{
		out: &out, errw: &out, getenv: func(string) string { return "" }, tokenFile: token,
		layout:         integrate.Layout{Home: home, StateDir: filepath.Join(home, "state"), Getenv: func(string) string { return "" }},
		detect:         func(func(string) (string, error)) []string { return []string{integrate.Codex, integrate.Claude} },
		plugins:        setupPlugins{},
		installService: func(string, string) error { t.Fatal("remote service installed"); return nil },
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			return doctor.Result{HubUp: true, TokenOK: true}, nil
		},
	})
	if code != 0 {
		t.Fatal(code, out.String())
	}
	raw, err := os.ReadFile(filepath.Join(home, "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "https://iris.klatt.ie") || !strings.Contains(string(raw), "hub.token") {
		t.Fatal(string(raw))
	}
	if _, err := os.Stat(token); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup created a token", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unselected codex integrated", err)
	}
	if !strings.Contains(out.String(), "setup cannot fetch it") {
		t.Fatal(out.String())
	}
}

func TestSetupDoesNotAskForATokenThatIsThere(t *testing.T) {
	home := t.TempDir()
	token := filepath.Join(home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := setup([]string{"--yes", "--hub", "https://iris.klatt.ie", "--clients", "claude"}, setupEnv{
		out: &out, errw: &out, getenv: func(string) string { return "" }, tokenFile: token,
		layout:  integrate.Layout{Home: home, StateDir: filepath.Join(home, "state"), Getenv: func(string) string { return "" }},
		detect:  func(func(string) (string, error)) []string { return []string{integrate.Claude} },
		plugins: setupPlugins{},
		runDoctor: func(context.Context, doctor.Options) (doctor.Result, error) {
			return doctor.Result{HubUp: true, TokenOK: true}, nil
		},
	})
	if code != 0 || strings.Contains(out.String(), "setup cannot fetch it") {
		t.Fatal(code, out.String())
	}
}

func TestTokenMissing(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank")
	full := filepath.Join(dir, "full")
	if err := os.WriteFile(blank, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !tokenMissing(filepath.Join(dir, "absent")) || !tokenMissing(blank) || tokenMissing(full) {
		t.Fatal("wrong verdict")
	}
}

func TestSetupServiceFailureEndsWithNextStep(t *testing.T) {
	for _, installErr := range []error{errors.New("access denied"), nil} {
		var out bytes.Buffer
		tokenFile := filepath.Join(t.TempDir(), "hub.token")
		code := setup([]string{"--yes"}, setupEnv{
			out: &out, errw: &out, getenv: func(string) string { return "" }, tokenFile: tokenFile,
			detect:         func(func(string) (string, error)) []string { return nil },
			installService: func(string, string) error { return installErr },
			runDoctor:      func(context.Context, doctor.Options) (doctor.Result, error) { return doctor.Result{}, nil },
		})
		text := out.String()
		reason := "hub is unreachable after service installation"
		if installErr != nil {
			reason = installErr.Error()
		}
		if code != 1 || !strings.HasSuffix(text, "Service could not start: "+reason+"\nNext step: iris serve --listen 127.0.0.1:7878 --token-file "+tokenFile+"\n") {
			t.Fatalf("code %d\n%s", code, text)
		}
	}
}

// declinedSetup runs setup, answers no, and returns what it printed.
func declinedSetup(t *testing.T, args []string, found []string, vars map[string]string) (string, int) {
	t.Helper()
	home := t.TempDir()
	var out bytes.Buffer
	code := setup(args, setupEnv{
		in:     strings.NewReader("n\n"),
		out:    &out,
		errw:   &out,
		getenv: func(key string) string { return vars[key] },
		layout: integrate.Layout{
			Home:     home,
			StateDir: filepath.Join(home, "state"),
			Getenv:   func(string) string { return "" },
		},
		detect:    func(func(string) (string, error)) []string { return found },
		tokenFile: filepath.Join(home, "hub.token"),
	})
	return out.String(), code
}

func TestSetupClientsAcceptAliasesAndAnyCase(t *testing.T) {
	cases := []struct{ flag, client, file string }{
		{"CODEX", integrate.Codex, filepath.Join(".codex", "hooks.json")},
		{" Cursor-Agent ", integrate.Cursor, filepath.Join(".cursor", "hooks.json")},
		{"grok,kimi-code", integrate.Kimi, filepath.Join(".kimi-code", "config.toml")},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			text, code := declinedSetup(t, []string{"--clients", tc.flag}, []string{tc.client}, nil)
			if code != 0 || !strings.Contains(text, tc.file) {
				t.Fatalf("code %d\n%s", code, text)
			}
		})
	}
}

func TestSetupClientsStillRefuseAnUnknownName(t *testing.T) {
	text, code := declinedSetup(t, []string{"--clients", "emacs"}, nil, nil)
	if code != 2 || !strings.Contains(text, `unknown client "emacs"`) {
		t.Fatalf("code %d\n%s", code, text)
	}
}

func TestSetupWarnsWhenTheEnvironmentOverridesTheChosenHub(t *testing.T) {
	vars := map[string]string{"IRIS_HUB_URL": "https://other.example"}
	text, _ := declinedSetup(t, []string{"--hub-url", hubclient.DefaultHubURL}, nil, vars)
	if !strings.Contains(text, "IRIS_HUB_URL is set to https://other.example") {
		t.Fatal(text)
	}
	if !strings.Contains(text, "Hub: "+hubclient.DefaultHubURL+"\n") {
		t.Fatal(text)
	}
}

func TestSetupDoesNotWarnWhenTheEnvironmentAgrees(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"unset":          nil,
		"same":           {"IRIS_HUB_URL": "https://hub.example"},
		"trailing slash": {"IRIS_HUB_URL": "https://hub.example/"},
	} {
		t.Run(name, func(t *testing.T) {
			text, _ := declinedSetup(t, []string{"--hub-url", "https://hub.example"}, nil, vars)
			if strings.Contains(text, "IRIS_HUB_URL") {
				t.Fatal(text)
			}
		})
	}
}

func TestServeCommandQuotesATokenPathWithSpaces(t *testing.T) {
	if got := serveCommand("127.0.0.1:7878", "/plain/hub.token"); got != "iris serve --listen 127.0.0.1:7878 --token-file /plain/hub.token" {
		t.Fatal(got)
	}
	want := `iris serve --listen 127.0.0.1:7878 --token-file "C:\Users\A B\hub.token"`
	if got := serveCommand("127.0.0.1:7878", `C:\Users\A B\hub.token`); got != want {
		t.Fatal(got)
	}
}

func TestSetupStopsOnABrokenClientConfiguration(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "client.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := setup([]string{"--yes"}, setupEnv{
		out: &out, errw: &out, getenv: func(string) string { return "" },
		tokenFile: filepath.Join(home, "hub.token"),
		detect:    func(func(string) (string, error)) []string { return nil },
	})
	if code != 1 || !strings.Contains(out.String(), "client.json") {
		t.Fatalf("code %d\n%s", code, out.String())
	}
}

func TestSetupStopsWhenItCannotSaveTheConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory modes are not enforced here")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var out bytes.Buffer
	code := setup([]string{"--yes"}, setupEnv{
		out: &out, errw: &out, getenv: func(string) string { return "" },
		tokenFile: filepath.Join(dir, "hub.token"),
		detect:    func(func(string) (string, error)) []string { return nil },
		installService: func(string, string) error {
			t.Fatal("installed a service after the configuration failed")
			return nil
		},
	})
	if code != 1 {
		t.Fatalf("code %d\n%s", code, out.String())
	}
}
