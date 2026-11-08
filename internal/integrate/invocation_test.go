package integrate

import (
	"errors"
	"testing"
)

func TestClientShell(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		for _, client := range Names {
			want := ShellSh
			if goos == "windows" {
				want = ShellCmd
				if client == Copilot || client == Cursor {
					want = ShellPowerShell
				}
			}
			if got := clientShell(goos, client); got != want {
				t.Fatalf("%s/%s: %s want %s", goos, client, got, want)
			}
		}
	}
}
func TestPowerShellPreference(t *testing.T) {
	for _, found := range []bool{true, false} {
		want := "powershell"
		if found {
			want = "path/to/pwsh"
		}
		got := powershellExe(func(name string) (string, error) {
			if name != "pwsh" {
				t.Fatal(name)
			}
			if found {
				return want, nil
			}
			return "", errors.New("missing")
		})
		if got != want {
			t.Fatalf("%s want %s", got, want)
		}
	}
}
