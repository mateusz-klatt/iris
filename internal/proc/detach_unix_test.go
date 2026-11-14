//go:build linux || darwin

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestDetachAndTerminateUnix(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup failed")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnixSleepHelper$")
	cmd.Env = append(os.Environ(), "IRIS_PROC_UNIX_SLEEP=1")
	release, err := DetachUnder(cmd, self.PID, self.StartMS)
	if err != nil {
		t.Fatalf("DetachUnder(self) = %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("DetachUnder SysProcAttr = %+v", cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	release()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	snap, ok := Lookup(cmd.Process.Pid)
	if !ok {
		t.Fatal("child lookup failed")
	}
	if err := Terminate(snap.PID, snap.StartMS); err != nil {
		t.Fatalf("Terminate(child) = %v", err)
	}
	_ = cmd.Wait()
	if err := terminateSignal(123, func(int, syscall.Signal) error { return syscall.ESRCH }); err != nil {
		t.Fatalf("ESRCH should be ignored: %v", err)
	}
	if err := terminateSignal(123, func(int, syscall.Signal) error { return syscall.EPERM }); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("EPERM should propagate: %v", err)
	}
}

func TestDetachUnderRefusesAParentThatNoLongerMatches(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup failed")
	}
	cmd := exec.Command(os.Args[0])
	if _, err := DetachUnder(cmd, self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DetachUnder(another start time) = %v", err)
	}
	if _, err := DetachUnder(cmd, 0, self.StartMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DetachUnder(pid 0) = %v", err)
	}
	if cmd.SysProcAttr != nil {
		t.Fatalf("a refused parent still configured the command: %+v", cmd.SysProcAttr)
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
