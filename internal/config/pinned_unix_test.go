//go:build !windows

package config

import (
	"errors"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedUnixHelpersCoverAccountAndPlatformBranches(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "bogus")
	t.Setenv("HOME", bogus)
	t.Setenv("XDG_CONFIG_HOME", bogus)
	if dir, err := PinnedDir(); err != nil || !filepath.IsAbs(dir) || strings.HasPrefix(dir, bogus) {
		t.Fatalf("PinnedDir = %q, %v", dir, err)
	}
	if env, err := PinnedRuntimeEnv(); err != nil || len(env) != 1 || strings.Contains(env[0], bogus) {
		t.Fatalf("PinnedRuntimeEnv = %v, %v", env, err)
	}

	okLookup := func(uid string) (*user.User, error) {
		if uid != "1000" {
			return nil, errors.New("unexpected uid")
		}
		return &user.User{HomeDir: "/Users/alice"}, nil
	}
	if got, err := pinnedConfigDirUnix(okLookup, 1000, "linux"); err != nil || got != filepath.Join("/Users/alice", ".config", "iris") {
		t.Fatalf("linux = %q, %v", got, err)
	}
	if got, err := pinnedConfigDirUnix(okLookup, 1000, "darwin"); err != nil || got != filepath.Join("/Users/alice", "Library", "Application Support", "iris") {
		t.Fatalf("darwin = %q, %v", got, err)
	}
	if got, err := pinnedRuntimeEnvUnix(okLookup, 1000); err != nil || len(got) != 1 || got[0] != "HOME=/Users/alice" {
		t.Fatalf("env = %v, %v", got, err)
	}

	errLookup := func(string) (*user.User, error) { return nil, errors.New("no passwd") }
	if _, err := pinnedConfigDirUnix(errLookup, 1000, "linux"); err == nil {
		t.Fatal("expected lookup error")
	}
	if _, err := pinnedRuntimeEnvUnix(errLookup, 1000); err == nil {
		t.Fatal("expected runtime env lookup error")
	}

	relLookup := func(string) (*user.User, error) { return &user.User{HomeDir: "relative/home"}, nil }
	if _, err := pinnedAccountHome(relLookup, 1000); err == nil {
		t.Fatal("expected relative home rejection")
	}
	nilLookup := func(string) (*user.User, error) { return nil, nil }
	if _, err := pinnedAccountHome(nilLookup, 1000); err == nil {
		t.Fatal("expected nil user rejection")
	}
}
