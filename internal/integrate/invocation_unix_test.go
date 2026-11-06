//go:build !windows

package integrate

import (
	"context"
	"strings"
	"testing"
)

func TestInvocationChoosesTheConfiguredShell(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		kind ShellKind
		want string
	}{
		{ShellSh, "/bin/sh -c x"},
		{"", "/bin/sh -c x"},
		{ShellCmd, "cmd /d /c x"},
		{ShellPowerShell, "-NoProfile -NonInteractive -Command x"},
	}
	for _, tc := range cases {
		cmd, err := Invocation{Shell: "x", Kind: tc.kind}.start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cmd.Args, " "); !strings.HasSuffix(got, tc.want) {
			t.Errorf("%q: %q", tc.kind, got)
		}
	}
	if _, err := (Invocation{Shell: "x", Kind: "fish"}).start(ctx); err == nil {
		t.Fatal("unknown shell accepted")
	}
}

func TestInvocationRunsAShellCommand(t *testing.T) {
	out, code, err := Invocation{Shell: "cat; exit 4"}.Run(context.Background(), []byte("in"))
	if err != nil || code != 4 || string(out) != "in" {
		t.Fatalf("%q %d %v", out, code, err)
	}
}
