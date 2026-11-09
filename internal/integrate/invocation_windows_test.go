package integrate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWindowsClientInvocationStdin(t *testing.T) {
	for _, tc := range []struct{ client, dir string }{
		{Copilot, "path with spaces"},
		{Cursor, "path with spaces"},
		{Codex, "path with spaces"},
		{Antigravity, "path with spaces"},
		{Kimi, "path with spaces"},
		{Codex, "installed=true"},
	} {
		t.Run(tc.client+"/"+tc.dir, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tc.dir)
			bin, err := PlaceFake(dir, "iris")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+";"+os.Getenv("PATH"))
			payload := `{"session_id":"canary","tool_input":{"path":"space here"}}`
			t.Setenv("IRIS_FAKE_STDIN", payload)
			receipt := filepath.Join(t.TempDir(), "stdin.json")
			t.Setenv("IRIS_FAKE_STDIN_RECEIPT", receipt)
			t.Setenv("IRIS_FAKE_STDOUT", "{}")
			t.Setenv("IRIS_FAKE_EXIT", "0")
			t.Setenv("IRIS_FAKE_NO_RECEIPT", "0")
			// Exercise the same command read back by doctor, including both PS guards
			// and direct PowerShell invocation. Native stdin must receive the JSON.
			l := Layout{Home: t.TempDir(), GOOS: "windows", Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return bin, nil }}
			c, _ := ByName(tc.client)
			if err := c.Install(l, nil); err != nil {
				t.Fatal(err)
			}
			in, err := c.Canary(l)
			if err != nil {
				t.Fatal(err)
			}
			if in.Kind != clientShell("windows", tc.client) {
				t.Fatal(in.Kind)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const nonce = "windows-invocation-receipt"
			var env []string
			if tc.client == Codex {
				env = []string{"IRIS_DOCTOR_NONCE=" + nonce}
			}
			ran, err := in.Run(ctx, []byte(payload), env...)
			out, code := ran.Stdout, ran.Exit
			if err != nil || code != 0 {
				t.Fatalf("code %d: %v (stdout %s; stderr %s)", code, err, out, ran.Stderr)
			}
			assertGuardJSON(t, string(out), code, "{}")
			if tc.client == Codex && string(ran.Stderr) != "iris: canary "+nonce+"\n" {
				t.Fatalf("canary receipt %q", ran.Stderr)
			}
			raw, err := os.ReadFile(receipt)
			if err != nil || string(raw) != payload {
				t.Fatalf("stdin receipt %q: %v", raw, err)
			}
		})
	}
}

func TestWindowsCmdKeepsExecutableQuotes(t *testing.T) {
	const shell = `"C:\tools\installed=true\iris.exe" hook --client codex --event SessionStart`
	for _, tc := range []struct{ name, comspec, wantExe string }{
		{"plain", "cmd.exe", "cmd.exe"},
		{"spaced", `C:\Command Tools\cmd.exe`, `"C:\Command Tools\cmd.exe"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COMSPEC", tc.comspec)
			for _, kind := range []ShellKind{ShellCmd, ""} {
				cmd, err := (Invocation{Shell: shell, Kind: kind}).start(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				want := tc.wantExe + ` /d /s /c ""C:\tools\installed=true\iris.exe" hook --client codex --event SessionStart"`
				if cmd.SysProcAttr == nil || cmd.SysProcAttr.CmdLine != want {
					t.Fatalf("kind %q: command line %v, want %q", kind, cmd.SysProcAttr, want)
				}
			}
		})
	}
}

func TestWindowsCmdWithoutComspec(t *testing.T) {
	t.Setenv("COMSPEC", "")
	for _, kind := range []ShellKind{ShellCmd, ""} {
		in := Invocation{Shell: "echo {}", Kind: kind}
		ran, err := in.Run(context.Background(), nil)
		out, code := ran.Stdout, ran.Exit
		if err != nil || code != 0 || strings.TrimSpace(string(out)) != "{}" {
			t.Fatalf("kind %q: %d %v %q", kind, code, err, out)
		}
	}
}

func TestWindowsShShell(t *testing.T) {
	in := Invocation{Shell: "echo from-sh", Kind: ShellSh}
	cmd, err := in.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"sh", "-c", "echo from-sh"}) {
		t.Fatal(cmd.Args)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this machine")
	}
	ran, err := in.Run(context.Background(), nil)
	out, code := ran.Stdout, ran.Exit
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != "from-sh" {
		t.Fatalf("%d %v %q", code, err, out)
	}
}

func TestWindowsRefusesAnUnknownShell(t *testing.T) {
	if _, err := (Invocation{Shell: "echo", Kind: "fish"}).Run(context.Background(), nil); err == nil {
		t.Fatal("no error")
	}
}
