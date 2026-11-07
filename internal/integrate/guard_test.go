package integrate

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestGuardOutcomes(t *testing.T) {
	deny := `{"decision":"deny"}`
	cases := []struct{ client, event, allow string }{
		{Copilot, "preToolUse", "{}"},
		{Antigravity, "PreToolUse", `{"decision":"allow"}`},
	}
	for _, tc := range cases {
		t.Run(tc.client+"/missing", func(t *testing.T) {
			out, code := runGuard(t, Guard(runtime.GOOS, tc.client, filepath.Join(t.TempDir(), "iris"), tc.event), guardPATH(t, ""))
			assertGuardJSON(t, out, code, tc.allow)
		})
		t.Run(tc.client+"/crash", func(t *testing.T) {
			dir := t.TempDir()
			path, err := PlaceFake(dir, "iris")
			if err != nil {
				t.Fatal(err)
			}
			out, code := runGuard(t, Guard(runtime.GOOS, tc.client, path, tc.event), guardPATH(t, dir), "IRIS_FAKE_EXIT=3")
			assertGuardJSON(t, out, code, tc.allow)
		})
		// The client's PATH need not contain the recorded binary's directory. Checking PATH
		// instead of the recorded path skipped every hook silently (measured with Copilot).
		t.Run(tc.client+"/recorded path off PATH", func(t *testing.T) {
			dir := t.TempDir()
			path, err := PlaceFake(dir, "iris")
			if err != nil {
				t.Fatal(err)
			}
			out, code := runGuard(t, Guard(runtime.GOOS, tc.client, path, tc.event), guardPATH(t, ""), "IRIS_FAKE_EXIT=0", "IRIS_FAKE_STDOUT="+deny)
			assertGuardJSON(t, out, code, deny)
		})
		// cmd splits an unquoted path at ( ) & and ^, inside the guard's blocks.
		t.Run(tc.client+"/recorded path with shell characters", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tools(1)&a^b")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path, err := PlaceFake(dir, "iris")
			if err != nil {
				t.Fatal(err)
			}
			out, code := runGuard(t, Guard(runtime.GOOS, tc.client, path, tc.event), guardPATH(t, ""), "IRIS_FAKE_EXIT=0", "IRIS_FAKE_STDOUT="+deny)
			assertGuardJSON(t, out, code, deny)
			missing := filepath.Join(dir, "gone(2)", "iris")
			out, code = runGuard(t, Guard(runtime.GOOS, tc.client, missing, tc.event), guardPATH(t, ""))
			assertGuardJSON(t, out, code, tc.allow)
		})
		t.Run(tc.client+"/recorded path gone, iris on PATH", func(t *testing.T) {
			dir := t.TempDir()
			if _, err := PlaceFake(dir, "iris"); err != nil {
				t.Fatal(err)
			}
			gone := filepath.Join(t.TempDir(), "moved", "iris")
			out, code := runGuard(t, Guard(runtime.GOOS, tc.client, gone, tc.event), guardPATH(t, dir), "IRIS_FAKE_EXIT=0", "IRIS_FAKE_STDOUT="+deny)
			assertGuardJSON(t, out, code, deny)
		})
		t.Run(tc.client+"/deny", func(t *testing.T) {
			dir := t.TempDir()
			path, err := PlaceFake(dir, "iris")
			if err != nil {
				t.Fatal(err)
			}
			out, code := runGuard(t, Guard(runtime.GOOS, tc.client, path, tc.event), guardPATH(t, dir), "IRIS_FAKE_EXIT=0", "IRIS_FAKE_STDOUT="+deny)
			assertGuardJSON(t, out, code, deny)
		})
	}
}

func TestWindowsGuardStrings(t *testing.T) {
	ps := Guard("windows", Copilot, `C:\Program Files\iris\iris.exe`, "preToolUse")
	if !strings.Contains(ps, "Get-Command") || !strings.Contains(ps, "$LASTEXITCODE") || !strings.Contains(ps, "exit 0") || !strings.Contains(ps, "{}") {
		t.Fatal(ps)
	}
	cmd := Guard("windows", Antigravity, `C:\iris\iris.exe`, "PreToolUse")
	if !strings.HasPrefix(cmd, `if exist "C:\iris\iris.exe" ("C:\iris\iris.exe" hook`) || !strings.Contains(cmd, "where iris") || !strings.HasSuffix(cmd, `|| echo {"decision":"allow"})`) {
		t.Fatal(cmd)
	}
	spaced := Guard("windows", Antigravity, `C:\Program Files\iris\iris.exe`, "PreToolUse")
	if !strings.HasPrefix(spaced, `if exist "C:\Program Files\iris\iris.exe" ("C:\Program Files\iris\iris.exe" hook`) {
		t.Fatal(spaced)
	}
	if !strings.Contains(ps, "Test-Path -LiteralPath $i") || !strings.Contains(ps, `$i = 'C:\Program Files\iris\iris.exe'`) {
		t.Fatal(ps)
	}
	cur := cursorCommand("windows", `C:\iris\iris.exe`, "sessionStart")
	if !strings.Contains(cur, "& ") || !strings.Contains(cur, "--client cursor --event sessionStart") {
		t.Fatal(cur)
	}
}

func guardPATH(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		if dir != "" {
			return dir
		}
		return t.TempDir()
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	base := filepath.Join(root, "System32")
	if dir == "" {
		return base
	}
	return dir + ";" + base
}

func assertGuardJSON(t *testing.T, out string, code int, want string) {
	t.Helper()
	got := strings.TrimRight(out, "\r\n")
	var have, need any
	if err := json.Unmarshal([]byte(got), &have); err != nil || json.Unmarshal([]byte(want), &need) != nil || code != 0 || !reflect.DeepEqual(have, need) {
		t.Fatalf("code %d output %q want %s", code, out, want)
	}
}

func runGuard(t *testing.T, script, path string, extra ...string) (string, int) {
	t.Helper()
	cmd := startGuard(script)
	cmd.Env = envWithPath(path, extra...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("guard start: %v\n%s", err, out)
	}
	return string(out), exit.ExitCode()
}

func envWithPath(path string, extra ...string) []string {
	out := make([]string, 0, len(os.Environ())+1+len(extra))
	for _, e := range os.Environ() {
		key, _, ok := strings.Cut(e, "=")
		if ok && (strings.EqualFold(key, "PATH") || strings.HasPrefix(key, "IRIS_FAKE_")) {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "PATH="+path)
	return append(out, extra...)
}
