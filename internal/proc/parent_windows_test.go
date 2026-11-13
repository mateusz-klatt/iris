package proc

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestParentHelper(t *testing.T) {
	if os.Getenv("IRIS_PROC_TEST_CHILD") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func parentChild(t *testing.T) *exec.Cmd {
	t.Helper()
	t.Setenv("IRIS_PROC_TEST_CHILD", "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestParentHelper$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestWatchRefusesWhatItCannotOpen(t *testing.T) {
	if _, err := watch(0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := watch(0x7FFFFFF0); err == nil {
		t.Fatal("watched a process that does not exist")
	}
}

func TestWatchFollowsTheOriginalProcess(t *testing.T) {
	cmd := parentChild(t)
	w, err := watch(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if !w.Alive() {
		t.Fatal("running process reported dead")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if w.Alive() {
		t.Fatal("exited process reported alive")
	}
}

func TestWatchParentSeesThisTestRunner(t *testing.T) {
	w, err := WatchParent()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if !w.Alive() {
		t.Fatal("the parent of this test is running")
	}
}

func TestAliveIsFalseOnAClosedHandle(t *testing.T) {
	w, err := WatchParent()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if w.Alive() {
		t.Fatal("a failed wait counted as alive")
	}
}

func TestLookupOS(t *testing.T) {
	if _, ok := lookupOS(0); ok {
		t.Fatal("pid 0")
	}
	if _, ok := lookupOS(0x7FFFFFF0); ok {
		t.Fatal("no such process")
	}
	got, ok := lookupOS(os.Getpid())
	if !ok {
		t.Fatal("this process")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != os.Getpid() || got.PPID != os.Getppid() || got.StartMS <= 0 || !strings.EqualFold(got.Name, bin) {
		t.Fatalf("%+v, executable %s", got, bin)
	}
}

func TestImageNameOfABadHandleIsEmpty(t *testing.T) {
	if got := imageName(0); got != "" {
		t.Fatal(got)
	}
}

func TestTerminateWindows(t *testing.T) {
	if err := terminateOS(0x7FFFFFF0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pid = %v", err)
	}
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	if got, err := Executable(self.PID, self.StartMS); err != nil || !strings.EqualFold(got, self.Name) {
		t.Fatalf("Executable(self) = %q, %v (want %q)", got, err, self.Name)
	}
	if err := terminateOS(self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched startMS = %v", err)
	}
	child := parentChild(t)
	snap, ok := Lookup(child.Process.Pid)
	if !ok {
		t.Fatal("child lookup")
	}
	if err := Terminate(snap.PID, snap.StartMS); err != nil {
		t.Fatalf("Terminate(child) = %v", err)
	}
	_ = child.Wait()
}
