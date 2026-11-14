//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Seams that tests replace, to reach failures that a same-user process does not otherwise meet.
var (
	openProcess         = windows.OpenProcess
	newAttributeList    = windows.NewProcThreadAttributeList
	updateAttributeList = (*windows.ProcThreadAttributeListContainer).Update
	createProcess       = windows.CreateProcess
	desktopInfo         = readDesktopInfo
)

// errDesktop refuses a start whose desktop cannot be placed without guessing a window station.
var errDesktop = errors.New("desktop does not name its window station")

// startDetachedOS creates the process itself: os/exec cannot choose its desktop. It has no
// standard handles and inherits none, so the parent is opened only to be named as the parent.
func startDetachedOS(exe string, args, env []string, pid int, startMS int64) (Detached, error) {
	desktop, err := targetDesktop(desktopInfo())
	if err != nil {
		return Detached{}, err
	}
	app, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return Detached{}, err
	}
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return Detached{}, err
	}
	block, err := environmentBlock(env)
	if err != nil {
		return Detached{}, err
	}
	parent, err := openParent(pid, startMS)
	if err != nil {
		return Detached{}, err
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	attrs, err := newAttributeList(1)
	if err != nil {
		return Detached{}, err
	}
	defer attrs.Delete()
	if err := updateAttributeList(attrs, windows.PROC_THREAD_ATTRIBUTE_PARENT_PROCESS, unsafe.Pointer(&parent), unsafe.Sizeof(parent)); err != nil {
		return Detached{}, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Null standard handles, rather than values copied from this process that mean nothing there.
	si.Flags = windows.STARTF_USESTDHANDLES
	if desktop != "" {
		name := utf16.Encode([]rune(desktop + "\x00"))
		si.Desktop = &name[0]
	}
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS |
		windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	var pi windows.ProcessInformation
	if err := createProcess(app, cmdLine, nil, nil, false, flags, &block[0], nil, &si.StartupInfo, &pi); err != nil {
		return Detached{}, err
	}
	_ = windows.CloseHandle(pi.Thread)
	process := pi.Process
	return Detached{
		PID:     int(pi.ProcessId),
		Release: func() { _ = windows.CloseHandle(process) },
		Kill: func() {
			_ = windows.TerminateProcess(process, 1)
			_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
			_ = windows.CloseHandle(process)
		},
	}, nil
}

// readDesktopInfo is this process's desktop as its process parameters record it,
// "<window station>\<desktop>". Reading it does not connect to a window station.
func readDesktopInfo() string {
	return windows.RtlGetCurrentPeb().ProcessParameters.DesktopInfo.String()
}

// targetDesktop names the desktop for a process started from one whose desktop is info. The
// empty result means the process inherits the desktop as it is: an empty info, and the Default
// desktop. Another desktop, such as the one agy runs a command on, ends with that command (ADR
// 028), so the process goes to the Default desktop of the same window station, where agy runs. A
// desktop named without its window station, or a value that does not parse, is refused: a window
// station is never guessed.
func targetDesktop(info string) (string, error) {
	if info == "" || strings.EqualFold(info, "Default") {
		return "", nil
	}
	station, desktop, found := strings.Cut(info, `\`)
	if !found {
		// A desktop named alone is in the window station the process inherits, which agy does
		// for its command desktops (measured on agy 1.5.8). Default named alone is in the same one.
		if strings.ContainsRune(info, 0) {
			return "", fmt.Errorf("%w: %q", errDesktop, info)
		}
		return "Default", nil
	}
	if station == "" || desktop == "" || strings.ContainsAny(desktop, "\\\x00") || strings.ContainsRune(station, 0) {
		return "", fmt.Errorf("%w: %q", errDesktop, info)
	}
	if strings.EqualFold(desktop, "Default") {
		return "", nil
	}
	return station + `\Default`, nil
}

// openParent opens (pid, startMS) only to name it as the parent of a new process. The identity is
// checked on that same handle, so a reused pid cannot become the parent.
func openParent(pid int, startMS int64) (windows.Handle, error) {
	h, err := openProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("open the parent process: %w", err)
	}
	var code uint32
	var created, exit, kernel, user windows.Filetime
	if windows.GetExitCodeProcess(h, &code) != nil || code != stillActive ||
		windows.GetProcessTimes(h, &created, &exit, &kernel, &user) != nil || created.Nanoseconds()/1e6 != startMS {
		_ = windows.CloseHandle(h)
		return 0, ErrNotFound
	}
	return h, nil
}

// environmentBlock is env as a Unicode environment block sorted by name. Names compare without
// regard to case and a later entry replaces an earlier one, including drive-directory names such
// as =C:. SYSTEMROOT is added when missing, because Windows networking requires it. Invalid entries
// are refused so an empty entry cannot terminate the block before later entries.
func environmentBlock(env []string) ([]uint16, error) {
	values := map[string]string{}
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return nil, errors.New("environment entry contains NUL")
		}
		name, _, found := strings.Cut(kv, "=")
		if name == "" && found {
			name, _, found = strings.Cut(kv[1:], "=")
			name = "=" + name
		}
		if !found || name == "=" {
			return nil, errors.New("environment entry has no name or value separator")
		}
		key := strings.ToUpper(name)
		values[key] = kv
	}
	if _, ok := values["SYSTEMROOT"]; !ok {
		values["SYSTEMROOT"] = "SYSTEMROOT=" + os.Getenv("SYSTEMROOT")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var block []uint16
	for _, key := range keys {
		block = append(block, utf16.Encode([]rune(values[key]))...)
		block = append(block, 0)
	}
	return append(block, 0), nil
}

func noConsoleOS(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
}

func terminateOS(pid int, startMS int64) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ErrNotFound
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil || created.Nanoseconds()/1e6 != startMS {
		return ErrNotFound
	}
	return windows.TerminateProcess(h, 1)
}
