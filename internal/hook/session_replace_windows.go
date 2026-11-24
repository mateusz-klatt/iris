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

// sessionRenameInfo is the Win32 FILE_RENAME_INFO passed to SetFileInformationByHandle with
// FileRenameInfoEx. Flags is the DWORD union member, FILE_RENAME_FLAG_REPLACE_IF_EXISTS |
// FILE_RENAME_FLAG_POSIX_SEMANTICS, which x/sys names without the FLAG_ infix.
type sessionRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       uint16
}

// replaceSessionFile gives a reader of the old file POSIX replacement semantics where Windows and
// the volume support them. The MoveFileEx fallback and every retry stay within hookLockBudget; that
// fallback can still fail while a reader holds the old file open.
func replaceSessionFile(oldpath, newpath string) error {
	return replaceSessionFileWithRetry(oldpath, newpath, renameSessionFilePOSIX, os.Rename, time.Sleep)
}

func replaceSessionFileWithRetry(oldpath, newpath string, posix, classic func(string, string) error, pause func(time.Duration)) error {
	return replaceSessionFileWithRetryUntil(oldpath, newpath, posix, classic, pause, time.Now)
}

func replaceSessionFileWithRetryUntil(oldpath, newpath string, posix, classic func(string, string) error, pause func(time.Duration), now func() time.Time) error {
	start := now()
	unsupported := false
	return retrySessionFileRenameAt(func() error {
		if !unsupported {
			err := posix(oldpath, newpath)
			if err == nil || !sessionPOSIXRenameUnsupported(err) {
				return err
			}
			unsupported = true
			if !now().Before(start.Add(hookLockBudget)) {
				return err
			}
		}
		return classic(oldpath, newpath)
	}, pause, now, start)
}

func sessionPOSIXRenameUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

func renameSessionFilePOSIX(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(extendedWindowsPath(oldpath))
	if err != nil {
		return err
	}
	to, err := windows.UTF16FromString(extendedWindowsPath(newpath))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
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

	var probe sessionRenameInfo
	size := int(unsafe.Offsetof(probe.FileName)) + len(to)*2
	words := make([]uint64, (size+7)/8)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), size)
	info := (*sessionRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(to) - 1) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&info.FileName)), len(to)), to)
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(size))
}

func retrySessionFileRename(rename func() error, pause func(time.Duration)) error {
	return retrySessionFileRenameUntil(rename, pause, time.Now)
}

func retrySessionFileRenameUntil(rename func() error, pause func(time.Duration), now func() time.Time) error {
	return retrySessionFileRenameAt(rename, pause, now, now())
}

func retrySessionFileRenameAt(rename func() error, pause func(time.Duration), now func() time.Time, start time.Time) error {
	err := os.ErrDeadlineExceeded
	for attempt := 0; ; attempt++ {
		if !now().Before(start.Add(hookLockBudget)) {
			return err
		}
		err = rename()
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrPermission) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		if attempt+1 == sessionFileRenameAttempts || !now().Before(start.Add(hookLockBudget)) {
			return err
		}
		pause(sessionFileRenamePause)
	}
}
