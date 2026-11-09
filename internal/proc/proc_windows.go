//go:build windows

package proc

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

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
