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
