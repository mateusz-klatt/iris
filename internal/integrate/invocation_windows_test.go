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
	for _, client := range []string{Copilot, Cursor, Codex, Antigravity, Kimi} {
		t.Run(client, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "path with spaces")
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
			// Exercise the same command read back by doctor, including both PS guards
			// and direct PowerShell invocation. Native stdin must receive the JSON.
			l := Layout{Home: t.TempDir(), GOOS: "windows", Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return bin, nil }}
			c, _ := ByName(client)
			if err := c.Install(l, nil); err != nil {
				t.Fatal(err)
			}
			in, err := c.Canary(l)
			if err != nil {
				t.Fatal(err)
			}
			if in.Kind != clientShell("windows", client) {
				t.Fatal(in.Kind)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ran, err := in.Run(ctx, []byte(payload))
			out, code := ran.Stdout, ran.Exit
			if err != nil || code != 0 {
				t.Fatalf("code %d: %v (%s)", code, err, out)
			}
			assertGuardJSON(t, string(out), code, "{}")
			raw, err := os.ReadFile(receipt)
			if err != nil || string(raw) != payload {
				t.Fatalf("stdin receipt %q: %v", raw, err)
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
