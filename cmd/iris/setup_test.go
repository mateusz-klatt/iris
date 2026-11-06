package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/doctor"
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

func TestSetupServiceFailureEndsWithNextStep(t *testing.T) {
	for _, installErr := range []error{errors.New("access denied"), nil} {
		var out bytes.Buffer
		code := setup([]string{"--yes"}, setupEnv{
			out: &out, errw: &out, getenv: func(string) string { return "" }, tokenFile: filepath.Join(t.TempDir(), "hub.token"),
			detect:         func(func(string) (string, error)) []string { return nil },
			installService: func(string, string) error { return installErr },
			runDoctor:      func(context.Context, doctor.Options) (doctor.Result, error) { return doctor.Result{}, nil },
		})
		text := out.String()
		reason := "hub is unreachable after service installation"
		if installErr != nil {
			reason = installErr.Error()
		}
		if code != 1 || !strings.HasSuffix(text, "Service could not start: "+reason+"\nNext step: iris serve\n") {
			t.Fatalf("code %d\n%s", code, text)
		}
	}
}
