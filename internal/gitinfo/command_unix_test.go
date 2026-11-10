//go:build !windows

package gitinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGit puts a script named git first on PATH, so a test decides what git starts.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// startStatus runs Status until the returned stop is called, and gives its result on the channel.
func startStatus(t *testing.T) (stop context.CancelFunc, result <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := Status(ctx, root)
		done <- err
	}()
	return cancel, done
}

// startedPID waits for the stand-in to write the pid of what it started. The pid is only ever
// looked up, never signalled: a test must not be able to kill anything it did not start.
func startedPID(t *testing.T, file string) int {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, err := os.ReadFile(file)
		if pid, parsed := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && parsed == nil {
			return pid
		}
	}
	t.Fatal("the stand-in never started its child")
	return 0
}

// running treats a zombie as finished: it has stopped, and only its parent's wait is outstanding.
// A ps that cannot run fails the test, because then nothing was observed.
func running(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		t.Fatalf("ps did not run: %v", err)
	}
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func await(t *testing.T, result <-chan error, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(limit):
		t.Fatalf("git still held the caller after %v", limit)
		return nil
	}
}

// recordSignals replaces the kill for one test, so stopGroup can be given any pid safely.
func recordSignals(t *testing.T, result error) *[]int {
	t.Helper()
	var sent []int
	real := signalGroup
	signalGroup = func(pgid int) error {
		sent = append(sent, pgid)
		return result
	}
	t.Cleanup(func() { signalGroup = real })
	return &sent
}

func TestADeadlineStopsWhatGitStarted(t *testing.T) {
	pid := filepath.Join(t.TempDir(), "pid")
	fakeGit(t, "sleep 30 &\necho $! > '"+pid+"'\nwait\n")
	stop, result := startStatus(t)
	child := startedPID(t, pid)
	if !running(t, child) {
		t.Fatal("the stand-in's child was not running before the deadline")
	}
	stop()
	if err := await(t, result, 5*time.Second); err == nil {
		t.Fatal("a stopped git reported success")
	}
	for deadline := time.Now().Add(5 * time.Second); running(t, child); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("git's child outlived the deadline")
		}
	}
}

// os/exec can call Cancel just after git has exited. With nothing left in git's group, that
// reports the command as done and is not an error.
func TestStoppingGitAfterItExitedIsNotAnError(t *testing.T) {
	fakeGit(t, "exit 0\n")
	cmd := gitCommand(context.Background(), t.TempDir(), "version")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stopping a finished git: %v", err)
	}
}

// The kill is replaced here, so the refused pids are never passed to the real one.
func TestOnlyAStartedGitsGroupIsSignalled(t *testing.T) {
	sent := recordSignals(t, nil)
	for _, pid := range []int{-1, 0, 1} {
		if err := stopGroup(pid); err == nil || errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("pid %d was not refused: %v", pid, err)
		}
	}
	if len(*sent) != 0 {
		t.Fatalf("signalled %v for pids that are no git's", *sent)
	}
	if err := stopGroup(4242); err != nil || len(*sent) != 1 || (*sent)[0] != 4242 {
		t.Fatalf("%v, signalled %v", err, *sent)
	}
}

func TestAGroupThatIsGoneIsDoneAndAnyOtherFailureIsReported(t *testing.T) {
	recordSignals(t, syscall.ESRCH)
	if err := stopGroup(4242); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("a group that no longer exists: %v", err)
	}
	recordSignals(t, syscall.EPERM)
	if err := stopGroup(4242); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("a refused signal: %v", err)
	}
}
