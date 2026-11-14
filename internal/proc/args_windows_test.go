//go:build windows

package proc

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestArgsOfAProcessThatIsGone(t *testing.T) {
	if _, err := argsOS(0x7FFFFFF0); err == nil {
		t.Fatal("read the command line of no process")
	}
}

// A query that fails is its error, and a process with an empty command line has no arguments.
func TestArgsReportsAFailedOrEmptyCommandLine(t *testing.T) {
	defer func(q func(windows.Handle, []byte) error) { queryCommandLine = q }(queryCommandLine)
	queryCommandLine = func(windows.Handle, []byte) error { return windows.ERROR_ACCESS_DENIED }
	if _, err := argsOS(os.Getpid()); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("a refused query: %v", err)
	}
	queryCommandLine = func(_ windows.Handle, buf []byte) error {
		clear(buf)
		return nil
	}
	if _, err := argsOS(os.Getpid()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty command line: %v", err)
	}
}

// Windows gives no cheap view of another process's input, so the check is whether this process
// has a console, which a hook shares with the client that started it. The answer matches an
// independent open of CONIN$, and a console that opens or not decides it.
func TestReadsTerminalOSOpensTheConsole(t *testing.T) {
	name, _ := windows.UTF16PtrFromString("CONIN$")
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err == nil {
		_ = windows.CloseHandle(h)
	}
	if got, readErr := readsTerminalOS(os.Getpid()); readErr != nil || got != (err == nil) {
		t.Fatalf("this process: %v %v, CONIN$ %v", got, readErr, err)
	}
	defer func(open func() (windows.Handle, error)) { openConsole = open }(openConsole)
	openConsole = func() (windows.Handle, error) { return windows.InvalidHandle, windows.ERROR_INVALID_HANDLE }
	if got, err := readsTerminalOS(0); err != nil || got {
		t.Fatalf("no console: %v %v", got, err)
	}
	openConsole = func() (windows.Handle, error) { return windows.CreateEvent(nil, 0, 0, nil) }
	if got, err := readsTerminalOS(0); err != nil || !got {
		t.Fatalf("a console: %v %v", got, err)
	}
}
