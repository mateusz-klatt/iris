//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func useLstat(t *testing.T, fn func(string, *unix.Stat_t) error) {
	t.Helper()
	orig := unixLstat
	unixLstat = fn
	t.Cleanup(func() { unixLstat = orig })
}

func TestTokenRemovesAPartialFileWhenItsOwnerCannotBeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.token")
	useLstat(t, func(string, *unix.Stat_t) error { return errors.New("lstat failed") })
	if _, err := Token(path); err == nil || !strings.Contains(err.Error(), "lstat failed") {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial token left behind", err)
	}
}

func TestTokenOwnedBySomeoneElseIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.token")
	if _, err := Token(path); err != nil {
		t.Fatal(err)
	}
	useLstat(t, func(_ string, st *unix.Stat_t) error {
		st.Uid = uint32(os.Geteuid()) + 1
		return nil
	})
	if _, err := Token(path); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatal(err)
	}
}

func TestTokenRefusesADirectoryAndAReadOnlyParent(t *testing.T) {
	dir := t.TempDir()
	if err := secureToken(dir); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	if _, err := Token(filepath.Join(locked, "hub.token")); err == nil {
		t.Fatal("created a token in a read-only directory")
	}
}

func TestOwnerOnlyLeavesUnixModesAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := OwnerOnly(path); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatal(info, err)
	}
}
