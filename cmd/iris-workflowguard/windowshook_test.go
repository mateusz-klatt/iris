package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeBuild stands in for go and goreleaser. go build succeeds unless fail names the step; a
// goreleaser build behaves as the real hook would for each case, or as bend says.
type fakeBuild struct {
	t    *testing.T
	fail string
	bend func(c string, out *string, err *error, dist string)
	ran  []string
}

func (f *fakeBuild) command(dir string, env []string, name string, args ...string) (string, error) {
	f.ran = append(f.ran, name+" "+strings.Join(args, " "))
	if name == "go" {
		if strings.Contains(strings.Join(args, " "), f.fail) && f.fail != "" {
			return "build output", errors.New("exit status 1")
		}
		return "", nil
	}
	config := args[slices.Index(args, "--config")+1]
	c := strings.TrimSuffix(filepath.Base(config), ".yaml")
	dist := filepath.Join(filepath.Dir(config), c)
	has := func(v string) bool {
		return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, v+"=") })
	}
	var out string
	var err error
	switch {
	case c == "snapshot":
		f.check(slices.Contains(args, "--snapshot"), "the snapshot case is not a snapshot")
	case !has("IRIS_RELEASE_HELPER"):
		out, err = "hook failed: IRIS_RELEASE_HELPER is not set", errors.New("exit status 1")
	case !has("CERTUM_OTP"):
		out, err = "release tools: signing configuration is incomplete", errors.New("exit status 1")
	default:
		f.mark(dist, "called once")
		if c == "repeated-hook" {
			out, err = "release tools: Windows signing failed", errors.New("exit status 1")
		}
	}
	if f.bend != nil {
		f.bend(c, &out, &err, dist)
	}
	return out, err
}

func (f *fakeBuild) check(ok bool, msg string) {
	if !ok {
		f.t.Error(msg)
	}
}

func (f *fakeBuild) mark(dist, content string) {
	dir := filepath.Join(dist, "iris_windows_amd64_v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "iris.exe"), []byte("binary"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "iris.exe.signed"), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func exerciseWith(t *testing.T, f *fakeBuild) hookExercise {
	t.Helper()
	return hookExercise{root: repoRoot, temp: t.TempDir(), environ: []string{"PATH=/bin", "CERTUM_OTP=real", "KEEP=1"}, command: f.command, write: os.WriteFile}
}

// The exercise builds the helper and the signer, then runs goreleaser once per case, and passes
// when each case's result, signer calls and output match.
func TestTheWindowsHookExercisePassesWhenTheHookBehaves(t *testing.T) {
	f := &fakeBuild{t: t}
	var out bytes.Buffer
	if err := exerciseWith(t, f).run(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), ": expected hook result and signer call count\n") != len(hookCases) {
		t.Fatalf("%s", out.String())
	}
	if len(f.ran) != 2+len(hookCases) || !strings.HasPrefix(f.ran[0], "go build -trimpath -o ") || !strings.HasSuffix(f.ran[0], " ./cmd/iris-release-tools") {
		t.Fatalf("ran %v", f.ran)
	}
}

// Each way a case can go wrong is reported with the case's name.
func TestTheWindowsHookExerciseReportsEachMismatch(t *testing.T) {
	for _, c := range []struct {
		name, want string
		bend       func(c string, out *string, err *error, dist string)
	}{
		{"a snapshot that fails", "snapshot: goreleaser succeeded false, want true", func(c string, out *string, err *error, _ string) {
			if c == "snapshot" {
				*err = errors.New("exit status 1")
			}
		}},
		{"a signer that never runs", "signed: the signer ran 0 times, want 1", func(c string, _ *string, _ *error, dist string) {
			if c == "signed" {
				_ = os.RemoveAll(dist)
			}
		}},
		{"a signer that wrote something else", "does not say called once", func(c string, _ *string, _ *error, dist string) {
			if c == "signed" {
				_ = os.WriteFile(filepath.Join(dist, "iris_windows_amd64_v1", "iris.exe.signed"), []byte("twice"), 0o600)
			}
		}},
		{"a missing line", `missing-credentials: the output lacks "release tools: signing configuration is incomplete"`, func(c string, out *string, _ *error, _ string) {
			if c == "missing-credentials" {
				*out = "other"
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := exerciseWith(t, &fakeBuild{t: t, bend: c.bend}).run(&bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v", err)
			}
		})
	}
}

// A failed build, a missing goreleaser config, an unwritable file or an unusable temp directory
// stops the exercise with what failed.
func TestTheWindowsHookExerciseStopsOnSetupFailures(t *testing.T) {
	for _, step := range []string{"./cmd/iris-release-tools", "signer.go"} {
		err := exerciseWith(t, &fakeBuild{t: t, fail: step}).run(&bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "build output") {
			t.Errorf("%s: %v", step, err)
		}
	}
	e := exerciseWith(t, &fakeBuild{t: t})
	e.root = t.TempDir()
	if err := e.run(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), ".goreleaser.yaml") {
		t.Errorf("no config: %v", err)
	}
	for _, refused := range []string{"signer.go", "snapshot.yaml"} {
		e := exerciseWith(t, &fakeBuild{t: t})
		e.write = func(path string, data []byte, perm os.FileMode) error {
			if filepath.Base(path) == refused {
				return errors.New("disk full")
			}
			return os.WriteFile(path, data, perm)
		}
		if err := e.run(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("%s: %v", refused, err)
		}
	}
	e = exerciseWith(t, &fakeBuild{t: t})
	e.temp = filepath.Join(t.TempDir(), "missing")
	if err := e.run(&bytes.Buffer{}); err == nil {
		t.Error("a missing temp directory was used")
	}
}

// The environment keeps what is not replaced, replaces what is set, and drops the missing
// variable even when it would otherwise be set.
func TestEnvironmentReplacesAndDrops(t *testing.T) {
	got := environment([]string{"A=1", "B=2", "C=3"}, map[string]string{"B": "9", "D": "4"}, "C")
	if !slices.Equal(got, []string{"A=1", "B=9", "D=4"}) {
		t.Fatal(got)
	}
	if got := environment([]string{"A=1"}, map[string]string{"A": "2"}, "A"); len(got) != 0 {
		t.Fatal(got)
	}
}

// The real exercise runs commands from the working directory; outside the repository the helper
// build fails, and the command runner returns what the command printed.
func TestTheDefaultExerciseRunsRealCommands(t *testing.T) {
	t.Setenv("RUNNER_TEMP", t.TempDir())
	if err := defaultExercise(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "building the release helper") {
		t.Fatalf("%v", err)
	}
	if out, err := runCommand(".", os.Environ(), "go", "version"); err != nil || !strings.HasPrefix(out, "go version") {
		t.Fatalf("%q %v", out, err)
	}
	if exeSuffix("windows") != ".exe" || exeSuffix("linux") != "" {
		t.Fatal("exeSuffix")
	}
}
