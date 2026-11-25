//go:build windows

package hook

import (
	"errors"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const sessionFileRenamePause = time.Millisecond
const sessionFileRenameAttempts = int(hookLockBudget / sessionFileRenamePause)

var (
	winCreateFile               = windows.CreateFile
	winSetFileInformationHandle = windows.SetFileInformationByHandle
)

// sessionRenameInfo is the Win32 FILE_RENAME_INFO passed to SetFileInformationByHandle with
// FileRenameInfoEx. Flags is the DWORD union member, FILE_RENAME_FLAG_REPLACE_IF_EXISTS |
// FILE_RENAME_FLAG_POSIX_SEMANTICS, which x/sys names without the FLAG_ infix.
type sessionRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       uint16
}

// replaceSessionFileDeadline gives a reader of the old file POSIX replacement semantics where
// Windows and the volume support them. The MoveFileEx fallback and every retry stay within
// deadline; optional writes do not sleep and retry after a sharing violation.
func replaceSessionFileDeadline(oldpath, newpath string, deadline time.Time, required bool, now func() time.Time, pause func(time.Duration)) error {
	if now == nil {
		now = time.Now
	}
	if deadline.IsZero() {
		deadline = now().Add(hookLockBudget)
	}
	return replaceSessionFileWithOpsDeadline(oldpath, newpath, deadline, required, now, pause, renameSessionFilePOSIXDeadline, os.Rename)
}

func replaceSessionFileWithRetry(oldpath, newpath string, posix, classic func(string, string) error, pause func(time.Duration)) error {
	return replaceSessionFileWithRetryUntil(oldpath, newpath, posix, classic, pause, time.Now)
}

func replaceSessionFileWithRetryUntil(oldpath, newpath string, posix, classic func(string, string) error, pause func(time.Duration), now func() time.Time) error {
	deadline := now().Add(hookLockBudget)
	return replaceSessionFileWithOpsDeadline(oldpath, newpath, deadline, true, now, pause, func(o, n string, _ time.Time, _ func() time.Time) error {
		return posix(o, n)
	}, classic)
}

func replaceSessionFileWithOpsDeadline(
	oldpath, newpath string,
	deadline time.Time,
	required bool,
	now func() time.Time,
	pause func(time.Duration),
	posix func(string, string, time.Time, func() time.Time) error,
	classic func(string, string) error,
) error {
	unsupported := false
	return retrySessionFileRenameDeadline(func() error {
		if !unsupported {
			err := posix(oldpath, newpath, deadline, now)
			if err == nil || !sessionPOSIXRenameUnsupported(err) {
				return err
			}
			unsupported = true
			if deadlineExpired(now, deadline) {
				return err
			}
		}
		return classic(oldpath, newpath)
	}, deadline, required, now, pause)
}

func sessionPOSIXRenameUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

func renameSessionFilePOSIX(oldpath, newpath string) error {
	return renameSessionFilePOSIXDeadline(oldpath, newpath, time.Time{}, time.Now)
}

func renameSessionFilePOSIXDeadline(oldpath, newpath string, deadline time.Time, now func() time.Time) error {
	if deadlineExpired(now, deadline) {
		return os.ErrDeadlineExceeded
	}
	from, err := windows.UTF16PtrFromString(extendedWindowsPath(oldpath))
	if err != nil {
		return err
	}
	to, err := windows.UTF16FromString(extendedWindowsPath(newpath))
	if err != nil {
		return err
	}
	h, err := winCreateFile(
		from,
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if deadlineExpired(now, deadline) {
		return os.ErrDeadlineExceeded
	}

	var probe sessionRenameInfo
	size := int(unsafe.Offsetof(probe.FileName)) + len(to)*2
	words := make([]uint64, (size+7)/8)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), size)
	info := (*sessionRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(to) - 1) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&info.FileName)), len(to)), to)
	return winSetFileInformationHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(size))
}

func retrySessionFileRename(rename func() error, pause func(time.Duration)) error {
	return retrySessionFileRenameUntil(rename, pause, time.Now)
}

func retrySessionFileRenameUntil(rename func() error, pause func(time.Duration), now func() time.Time) error {
	return retrySessionFileRenameDeadline(rename, now().Add(hookLockBudget), true, now, pause)
}

func retrySessionFileRenameDeadline(rename func() error, deadline time.Time, required bool, now func() time.Time, pause func(time.Duration)) error {
	if pause == nil {
		pause = time.Sleep
	}
	err := os.ErrDeadlineExceeded
	for attempt := 0; ; attempt++ {
		if deadlineExpired(now, deadline) {
			return err
		}
		err = rename()
		if err == nil {
			return nil
		}
		if !required || (!errors.Is(err, os.ErrPermission) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION)) {
			return err
		}
		rem := deadline.Sub(now())
		if rem <= 0 {
			return err
		}
		if attempt+1 == sessionFileRenameAttempts {
			return err
		}
		pause(min(sessionFileRenamePause, rem))
	}
}
