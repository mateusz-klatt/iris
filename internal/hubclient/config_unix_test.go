//go:build !windows

package hubclient

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSessionSuccessionRejectsFIFOPipeAndSwapRace(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	cfgPath := filepath.Join(dir, "client.json")
	if err := syscall.Mkfifo(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(cfgPath) })
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(string) string { return "" }}
	if e.SuccessionEnabled() {
		t.Fatal("FIFO client.json enabled succession")
	}
	_ = os.Remove(cfgPath)

	// Swap between Lstat and open: symlink/replacement detected by openRegularConfig.
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := os.WriteFile(a, []byte(`{"session_succession":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(`{"session_succession":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	origLstat := unixConfigLstat
	origOpen := unixConfigOpen
	t.Cleanup(func() {
		unixConfigLstat = origLstat
		unixConfigOpen = origOpen
	})
	// Regular file swapped with a different inode after Lstat -> rejected.
	if err := os.Rename(a, cfgPath); err != nil {
		t.Fatal(err)
	}
	swapped := false
	unixConfigOpen = func(p string, flag int, perm os.FileMode) (*os.File, error) {
		if !swapped && p == cfgPath {
			swapped = true
			if err := os.Rename(b, cfgPath); err != nil {
				t.Fatal(err)
			}
		}
		return origOpen(p, flag, perm)
	}
	if e.SuccessionEnabled() {
		t.Fatal("accepted swapped file between Lstat and open")
	}
	// Open failure, Stat failure, and non-regular opened file inside openRegularConfig.
	unixConfigOpen = func(p string, flag int, perm os.FileMode) (*os.File, error) {
		f, err := origOpen(p, flag, perm)
		if err == nil {
			_ = f.Close()
		}
		return f, err
	}
	if e.SuccessionEnabled() {
		t.Fatal("accepted closed fd inside openRegularConfig")
	}
	unixConfigOpen = func(string, int, os.FileMode) (*os.File, error) {
		return os.Open(dir)
	}
	if e.SuccessionEnabled() {
		t.Fatal("accepted directory fd inside openRegularConfig")
	}
	unixConfigOpen = func(string, int, os.FileMode) (*os.File, error) {
		return nil, os.ErrPermission
	}
	if e.SuccessionEnabled() {
		t.Fatal("accepted open error inside openRegularConfig")
	}
}
