//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The kernel's argument block separates arguments with NULs and ends with one, so an empty
// argument survives. A process that rewrote its title into one string gives that one string.
func TestSplitArgsKeepsEmptyArguments(t *testing.T) {
	for raw, want := range map[string][]string{
		"agy\x00-p\x00\x00":       {"agy", "-p", ""},
		"agy\x00--model\x00x\x00": {"agy", "--model", "x"},
		"a rewritten title":       {"a rewritten title"},
	} {
		got, err := splitArgs([]byte(raw))
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	if _, err := splitArgs(nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no arguments: %v", err)
	}
}

func TestArgsAndTerminalOfAProcessThatIsGone(t *testing.T) {
	if _, err := argsOS(0x7FFFFFF0); err == nil {
		t.Fatal("read the arguments of no process")
	}
	if _, err := readsTerminalOS(0x7FFFFFF0); err == nil {
		t.Fatal("read the input of no process")
	}
}

func TestTerminalPathNamesTerminalsOnly(t *testing.T) {
	for target, want := range map[string]bool{
		"/dev/pts/3":           true,
		"/dev/tty":             true,
		"/dev/tty1":            true,
		"/dev/ttyS0":           true,
		"/dev/console":         true,
		"/dev/null":            false,
		"pipe:[5078003]":       false,
		"socket:[12]":          false,
		"/tmp/prompt.txt":      false,
		"anon_inode:[eventfd]": false,
	} {
		if got := terminalPath(target); got != want {
			t.Fatalf("%s: %v", target, got)
		}
	}
}

// A process whose standard input is a pseudo-terminal reads from a terminal. This test's own input
// does exactly when an ioctl says it is a terminal.
func TestReadsTerminalOSReadsWhereStandardInputPoints(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close() }()
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pts.Close() }()
	cmd := exec.Command("sleep", "30")
	cmd.Stdin = pts
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if got, err := readsTerminalOS(cmd.Process.Pid); err != nil || !got {
		t.Fatalf("a child reading a pseudo-terminal: %v %v", got, err)
	}
	_, notTerminal := unix.IoctlGetTermios(0, unix.TCGETS)
	if got, err := readsTerminalOS(os.Getpid()); err != nil || got != (notTerminal == nil) {
		t.Fatalf("this test's input: %v %v, ioctl %v", got, err, notTerminal)
	}
}
