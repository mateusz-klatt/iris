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
	if !strings.Contains(cmd, "where") || !strings.HasSuffix(cmd, `|| echo {"decision":"allow"}`) {
		t.Fatal(cmd)
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
