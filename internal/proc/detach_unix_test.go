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
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnixSleepHelper$")
	cmd.Env = append(os.Environ(), "IRIS_PROC_UNIX_SLEEP=1")
	Detach(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("Detach SysProcAttr = %+v", cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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

func TestUnixSleepHelper(t *testing.T) {
	if os.Getenv("IRIS_PROC_UNIX_SLEEP") == "1" {
		select {}
	}
}
