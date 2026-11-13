package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestCmdIntegrateAntigravityHelperSleep(t *testing.T) {
	if os.Getenv("IRIS_CMD_INTEGRATE_AGY_HELPER") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func startCmdTestAgyHelper(t *testing.T, cfg string) (*exec.Cmd, hook.AntigravityHelperOwner) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestCmdIntegrateAntigravityHelperSleep$")
	cmd.Env = append(os.Environ(), "IRIS_CMD_INTEGRATE_AGY_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	childSnap, ok := proc.Lookup(cmd.Process.Pid)
	if !ok {
		t.Fatal("proc.Lookup(child) failed")
	}
	selfSnap, ok := proc.Lookup(os.Getpid())
	if !ok {
		t.Fatal("proc.Lookup(self) failed")
	}
	tokenPath := filepath.Join(cfg, "hub.token")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"conversationId": "conv-live"})
	(hook.Runner{
		ClientName: "antigravity",
		EventName:  "SessionStart",
		Stdin:      bytes.NewReader(raw),
		Getenv: func(k string) string {
			if k == "IRIS_TOKEN_FILE" {
				return tokenPath
			}
			return ""
		},
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: selfSnap.PID, StartMS: selfSnap.StartMS, Name: "agy"}, nil
		},
	}).Run()
	owner := hook.AntigravityHelperOwner{
		AgyPID:        selfSnap.PID,
		AgyStartMS:    selfSnap.StartMS,
		HelperPID:     childSnap.PID,
		HelperStartMS: childSnap.StartMS,
		SessionID:     "conv-live",
		AttemptID:     "att-live",
	}
	if _, err := hook.PublishAntigravityHelper(cfg, owner, nil); err != nil {
		t.Fatal(err)
	}
	return cmd, owner
}

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

func TestIntegrateGrokWakeFlagsStayLocalAndSurviveAPlainReinstall(t *testing.T) {
	home := grokWakeIntegrationHome(t)
	for _, tc := range []struct {
		args    []string
		enabled bool
		text    string
	}{
		{[]string{"grok"}, true, ""},
		{[]string{"--no-wake", "grok"}, false, "off\n"},
		{[]string{"grok"}, false, "off\n"},
		{[]string{"grok", "--wake"}, true, "on\n"},
		{[]string{"grok", "--no-wake"}, false, "off\n"},
		{[]string{"--wake=false", "grok"}, false, "off\n"},
		{[]string{"grok", "--no-wake=false"}, true, "on\n"},
	} {
		out, errText := capture(t, func() {
			if code := runIntegrate(tc.args); code != 0 {
				t.Fatalf("%v: code %d", tc.args, code)
			}
		})
		enabled, err := integrate.GrokWake(integrate.Layout{})
		marker := filepath.Join(home, "iris-wake")
		body := ""
		if raw, readErr := os.ReadFile(marker); readErr == nil {
			body = string(raw)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		if err != nil || enabled != tc.enabled || body != tc.text || !strings.Contains(out, "integrated grok") || errText != "" {
			t.Fatalf("%v: enabled %v %v marker %q stdout %q stderr %q", tc.args, enabled, err, body, out, errText)
		}
	}
	skill := filepath.Join(home, "skills", "iris", "SKILL.md")
	if !strings.Contains(string(mustReadIntegrate(t, skill)), "An Iris notification means new mail or a claim someone is waiting on.") {
		t.Fatal("skill missing the notification sentence")
	}
	out, _ := capture(t, func() {
		if code := runIntegrate([]string{"grok", "--remove"}); code != 0 {
			t.Fatalf("remove: %d", code)
		}
	})
	if !strings.Contains(out, "removed grok") {
		t.Fatalf("removal output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "iris-wake")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removal retained the wake marker: %v", err)
	}
	if _, err := os.Stat(skill); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removal retained the skill: %v", err)
	}
}

func TestIntegrateAntigravityWakeFlagsAreOptInAndSurviveReinstall(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfg := filepath.Join(dir, "cfg")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("IRIS_TOKEN_FILE", filepath.Join(cfg, "hub.token"))
	t.Setenv("IRIS_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("PATH", t.TempDir())

	origLayout := integrateLayout
	t.Cleanup(func() { integrateLayout = origLayout })
	integrateLayout = func() integrate.Layout {
		return integrate.Layout{
			PinnedConfigDir: func() (string, error) { return cfg, nil },
		}
	}

	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{[]string{"antigravity"}, false},
		{[]string{"agy", "--wake"}, true},
		{[]string{"antigravity"}, true},
		{[]string{"antigravity", "--no-wake"}, false},
		{[]string{"agy"}, false},
	} {
		out, errText := capture(t, func() {
			if code := runIntegrate(tc.args); code != 0 {
				t.Fatalf("%v: code %d", tc.args, code)
			}
		})
		enabled, err := integrate.AntigravityWake(integrate.Layout{})
		if err != nil || enabled != tc.enabled || !strings.Contains(out, "integrated antigravity") || errText != "" {
			t.Fatalf("%v: enabled %v %v stdout %q stderr %q", tc.args, enabled, err, out, errText)
		}
	}

	// Re-enable --wake, attach a live helper, and verify --no-wake terminates it.
	capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--wake"}); code != 0 {
			t.Fatal("re-enable --wake failed")
		}
	})
	child1, owner1 := startCmdTestAgyHelper(t, cfg)
	if !hook.VerifyAntigravityHelper(cfg, owner1, nil) {
		t.Fatal("owner1 should verify while wake is on")
	}
	capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--no-wake"}); code != 0 {
			t.Fatal("--no-wake failed")
		}
	})
	_ = child1.Wait()
	if hook.VerifyAntigravityHelper(cfg, owner1, nil) {
		t.Fatal("owner1 still verified after --no-wake")
	}

	// Re-enable --wake, attach a second live helper, and verify --remove terminates it and removes the marker.
	capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--wake"}); code != 0 {
			t.Fatal("re-enable --wake failed")
		}
	})
	child2, _ := startCmdTestAgyHelper(t, cfg)
	out, _ := capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--remove"}); code != 0 {
			t.Fatalf("remove: %d", code)
		}
	})
	_ = child2.Wait()
	if !strings.Contains(out, "removed antigravity") {
		t.Fatalf("removal output = %q", out)
	}
	if _, err := os.Stat(hook.AntigravityWakePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--remove retained wake marker: %v", err)
	}

	// When IRIS_TOKEN_FILE points away from the pinned directory:
	// - --wake refuses with exit 1 naming both directories;
	// - --no-wake writes "off" and stops helpers in both the environment and pinned directories, idempotently;
	// - --remove also writes "off" and stops helpers in the pinned directory, idempotently;
	// - when IrisConfigDir fails (empty HOME/USERPROFILE/IRIS_TOKEN_FILE), --no-wake still stops the pinned helper and exits 1.
	altCfg := filepath.Join(dir, "alt-cfg")
	t.Setenv("IRIS_TOKEN_FILE", filepath.Join(altCfg, "hub.token"))
	_, errText := capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--wake"}); code != 1 {
			t.Fatalf("split --wake returned %d, want 1", code)
		}
	})
	if !strings.Contains(errText, cfg) || !strings.Contains(errText, altCfg) {
		t.Fatalf("split --wake stderr = %q, want both %q and %q", errText, cfg, altCfg)
	}

	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook.AntigravityWakePath(cfg), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child3, owner3 := startCmdTestAgyHelper(t, cfg)
	for i := 0; i < 2; i++ {
		capture(t, func() {
			if code := runIntegrate([]string{"antigravity", "--no-wake"}); code != 0 {
				t.Fatalf("split --no-wake pass %d failed: %d", i, code)
			}
		})
	}
	_ = child3.Wait()
	if hook.VerifyAntigravityHelper(cfg, owner3, nil) || hook.AntigravityWakeEnabled(cfg) || hook.AntigravityWakeEnabled(altCfg) {
		t.Fatal("split --no-wake left pinned helper or wake marker active")
	}

	if err := os.WriteFile(hook.AntigravityWakePath(cfg), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child4, owner4 := startCmdTestAgyHelper(t, cfg)
	for i := 0; i < 2; i++ {
		capture(t, func() {
			if code := runIntegrate([]string{"antigravity", "--remove"}); code != 0 {
				t.Fatalf("split --remove pass %d failed: %d", i, code)
			}
		})
	}
	_ = child4.Wait()
	if hook.VerifyAntigravityHelper(cfg, owner4, nil) || hook.AntigravityWakeEnabled(cfg) {
		t.Fatal("split --remove left pinned helper or wake marker active")
	}

	if err := os.WriteFile(hook.AntigravityWakePath(cfg), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child5, owner5 := startCmdTestAgyHelper(t, cfg)
	t.Setenv("IRIS_TOKEN_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("APPDATA", "")
	capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--no-wake"}); code != 1 {
			t.Fatalf("homeless --no-wake returned %d, want 1", code)
		}
	})
	_ = child5.Wait()
	if hook.VerifyAntigravityHelper(cfg, owner5, nil) || hook.AntigravityWakeEnabled(cfg) {
		t.Fatal("homeless --no-wake left pinned helper or wake marker active")
	}
}

func TestRunIntegrateAntigravityDefaultLayoutUsesIsolatedPinnedDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("IRIS_STATE_DIR", filepath.Join(home, "state"))

	isolatedPinned, err := config.PinnedDir()
	if err != nil || !strings.HasPrefix(filepath.Clean(isolatedPinned), filepath.Clean(os.TempDir())) {
		t.Fatalf("TestMain PinnedDir = %q, %v", isolatedPinned, err)
	}
	if err := os.MkdirAll(isolatedPinned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook.AntigravityWakePath(isolatedPinned), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child1, owner1 := startCmdTestAgyHelper(t, isolatedPinned)
	capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--no-wake"}); code != 0 {
			t.Fatalf("default layout --no-wake returned %d", code)
		}
	})
	_ = child1.Wait()
	if hook.VerifyAntigravityHelper(isolatedPinned, owner1, nil) || hook.AntigravityWakeEnabled(isolatedPinned) {
		t.Fatal("default layout --no-wake did not stop helper or disable isolatedPinned marker")
	}

	if err := os.WriteFile(hook.AntigravityWakePath(isolatedPinned), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child2, owner2 := startCmdTestAgyHelper(t, isolatedPinned)
	capture(t, func() {
		if code := runIntegrate([]string{"antigravity", "--remove"}); code != 0 {
			t.Fatalf("default layout --remove returned %d", code)
		}
	})
	_ = child2.Wait()
	if hook.VerifyAntigravityHelper(isolatedPinned, owner2, nil) || hook.AntigravityWakeEnabled(isolatedPinned) {
		t.Fatal("default layout --remove did not stop helper or disable isolatedPinned marker")
	}
}

func grokWakeIntegrationHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "grok")
	t.Setenv("GROK_HOME", home)
	t.Setenv("IRIS_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("PATH", t.TempDir())
	return home
}

func mustReadIntegrate(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
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
