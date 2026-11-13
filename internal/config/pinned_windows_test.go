//go:build windows

package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedWindowsHelpersCoverErrorAndRelativeBranches(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "bogus")
	t.Setenv("APPDATA", bogus)
	t.Setenv("USERPROFILE", bogus)
	if dir, err := pinnedConfigDirOS(); err != nil || !filepath.IsAbs(dir) || strings.HasPrefix(dir, bogus) {
		t.Fatalf("pinnedConfigDirOS = %q, %v", dir, err)
	}
	if env, err := pinnedRuntimeEnvOS(); err != nil || len(env) != 1 || strings.Contains(env[0], bogus) {
		t.Fatalf("pinnedRuntimeEnvOS = %v, %v", env, err)
	}

	if got, err := pinnedConfigDirWindows(func() (string, error) { return `C:\Users\alice`, nil }); err != nil || got != filepath.Join(`C:\Users\alice`, "AppData", "Roaming", "iris") {
		t.Fatalf("configDir = %q, %v", got, err)
	}
	if _, err := pinnedConfigDirWindows(func() (string, error) { return "", errors.New("folder err") }); err == nil {
		t.Fatal("expected folder error")
	}
	if _, err := pinnedConfigDirWindows(func() (string, error) { return `relative\profile`, nil }); err == nil {
		t.Fatal("expected relative profile error")
	}

	if got, err := pinnedRuntimeEnvWindows(func() (string, error) { return `C:\Windows\System32`, nil }); err != nil || len(got) != 1 || got[0] != `SYSTEMROOT=C:\Windows` {
		t.Fatalf("runtimeEnv = %v, %v", got, err)
	}
	if _, err := pinnedRuntimeEnvWindows(func() (string, error) { return "", errors.New("sysdir err") }); err == nil {
		t.Fatal("expected sysdir error")
	}
	if _, err := pinnedRuntimeEnvWindows(func() (string, error) { return `relative\System32`, nil }); err == nil {
		t.Fatal("expected relative sysroot error")
	}
}
