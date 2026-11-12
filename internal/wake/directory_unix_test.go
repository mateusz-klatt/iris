//go:build !windows

package wake

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStateUnixDirectoriesAreExactlyPrivateBeforeAnySnapshot(t *testing.T) {
	for _, mode := range []os.FileMode{0500, 0000, 0777, 0700, 0700 | os.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			s := testStore(t)
			if err := os.MkdirAll(filepath.Dir(s.path()), 0700); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{filepath.Dir(s.path()), s.sessions.Dir()} {
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatal(err)
				}
			}
			before := mustSnapshot(t, s)
			mustPublish(t, s, before, testDocument(testAttempt(1, "queue", "")))
			for _, path := range []string{s.sessions.Dir(), filepath.Dir(s.path()), s.path()} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				want := os.FileMode(0700)
				if path == s.path() {
					want = 0600
				}
				if info.Mode().Perm() != want || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
					t.Fatalf("%s has unsafe mode %s", path, info.Mode())
				}
			}
		})
	}
	// Umask is process-wide, so this test and the directory seams never run in parallel.
	s := testStore(t)
	old := unix.Umask(0777)
	defer unix.Umask(old)
	mustPublish(t, s, mustSnapshot(t, s), testDocument(testAttempt(1, "queue", "")))
	for _, dir := range []string{s.sessions.Dir(), filepath.Dir(s.path())} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("restrictive umask left unusable directory: %v %v", info, err)
		}
	}
}

func TestStateUnixDirectoryProtectionRefusesFailuresAndForeignOwnership(t *testing.T) {
	for _, boundary := range []string{"mkdir", "stat", "regular file", "owner stat", "foreign owner", "chmod", "verify stat", "verify mode"} {
		t.Run(boundary, func(t *testing.T) {
			makeDir, stat, owner, chmod := directoryMake, directoryStat, directoryOwner, directoryChmod
			t.Cleanup(func() { directoryMake, directoryStat, directoryOwner, directoryChmod = makeDir, stat, owner, chmod })
			dir := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch boundary {
			case "mkdir":
				directoryMake = func(string, os.FileMode) error { return errStateTest }
			case "stat":
				directoryStat = func(string) (os.FileInfo, error) { return nil, errStateTest }
			case "regular file":
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, nil, 0600); err != nil {
					t.Fatal(err)
				}
				directoryMake = func(string, os.FileMode) error { return nil }
			case "owner stat":
				directoryOwner = func(string, *unix.Stat_t) error { return errStateTest }
			case "foreign owner":
				directoryOwner = func(_ string, s *unix.Stat_t) error { s.Uid = uint32(os.Geteuid()) + 1; return nil }
			case "chmod":
				if err := os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
				directoryChmod = func(string, os.FileMode) error { return errStateTest }
			case "verify stat":
				if err := os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
				calls := 0
				directoryStat = func(path string) (os.FileInfo, error) {
					calls++
					if calls == 2 {
						return nil, errStateTest
					}
					return stat(path)
				}
			case "verify mode":
				if err := os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
				directoryChmod = func(string, os.FileMode) error { return nil }
			}
			if err := privateDirectory(dir); err == nil {
				t.Fatal("unsafe directory was accepted")
			}
		})
	}
	// The actual mkdir boundary also refuses a symlink, even when its target is a directory.
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDirectory(link); err == nil {
		t.Fatal("symlink directory accepted")
	}
}

type faultySyncFile struct {
	syncErr, closeErr error
	closed            *bool
}

func (f faultySyncFile) Sync() error  { return f.syncErr }
func (f faultySyncFile) Close() error { *f.closed = true; return f.closeErr }

func TestStateUnixDirectorySyncReportsOpenSyncAndCloseFailures(t *testing.T) {
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing directory sync succeeded")
	}
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	open := directoryOpen
	t.Cleanup(func() { directoryOpen = open })
	for _, syncFails := range []bool{false, true} {
		closed := false
		want := errStateTest
		syncErr := error(nil)
		if syncFails {
			syncErr = errStateTest
		}
		closeErr := errors.New("close failure")
		if !syncFails {
			want = closeErr
		}
		directoryOpen = func(string) (syncFile, error) {
			return faultySyncFile{syncErr: syncErr, closeErr: closeErr, closed: &closed}, nil
		}
		if err := syncDirectory("private"); !errors.Is(err, want) || !closed {
			t.Fatalf("directory sync failure lost or handle retained: %v, closed %t", err, closed)
		}
	}
}
