//go:build windows

package proc

import (
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
