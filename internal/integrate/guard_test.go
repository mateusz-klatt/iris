package integrate

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGuardMissingIris(t *testing.T) {
	empty := t.TempDir()
	cases := []struct{ client, event, want string }{
		{Copilot, "preToolUse", "{}"},
		{Antigravity, "PreToolUse", `{"decision":"allow"}`},
	}
	for _, tc := range cases {
		got := runGuard(t, Guard("linux", tc.client, "/no/such/iris", tc.event), empty)
		if strings.TrimSpace(got) != tc.want {
			t.Fatalf("%s: %q", tc.client, got)
		}
	}
}

func TestGuardCrashIsAllow(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/iris"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := runGuard(t, Guard("linux", Copilot, path, "preToolUse"), dir)
	if strings.TrimSpace(got) != "{}" {
		t.Fatalf("%q", got)
	}
	got = runGuard(t, Guard("linux", Antigravity, path, "PreToolUse"), dir)
	if strings.TrimSpace(got) != `{"decision":"allow"}` {
		t.Fatalf("%q", got)
	}
}

func TestWindowsGuardStrings(t *testing.T) {
	ps := Guard("windows", Copilot, `C:\Program Files\iris\iris.exe`, "preToolUse")
	if !strings.Contains(ps, "Get-Command") || !strings.Contains(ps, "$LASTEXITCODE") || !strings.Contains(ps, "{}") {
		t.Fatal(ps)
	}
	cmd := Guard("windows", Antigravity, `C:\iris\iris.exe`, "PreToolUse")
	if !strings.Contains(cmd, "where") || !strings.Contains(cmd, `{"decision":"allow"}`) {
		t.Fatal(cmd)
	}
	cur := cursorCommand("windows", `C:\iris\iris.exe`, "sessionStart")
	if !strings.Contains(cur, "& ") || !strings.Contains(cur, "--client cursor --event sessionStart") {
		t.Fatal(cur)
	}
}

func runGuard(t *testing.T, script, path string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + path}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guard exit: %v\n%s", err, out)
	}
	return string(out)
}
