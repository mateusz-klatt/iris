//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsStdinChild       = "IRIS_PROC_WINDOWS_STDIN_CHILD"
	windowsStdinChildReport = "IRIS_PROC_WINDOWS_STDIN_CHILD_REPORT"
)

func TestWindowsStdinChild(t *testing.T) {
	if os.Getenv(windowsStdinChild) != "1" {
		return
	}
	if report := os.Getenv(windowsStdinChildReport); report != "" {
		name, _ := windows.UTF16PtrFromString("CONIN$")
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			err = windows.SetStdHandle(windows.STD_INPUT_HANDLE, h)
		}
		if err == nil {
			var mode uint32
			err = windows.GetConsoleMode(h, &mode)
		}
		status := []byte("ready")
		if err == nil {
			defer func() { _ = windows.CloseHandle(h) }()
		} else {
			status = []byte(err.Error())
		}
		_ = os.WriteFile(report, status, 0o600)
	}
	time.Sleep(time.Minute)
}

// A pipe and the NUL device are not console input. In particular, NUL is a character device but
// has no console mode, so checking only GetFileType would count it as a terminal.
func TestReadsTerminalOSRejectsPipeAndNUL(t *testing.T) {
	for _, name := range []string{"pipe", "NUL"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsStdinChild$")
			cmd.Env = append(os.Environ(), windowsStdinChild+"=1")
			switch name {
			case "pipe":
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = stdin.Close() }()
			case "NUL":
				stdin, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = stdin.Close() }()
				cmd.Stdin = stdin
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}()
			if got, err := readsTerminalOS(cmd.Process.Pid); err != nil || got {
				t.Fatalf("child standard input %s: terminal %v, error %v", name, got, err)
			}
		})
	}
}

// Failures to inspect or duplicate the target's handle make its input unreadable. The second
// memory read is the StandardInput field itself; the first reads its ProcessParameters pointer.
func TestReadsTerminalOSReportsInspectionFailures(t *testing.T) {
	query := queryBasicInformation
	read := readProcessMemory
	duplicate := duplicateHandle
	defer func() {
		queryBasicInformation = query
		readProcessMemory = read
		duplicateHandle = duplicate
	}()
	denied := windows.ERROR_ACCESS_DENIED
	self := os.Getpid()

	queryBasicInformation = func(windows.Handle, *windows.PROCESS_BASIC_INFORMATION) error { return denied }
	if _, err := readsTerminalOS(self); !errors.Is(err, denied) {
		t.Fatalf("basic information query: %v", err)
	}
	queryBasicInformation = query

	readProcessMemory = func(windows.Handle, uintptr, *byte, uintptr, *uintptr) error { return denied }
	if _, err := readsTerminalOS(self); !errors.Is(err, denied) {
		t.Fatalf("ProcessParameters read: %v", err)
	}
	calls := 0
	readProcessMemory = func(process windows.Handle, address uintptr, buffer *byte, size uintptr, readCount *uintptr) error {
		calls++
		if calls == 2 {
			return denied
		}
		return read(process, address, buffer, size, readCount)
	}
	if _, err := readsTerminalOS(self); !errors.Is(err, denied) {
		t.Fatalf("StandardInput read: %v", err)
	}
	readProcessMemory = read

	duplicateHandle = func(windows.Handle, windows.Handle, windows.Handle, *windows.Handle, uint32, bool, uint32) error {
		return denied
	}
	if _, err := readsTerminalOS(self); !errors.Is(err, denied) {
		t.Fatalf("DuplicateHandle: %v", err)
	}
}

// Both standard-input sentinel values mean that the process has no input handle to duplicate.
func TestReadsTerminalOSRejectsNullAndInvalidHandles(t *testing.T) {
	read := readProcessMemory
	defer func() { readProcessMemory = read }()
	for _, input := range []windows.Handle{0, windows.InvalidHandle} {
		calls := 0
		readProcessMemory = func(process windows.Handle, address uintptr, buffer *byte, size uintptr, readCount *uintptr) error {
			calls++
			if calls == 2 {
				*(*windows.Handle)(unsafe.Pointer(buffer)) = input
				return nil
			}
			return read(process, address, buffer, size, readCount)
		}
		if got, err := readsTerminalOS(os.Getpid()); err != nil || got {
			t.Fatalf("standard input handle %#x: terminal %v, error %v", input, got, err)
		}
	}
}

// CREATE_NEW_CONSOLE gives the child its own console; the helper sets its standard input to that
// console's CONIN$ and verifies its console mode before reporting ready. This test process does
// not share that console, so GetConsoleMode refuses the duplicate. The shared-console positive
// branch is therefore supplied by a seam, with the duplicate independently checked as a real
// character-device handle; NUL above proves that this alone is not enough in production.
func TestReadsTerminalOSAcceptsAConsoleChild(t *testing.T) {
	t.Setenv(windowsStdinChild, "1")
	report := filepath.Join(t.TempDir(), "console-ready")
	t.Setenv(windowsStdinChildReport, report)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	application, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		t.Fatal(err)
	}
	commandLine, err := windows.UTF16PtrFromString(`"` + exe + `" -test.run=^TestWindowsStdinChild$`)
	if err != nil {
		t.Fatal(err)
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var created windows.ProcessInformation
	if err := windows.CreateProcess(application, commandLine, nil, nil, false, windows.CREATE_NEW_CONSOLE, nil, nil, &startup, &created); err != nil {
		t.Fatal(err)
	}
	_ = windows.CloseHandle(created.Thread)
	defer func() {
		_ = windows.TerminateProcess(created.Process, 1)
		_, _ = windows.WaitForSingleObject(created.Process, 5000)
		_ = windows.CloseHandle(created.Process)
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		status, err := os.ReadFile(report)
		if err == nil {
			if string(status) != "ready" {
				t.Fatalf("child console setup: %s", status)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not initialize its console")
		}
	}
	fileType := getFileType
	defer func() { getFileType = fileType }()
	var duplicatedKind uint32
	var duplicatedTypeErr error
	getFileType = func(h windows.Handle) (uint32, error) {
		duplicatedKind, duplicatedTypeErr = windows.GetFileType(h)
		return duplicatedKind, duplicatedTypeErr
	}
	if got, err := readsTerminalOS(int(created.ProcessId)); err != nil || got {
		t.Fatalf("console not shared with this test process: terminal %v, error %v", got, err)
	}
	if duplicatedTypeErr != nil || duplicatedKind != windows.FILE_TYPE_CHAR {
		t.Fatalf("duplicated child console handle: type %#x, error %v", duplicatedKind, duplicatedTypeErr)
	}

	consoleMode := getConsoleMode
	defer func() { getConsoleMode = consoleMode }()
	getConsoleMode = func(windows.Handle, *uint32) error { return nil }
	if got, err := readsTerminalOS(int(created.ProcessId)); err != nil || !got {
		t.Fatalf("child console with a shared-console result: terminal %v, error %v", got, err)
	}
}
