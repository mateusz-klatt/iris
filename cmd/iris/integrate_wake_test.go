package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

func codexWakeIntegrationHome(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	home, state := filepath.Join(dir, "codex"), filepath.Join(dir, "state")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("IRIS_STATE_DIR", state)
	// Codex's file integration runs no client, and the command path falls back to iris.
	t.Setenv("PATH", t.TempDir())
	return home, state
}

func TestIntegrateCodexWakeFlagsWorkBeforeAndAfterTheClient(t *testing.T) {
	home, _ := codexWakeIntegrationHome(t)
	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{[]string{"codex"}, true},
		{[]string{"codex", "--wake"}, true},
		{[]string{"codex"}, true},
		{[]string{"--no-wake", "codex"}, false},
		{[]string{"codex"}, false},
		{[]string{"--wake", "codex"}, true},
		{[]string{"codex", "--no-wake"}, false},
		{[]string{"codex"}, false},
		{[]string{"codex", "--wake=false"}, false},
		{[]string{"codex", "--no-wake=false"}, true},
	} {
		out, errText := capture(t, func() {
			if code := runIntegrate(tc.args); code != 0 {
				t.Fatalf("%v: code %d", tc.args, code)
			}
		})
		configured, enabled, err := integrate.CodexWake(integrate.Layout{})
		if err != nil || !configured || enabled != tc.enabled || !strings.Contains(out, "integrated codex") || errText != "" {
			t.Fatalf("%v: configured/enabled %v/%v, %v, stdout %q, stderr %q", tc.args, configured, enabled, err, out, errText)
		}
	}
	out, _ := capture(t, func() {
		if code := runIntegrate([]string{"codex", "--remove"}); code != 0 {
			t.Fatalf("remove after the client: %d", code)
		}
	})
	if !strings.Contains(out, "removed codex") {
		t.Fatalf("removal output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removal retained the generated wake configuration: %v", err)
	}
	// The positional terminator keeps its ordinary meaning while leaving flags before it valid.
	capture(t, func() {
		if code := runIntegrate([]string{"--wake", "--", "codex"}); code != 0 {
			t.Fatalf("terminated client argument: %d", code)
		}
	})
}

func TestIntegrateCodexWakeRejectsConflictingOrUnscopedFlagsWithoutWriting(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "--wake", "--no-wake"},
		{"--wake=false", "--no-wake=false", "codex"},
		{"--all", "--wake"},
		{"--no-wake", "--all"},
		{"codex", "--remove", "--wake"},
		{"--remove", "codex", "--no-wake"},
		{"claude", "--wake"},
		{"copilot", "--no-wake"},
		{"--wake"},
		{"codex", "extra", "--wake"},
		{"codex", "--unknown"},
		{"--", "codex", "--wake"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home, state := codexWakeIntegrationHome(t)
			_, errText := capture(t, func() {
				if code := runIntegrate(args); code != 2 {
					t.Fatalf("invalid wake arguments returned %d", code)
				}
			})
			if errText == "" {
				t.Fatal("invalid arguments produced no explanation")
			}
			for _, path := range []string{home, state} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid arguments wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestIntegrateCodexWakeReportsBrokenTOMLBeforeHooksAreWritten(t *testing.T) {
	home, state := codexWakeIntegrationHome(t)
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	const original = "model = [\n"
	file := filepath.Join(home, "config.toml")
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errText := capture(t, func() {
		if code := runIntegrate([]string{"codex", "--wake"}); code != 1 {
			t.Fatalf("broken TOML returned %d", code)
		}
	})
	if out != "" || !strings.Contains(errText, "codex: refusing to overwrite a broken file") {
		t.Fatalf("broken TOML output: %q, %q", out, errText)
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != original {
		t.Fatalf("broken TOML was changed: %q, %v", raw, err)
	}
	for _, path := range []string{filepath.Join(home, "hooks.json"), state} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused integration wrote %s: %v", path, err)
		}
	}
}

func TestIntegrateAllDefaultsCodexOnAndPreservesExplicitDisable(t *testing.T) {
	fakeClaude(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "custom-codex"))
	t.Setenv("IRIS_STATE_DIR", filepath.Join(home, "state"))
	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{[]string{"--all"}, true},
		{[]string{"codex", "--no-wake"}, false},
		{[]string{"--all"}, false},
		{[]string{"codex", "--wake"}, true},
		{[]string{"--all"}, true},
	} {
		capture(t, func() {
			if code := runIntegrate(tc.args); code != 0 {
				t.Fatalf("%v: exit %d", tc.args, code)
			}
		})
		configured, enabled, err := integrate.CodexWake(integrate.Layout{})
		if err != nil || !configured || enabled != tc.enabled {
			t.Fatalf("%v: configured/enabled %v/%v, %v", tc.args, configured, enabled, err)
		}
	}
}
