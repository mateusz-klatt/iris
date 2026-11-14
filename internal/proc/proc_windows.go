//go:build windows

package proc

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE, the exit code Windows reports for a process that has not exited.
const stillActive = 259

func lookupOS(pid int) (Snapshot, bool) {
	if pid <= 0 {
		return Snapshot{}, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Snapshot{}, false
	}
	// This query-only handle owns no pending writes; closing it cannot affect the snapshot.
	defer func() { _ = windows.CloseHandle(h) }()
	// An exited process keeps its process object, start time included, for as long as anyone
	// holds a handle to it, so opening it does not prove it runs. Like a zombie on Linux, it is
	// not running. A process that itself exited with code 259 is indistinguishable from a running
	// one here; Windows documents that limit of the exit code.
	var code uint32
	if windows.GetExitCodeProcess(h, &code) == nil && code != stillActive {
		return Snapshot{}, false
	}

	var info windows.PROCESS_BASIC_INFORMATION
	var n uint32
	// Without a parent the walk stops here, which is the safe outcome of a failed query.
	_ = windows.NtQueryInformationProcess(h, int32(windows.ProcessBasicInformation), unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &n)
	var created, exit, kernel, user windows.Filetime
	_ = windows.GetProcessTimes(h, &created, &exit, &kernel, &user)
	return Snapshot{
		PID:     pid,
		PPID:    int(info.InheritedFromUniqueProcessId),
		Name:    imageName(h),
		StartMS: created.Nanoseconds() / 1e6,
	}, true
}

func imageName(h windows.Handle) string {
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil || size == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

func executableOS(_ int, s Snapshot) (string, error) {
	return s.Name, nil
}

// queryCommandLine is NtQueryInformationProcess for ProcessCommandLineInformation, which fills
// buf with a counted UTF-16 string and its text. Tests replace it.
var queryCommandLine = func(h windows.Handle, buf []byte) error {
	var n uint32
	return windows.NtQueryInformationProcess(h, int32(windows.ProcessCommandLineInformation), unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
}

// argsOS splits the process's command line by CommandLineToArgvW's rules. A command line holds at
// most 32767 UTF-16 code units, so the buffer fits it with its header.
func argsOS(pid int) ([]string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	// This query-only handle owns no pending writes; closing it cannot affect the result.
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]byte, 64<<10+int(unsafe.Sizeof(windows.NTUnicodeString{})))
	if err := queryCommandLine(h, buf); err != nil {
		return nil, err
	}
	return splitCommandLine((*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String())
}

// splitCommandLine splits a command line by CommandLineToArgvW's rules. A doubled quote is
// refused: runtimes disagree about it, Rust's (Grok's) among them, so the split could hide a flag
// (ADR 031).
func splitCommandLine(line string) ([]string, error) {
	if line == "" || strings.Contains(line, `""`) {
		return nil, ErrNotFound
	}
	return windows.DecomposeCommandLine(line)
}

// The process and handle operations are package-level seams for failures that tests cannot
// otherwise induce in a live child.
var (
	queryBasicInformation = func(h windows.Handle, info *windows.PROCESS_BASIC_INFORMATION) error {
		var n uint32
		return windows.NtQueryInformationProcess(h, int32(windows.ProcessBasicInformation), unsafe.Pointer(info), uint32(unsafe.Sizeof(*info)), &n)
	}
	readProcessMemory = windows.ReadProcessMemory
	duplicateHandle   = windows.DuplicateHandle
	getFileType       = windows.GetFileType
	getConsoleMode    = windows.GetConsoleMode
)

// readsTerminalOS reports whether pid's own standard input is a console input handle. A null or
// invalid handle is not a terminal. GetConsoleMode on a duplicated console handle may succeed only
// when the caller shares that console; the Windows control in ADR 031 measures whether a hook of an
// interactive agy does. Without a shared console the answer is false, so Iris does not ask for wake.
func readsTerminalOS(pid int) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ|windows.PROCESS_DUP_HANDLE, false, uint32(pid))
	if err != nil {
		return false, err
	}
	// The process handle and the duplicated input handle own no pending writes.
	defer func() { _ = windows.CloseHandle(process) }()

	var info windows.PROCESS_BASIC_INFORMATION
	if err := queryBasicInformation(process, &info); err != nil {
		return false, err
	}
	var parameters *windows.RTL_USER_PROCESS_PARAMETERS
	parametersAddress := uintptr(unsafe.Pointer(info.PebBaseAddress)) + unsafe.Offsetof(windows.PEB{}.ProcessParameters)
	if err := readProcessMemory(process, parametersAddress, (*byte)(unsafe.Pointer(&parameters)), unsafe.Sizeof(parameters), nil); err != nil {
		return false, err
	}
	var input windows.Handle
	inputAddress := uintptr(unsafe.Pointer(parameters)) + unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.StandardInput)
	if err := readProcessMemory(process, inputAddress, (*byte)(unsafe.Pointer(&input)), unsafe.Sizeof(input), nil); err != nil {
		return false, err
	}
	if input == 0 || input == windows.InvalidHandle {
		return false, nil
	}

	var duplicated windows.Handle
	if err := duplicateHandle(process, input, windows.CurrentProcess(), &duplicated, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(duplicated) }()
	kind, err := getFileType(duplicated)
	if err != nil || kind != windows.FILE_TYPE_CHAR {
		return false, nil
	}
	var mode uint32
	if err := getConsoleMode(duplicated, &mode); err != nil {
		return false, nil
	}
	return true, nil
}
