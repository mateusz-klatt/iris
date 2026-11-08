package guard

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testIris is a recorded path that does not exist, for tests that never run the hook.
var testIris = filepath.Join(os.TempDir(), "iris-guard-tests-no-such-dir", "iris")

// fakeIris builds a binary that exits with IRIS_GUARD_TEST_EXIT, standing in for iris.
func fakeIris(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	source := filepath.Join(t.TempDir(), "fake.go")
	if err := os.WriteFile(source, []byte("package main\nimport (\"os\";\"strconv\")\nfunc main(){ code,_:=strconv.Atoi(os.Getenv(\"IRIS_GUARD_TEST_EXIT\")); os.Exit(code) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "iris"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path = filepath.Join(dir, name)
	if out, err := exec.Command("go", "build", "-o", path, source).CombinedOutput(); err != nil {
		t.Fatalf("fake iris: %v: %s", err, out)
	}
	return dir, path
}

// pathWithoutIris is the current PATH minus every directory that holds an iris, so the hook
// can only find the one a case provides.
func pathWithoutIris() string {
	var keep []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		found := false
		for _, name := range []string{"iris", "iris.exe"} {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				found = true
			}
		}
		if !found {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// recorded is the path the hook stores. Git's sh on Windows reads forward slashes.
func recorded(path string) string {
	if runtime.GOOS == "windows" {
		return filepath.ToSlash(path)
	}
	return path
}

func commitWith(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "test")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestTheHookFindsIrisOrAllowsWithAWarning(t *testing.T) {
	bin, path := fakeIris(t)
	t.Setenv("IRIS_GUARD_TEST_EXIT", "1") // iris refuses: a real conflict
	cases := []struct {
		name, iris, path string
		blocked          bool
	}{
		{"recorded path, not on PATH", recorded(path), pathWithoutIris(), true},
		{"recorded path gone, iris on PATH", recorded(filepath.Join(t.TempDir(), "gone", "iris")), bin + string(os.PathListSeparator) + pathWithoutIris(), true},
		{"neither", recorded(filepath.Join(t.TempDir(), "gone", "iris")), pathWithoutIris(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init")
			p, err := hookPath(bg, dir)
			if err != nil {
				t.Fatal(err)
			}
			// The chained original runs whenever the check did not refuse.
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(p, []byte("#!/bin/sh\nprintf ran > original-ran\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err = Install(bg, dir, tc.iris); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", tc.path)
			out, err := commitWith(t, dir)
			_, ranErr := os.Stat(filepath.Join(dir, "original-ran"))
			if tc.blocked {
				if err == nil || ranErr == nil {
					t.Fatalf("the check should refuse before the original runs: %v\n%s", err, out)
				}
				return
			}
			if err != nil || ranErr != nil {
				t.Fatalf("a missing iris must allow the commit and run the original: %v %v\n%s", err, ranErr, out)
			}
			if !strings.Contains(out, "Iris guard: iris not found; commit not checked") {
				t.Fatalf("no warning\n%s", out)
			}
		})
	}
}

func TestInstallUpgradesTheHooksIrisWroteAndLeavesOthers(t *testing.T) {
	for _, chain := range []bool{false, true} {
		dir, hook := repoHook(t)
		original := []byte("#!/bin/sh\nexit 0\n")
		if chain {
			if err := os.WriteFile(hook+".iris-original", original, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(hook, legacy(chain), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := Install(bg, dir, "/opt/iris/bin/iris"); err != nil {
			t.Fatal(err)
		}
		if got := mustRead(t, hook); !bytes.Equal(got, wrapper("/opt/iris/bin/iris", chain)) {
			t.Fatalf("v1 not upgraded (chain %v)\n%s", chain, got)
		}
		// A new recorded path rewrites an Iris hook of this version too.
		if err := Install(bg, dir, "/usr/local/bin/iris"); err != nil {
			t.Fatal(err)
		}
		if got := mustRead(t, hook); !bytes.Equal(got, wrapper("/usr/local/bin/iris", chain)) {
			t.Fatalf("v2 path not updated\n%s", got)
		}
		if chain && !bytes.Equal(mustRead(t, hook+".iris-original"), original) {
			t.Fatal("the chained original changed")
		}
		if err := Remove(bg, dir); err != nil {
			t.Fatal(err)
		}
		if chain && !bytes.Equal(mustRead(t, hook), original) {
			t.Fatal("original not restored")
		}
		if _, err := os.Stat(hook); !chain && !os.IsNotExist(err) {
			t.Fatalf("hook left behind: %v", err)
		}
	}
}

func TestAnEditedHookIsNotIris(t *testing.T) {
	dir, hook := repoHook(t)
	edited := append(wrapper("/opt/iris", false), []byte("echo mine\n")...)
	if err := os.WriteFile(hook, edited, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove(bg, dir); err == nil {
		t.Fatal("removed a hook the user edited")
	}
	if !bytes.Equal(mustRead(t, hook), edited) {
		t.Fatal("edited hook changed")
	}
}

func TestQuotedPathsSurviveTheShell(t *testing.T) {
	if got := shellQuote("/a b/it's/iris"); got != `'/a b/it'\''s/iris'` {
		t.Fatal(got)
	}
}
