//go:build darwin

package proc

import (
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// kern.procargs2 holds the argument count, the executable path and its padding, the arguments and
// then the environment. Only the counted arguments are read, and a buffer cut short gives none.
func TestProcargsArgsReadsTheLayout(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want []string
	}{
		"padded":     {"\x02\x00\x00\x00/opt/agy\x00\x00\x00\x00agy\x00-p\x00PATH=/bin\x00", []string{"agy", "-p"}},
		"one":        {"\x01\x00\x00\x00/bin/sh\x00sh\x00", []string{"sh"}},
		"empty":      {"\x03\x00\x00\x00/opt/agy\x00agy\x00-p\x00\x00", []string{"agy", "-p", ""}},
		"cut":        {"\x02\x00\x00\x00/bin/sh\x00sh\x00", nil},
		"unended":    {"\x01\x00\x00\x00/bin/sh\x00sh", nil},
		"no count":   {"\x00\x00\x00\x00/bin/sh\x00", nil},
		"past data":  {"\xff\x00\x00\x00/bin/sh\x00sh\x00", nil},
		"cut path":   {"\x01\x00\x00\x00/bin", nil},
		"too short":  {"\x01\x00", nil},
		"no content": {"", nil},
	} {
		got, err := procargsArgs([]byte(tc.raw))
		if tc.want == nil {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: %q %v", name, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}

// The kernel refuses another user's arguments, as launchd's are.
func TestArgsOSRefusesAnotherUsersProcess(t *testing.T) {
	if _, err := argsOS(1); err == nil {
		t.Fatal("read launchd's arguments")
	}
}

// macOS gives no cheap view of another process's input, so the check is whether the process has a
// controlling terminal. For this test it matches an independent open of /dev/tty, launchd has
// none, and the kernel's e_tdev decides it.
func TestReadsTerminalOSReadsTheProcessesControllingTerminal(t *testing.T) {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err == nil {
		_ = f.Close()
	}
	if got, readErr := readsTerminalOS(os.Getpid()); readErr != nil || got != (err == nil) {
		t.Fatalf("this process: %v %v, /dev/tty %v", got, readErr, err)
	}
	if got, err := readsTerminalOS(1); err != nil || got {
		t.Fatalf("launchd: %v %v", got, err)
	}
	defer func(read func(int) (*unix.KinfoProc, error)) { kinfoProc = read }(kinfoProc)
	for name, tc := range map[string]struct {
		k    *unix.KinfoProc
		err  error
		want bool
		fail error
	}{
		"no terminal": {k: &unix.KinfoProc{Eproc: unix.Eproc{Tdev: noDevice}}},
		"a terminal":  {k: &unix.KinfoProc{Eproc: unix.Eproc{Tdev: 0x10000003}}, want: true},
		"refused":     {err: syscall.EPERM, fail: syscall.EPERM},
		"no process":  {fail: ErrNotFound},
	} {
		kinfoProc = func(int) (*unix.KinfoProc, error) { return tc.k, tc.err }
		if got, err := readsTerminalOS(7); got != tc.want || !errors.Is(err, tc.fail) {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
}
