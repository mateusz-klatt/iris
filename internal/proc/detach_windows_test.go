//go:build windows

package proc

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// The child is created under the given process, not under the caller: it takes that process's
// job, which is how the Antigravity helper outlives the job agy runs its command in.
func TestDetachUnderStartsTheChildUnderTheGivenParent(t *testing.T) {
	parent := parentChild(t)
	snap, ok := Lookup(parent.Process.Pid)
	if !ok {
		t.Fatal("parent lookup")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestParentHelper$")
	release, err := DetachUnder(cmd, snap.PID, snap.StartMS)
	if err != nil {
		t.Fatalf("DetachUnder(live parent) = %v", err)
	}
	err = cmd.Start()
	release()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	child, ok := Lookup(cmd.Process.Pid)
	if !ok {
		t.Fatal("child lookup")
	}
	if child.PPID != snap.PID {
		t.Fatalf("child's parent = %d, want %d rather than this test (%d)", child.PPID, snap.PID, os.Getpid())
	}
}

// A refusal to open the parent fails the spawn before anything starts. A same-user process is
// not refused in practice, so the refusal is simulated.
func TestDetachUnderFailsWhenTheParentCannotBeOpened(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	orig := openProcess
	t.Cleanup(func() { openProcess = orig })
	openProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
	cmd := exec.Command(os.Args[0])
	if _, err := DetachUnder(cmd, self.PID, self.StartMS); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("DetachUnder(refused parent) = %v", err)
	}
	if cmd.SysProcAttr != nil {
		t.Fatalf("a refused parent still configured the command: %+v", cmd.SysProcAttr)
	}
}

// The identity is checked on the handle that names the parent: another start time, a pid that
// does not exist, and an exited process whose object a handle still keeps are all refused.
func TestDetachUnderRefusesAParentThatNoLongerMatches(t *testing.T) {
	self, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("self lookup")
	}
	cmd := exec.Command(os.Args[0])
	if _, err := DetachUnder(cmd, self.PID, self.StartMS+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DetachUnder(another start time) = %v", err)
	}
	if _, err := DetachUnder(cmd, 0x7FFFFFF0, 100); err == nil {
		t.Fatal("DetachUnder accepted a pid that does not exist")
	}

	exited := exec.Command(os.Args[0], "-test.run=^$")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(exited.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		t.Fatal(err)
	}
	if _, err := DetachUnder(cmd, exited.Process.Pid, created.Nanoseconds()/1e6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DetachUnder(exited parent) = %v", err)
	}
	_ = exited.Wait()
	if cmd.SysProcAttr != nil {
		t.Fatalf("a refused parent still configured the command: %+v", cmd.SysProcAttr)
	}
}

// A console program started without a console still runs, and its output still arrives through
// the pipe set on the command.
func TestNoConsoleRunsAConsoleProgramWithoutAConsole(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleProbeHelper$")
	cmd.Env = append(os.Environ(), "IRIS_PROC_CONSOLE_PROBE=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	NoConsole(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("console program without a console: %v", err)
	}
	if !strings.Contains(out.String(), "console=none") {
		t.Fatalf("probe output = %q", out.String())
	}
}

func TestConsoleProbeHelper(t *testing.T) {
	if os.Getenv("IRIS_PROC_CONSOLE_PROBE") != "1" {
		return
	}
	hwnd, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	state := "console=attached"
	if hwnd == 0 {
		state = "console=none"
	}
	_, _ = os.Stdout.WriteString(state + "\n")
	os.Exit(0)
}
