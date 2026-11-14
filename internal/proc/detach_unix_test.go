//go:build linux || darwin

package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStartDetachedAndTerminateUnix(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup failed")
	}
	env := append(os.Environ(), "IRIS_PROC_UNIX_SLEEP=1")
	d, err := StartDetached(os.Args[0], []string{"-test.run=^TestUnixSleepHelper$"}, env, self.PID, self.StartMS)
	if err != nil {
		t.Fatalf("StartDetached(self) = %v", err)
	}
	t.Cleanup(d.Kill)
	snap, ok := Lookup(d.PID)
	if !ok {
		t.Fatal("child lookup failed")
	}
	// The child leads a session of its own, apart from this test's.
	if sid, err := unix.Getsid(d.PID); err != nil || sid != d.PID {
		t.Fatalf("child session = %d, %v; want its own, %d", sid, err, d.PID)
	}
	if err := Terminate(snap.PID, snap.StartMS); err != nil {
		t.Fatalf("Terminate(child) = %v", err)
	}

	// Release forgets the process and leaves it running.
	d, err = StartDetached(os.Args[0], []string{"-test.run=^TestUnixSleepHelper$"}, env, self.PID, self.StartMS)
	if err != nil {
		t.Fatal(err)
	}
	snap, ok = Lookup(d.PID)
	d.Release()
	if !ok {
		t.Fatal("released child lookup failed")
	}
	t.Cleanup(func() { _ = Terminate(snap.PID, snap.StartMS) })
	if _, still := Lookup(d.PID); !still {
		t.Fatal("Release stopped the process")
	}
	_ = Terminate(snap.PID, snap.StartMS)

	if err := terminateSignal(123, func(int, syscall.Signal) error { return syscall.ESRCH }); err != nil {
		t.Fatalf("ESRCH should be ignored: %v", err)
	}
	if err := terminateSignal(123, func(int, syscall.Signal) error { return syscall.EPERM }); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("EPERM should propagate: %v", err)
	}
}

func TestStartDetachedRefusesBeforeAnythingStartsUnix(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup failed")
	}
	if _, err := StartDetached(os.Args[0], nil, nil, self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartDetached(another start time) = %v", err)
	}
	if _, err := StartDetached(os.Args[0], nil, nil, 0, self.StartMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartDetached(pid 0) = %v", err)
	}
	if _, err := StartDetached(filepath.Join(t.TempDir(), "missing"), nil, nil, self.PID, self.StartMS); err == nil {
		t.Fatal("started a program that does not exist")
	}
	orig := openDevNull
	t.Cleanup(func() { openDevNull = orig })
	openDevNull = func() (*os.File, error) { return nil, errors.New("no null device") }
	if _, err := StartDetached(os.Args[0], nil, nil, self.PID, self.StartMS); err == nil {
		t.Fatal("started without a null device for the standard streams")
	}
}

func TestNoConsoleLeavesAUnixCommandAsItIs(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	NoConsole(cmd)
	if cmd.SysProcAttr != nil {
		t.Fatalf("NoConsole changed a Unix command: %+v", cmd.SysProcAttr)
	}
}

func TestUnixSleepHelper(t *testing.T) {
	if os.Getenv("IRIS_PROC_UNIX_SLEEP") == "1" {
		select {}
	}
}
