package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iris-config-test")
	if err != nil {
		panic(err)
	}
	restore := SetPinnedForTest(filepath.Join(dir, "pinned-iris"))
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestSetPinnedForTestIsolatesAndRestores(t *testing.T) {
	defaultDir, err := PinnedDir()
	if err != nil || !strings.HasSuffix(defaultDir, "pinned-iris") {
		t.Fatalf("TestMain PinnedDir = %q, %v", defaultDir, err)
	}
	custom := filepath.Join(t.TempDir(), "custom-pinned")
	restore := SetPinnedForTest(custom)
	gotDir, err := PinnedDir()
	if err != nil || gotDir != custom {
		t.Fatalf("PinnedDir = %q, %v, want %q", gotDir, err, custom)
	}
	env1, err := PinnedRuntimeEnv()
	if err != nil || len(env1) != 1 {
		t.Fatalf("PinnedRuntimeEnv = %v, %v", env1, err)
	}
	env1[0] = "MUTATED=1"
	env2, err := PinnedRuntimeEnv()
	if err != nil || env2[0] == "MUTATED=1" {
		t.Fatalf("PinnedRuntimeEnv slice was not copied: %v, %v", env2, err)
	}
	restore()
	restoredDir, err := PinnedDir()
	if err != nil || restoredDir != defaultDir {
		t.Fatalf("restored PinnedDir = %q, %v, want %q", restoredDir, err, defaultDir)
	}

	if got := testPinnedRuntimeEnv("windows", "/tmp/d", `C:\Windows`); len(got) != 1 || got[0] != `SYSTEMROOT=C:\Windows` {
		t.Fatalf("windows runtime env = %v", got)
	}
	if got := testPinnedRuntimeEnv("windows", "/tmp/d", ""); len(got) != 1 || got[0] != "HOME=/tmp/d" {
		t.Fatalf("windows empty systemroot runtime env = %v", got)
	}
	if got := testPinnedRuntimeEnv("linux", "/tmp/d", `C:\Windows`); len(got) != 1 || got[0] != "HOME=/tmp/d" {
		t.Fatalf("linux runtime env = %v", got)
	}
}
