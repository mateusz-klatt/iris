package doctor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/guard"
)

func TestDoctorReportsTheGuardOfItsRepository(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ctx := context.Background()
	if got := guardLine(ctx, t.TempDir()); got != "not in a git repository" {
		t.Fatal(got)
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if got := guardLine(ctx, dir); !strings.HasPrefix(got, "not installed (") || !strings.HasSuffix(got, "run `iris guard install`") {
		t.Fatal(got)
	}
	if err := guard.Install(ctx, dir, "/usr/local/bin/iris"); err != nil {
		t.Fatal(err)
	}
	if got := guardLine(ctx, dir); !strings.HasPrefix(got, "installed (") || !strings.Contains(got, "pre-commit") {
		t.Fatal(got)
	}
	hook := strings.TrimSuffix(strings.TrimPrefix(guardLine(ctx, dir), "installed ("), ")")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(hook, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := guardLine(ctx, dir); !strings.HasPrefix(got, "unknown (") {
		t.Fatal(got)
	}
	// An empty directory is the working directory, which is inside this repository.
	if got := guardLine(ctx, ""); got == "not in a git repository" {
		t.Fatal(got)
	}
	text := Result{Guard: "installed (x)"}.Format()
	if !strings.Contains(text, "pre-commit guard: installed (x)\n") {
		t.Fatal(text)
	}
}
