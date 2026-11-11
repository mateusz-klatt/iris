package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupFakeRemovesTheBuildDirectory(t *testing.T) {
	freshFake(t)
	exe, err := fakeBinary()
	if err != nil {
		t.Fatal(err)
	}
	CleanupFake()
	if _, err := os.Stat(filepath.Dir(exe)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("build directory remains: %v", err)
	}
}

func TestCleanupTestDirRemovesOnlyTheCreatedDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("", "iris-test-home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CleanupTestDir(dir, "iris-test-home") })
	seedFile(t, filepath.Join(dir, "child", "state"), "temporary state")
	CleanupTestDir(dir, "iris-test-home")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home directory remains: %v", err)
	}
}

func TestCleanupTestDirRefusesUnsafePaths(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "iris-test-home-outside")
	seedFile(t, filepath.Join(outside, "keep"), "retained")
	for _, tc := range []struct{ dir, prefix string }{
		{"", "iris-test-home"},
		{"iris-test-home-relative", "iris-test-home"},
		{outside, "iris-test-home"},
		{os.TempDir(), "iris-test-home"},
		{filepath.Join(os.TempDir(), "other-directory"), "iris-test-home"},
		{filepath.Join(os.TempDir(), "iris-test-home"), ""},
	} {
		t.Run(tc.dir+tc.prefix, func(t *testing.T) {
			err := removeTestDir(tc.dir, tc.prefix, func(string) error {
				t.Fatal("unsafe removal attempted")
				return nil
			}, func(time.Duration) { t.Fatal("unsafe removal retried") })
			if err == nil || !strings.Contains(err.Error(), "refusing to remove") {
				t.Fatalf("unsafe path accepted: %v", err)
			}
		})
	}
	if got, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(got) != "retained" {
		t.Fatalf("refused directory changed: %q, %v", got, err)
	}
}

func TestCleanupTestDirRetriesAreBounded(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		calls, waited := 0, time.Duration(0)
		locked := errors.New("child still holds a file")
		dir := filepath.Join(os.TempDir(), "iris-test-home-retry")
		err := removeTestDir(dir, "iris-test-home", func(path string) error {
			if path != dir {
				t.Fatalf("removing %q, want %q", path, dir)
			}
			calls++
			if succeeds && calls == 3 {
				return nil
			}
			return locked
		}, func(delay time.Duration) { waited += delay })
		if succeeds {
			if err != nil || calls != 3 || waited != 200*time.Millisecond {
				t.Fatalf("transient failure: calls=%d waited=%v err=%v", calls, waited, err)
			}
		} else if !errors.Is(err, locked) || calls != 21 || waited != 2*time.Second {
			t.Fatalf("persistent failure: calls=%d waited=%v err=%v", calls, waited, err)
		}
	}
}

func TestCleanupTestDirReportsFailure(t *testing.T) {
	log, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	stderr := os.Stderr
	os.Stderr = log
	defer func() { os.Stderr = stderr }()
	CleanupTestDir("", "iris-test-home")
	if got, err := os.ReadFile(log.Name()); err != nil || !strings.Contains(string(got), "test cleanup") || !strings.Contains(string(got), "refusing to remove") {
		t.Fatalf("cleanup diagnostic missing: %q, %v", got, err)
	}
}
