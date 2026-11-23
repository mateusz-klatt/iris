//go:build windows

package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const antigravityRenamePause = time.Millisecond

const antigravityRenameAttempts = int(hookLockBudget / antigravityRenamePause)

// antigravityRenameInfo is the Win32 FILE_RENAME_INFO passed to
// SetFileInformationByHandle with FileRenameInfoEx. Flags is that struct's
// DWORD union member, FILE_RENAME_FLAG_REPLACE_IF_EXISTS |
// FILE_RENAME_FLAG_POSIX_SEMANTICS, which x/sys names without the FLAG_ infix.
// The handle field is pointer-aligned, matching the Windows header.
type antigravityRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       uint16
}

// renameAntigravityStateFile replaces the file at newpath with oldpath.
// A reader that still has newpath open keeps that handle on the previous
// contents, and a later open sees the replacement. MoveFileEx cannot do this:
// it refuses while any handle is open, including one opened with
// FILE_SHARE_DELETE. FileRenameInfoEx with POSIX semantics can, on Windows 10
// version 1607 and later and on a volume that supports it. Where that call is
// rejected, the rename falls back to MoveFileEx and retries access-denied and
// sharing-violation until hookLockBudget has elapsed or the attempt cap is
// reached. The fallback still cannot replace a file a reader holds open.
// Tests set this to the error a replace returns after those retries are finished.
var renameAntigravityStateFile = func(oldpath, newpath string) error {
	return replaceAntigravityState(oldpath, newpath, renameAntigravityStatePOSIX, os.Rename, time.Sleep)
}

func replaceAntigravityState(oldpath, newpath string, posix, classic func(string, string) error, pause func(time.Duration)) error {
	unsupported := false
	return retryAntigravityStateRename(func() error {
		if !unsupported {
			err := posix(oldpath, newpath)
			if err == nil || !posixRenameUnsupported(err) {
				return err
			}
			unsupported = true
		}
		return classic(oldpath, newpath)
	}, pause)
}

// posixRenameUnsupported reports that this Windows or this volume rejected
// FileRenameInfoEx. Older Windows returns ERROR_INVALID_PARAMETER or
// ERROR_INVALID_FUNCTION. A volume without POSIX rename, such as FAT, returns
// ERROR_NOT_SUPPORTED.
func posixRenameUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

func renameAntigravityStatePOSIX(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16FromString(newpath)
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

	// FileNameLength is the name in bytes and excludes the trailing NUL.
	// UTF16FromString returns at least that NUL.
	var probe antigravityRenameInfo
	size := int(unsafe.Offsetof(probe.FileName)) + len(to)*2
	words := make([]uint64, (size+7)/8)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), size)
	info := (*antigravityRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(to) - 1) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&info.FileName)), len(to)), to)
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(size))
}

func retryAntigravityStateRename(rename func() error, pause func(time.Duration)) error {
	return retryAntigravityStateRenameUntil(rename, pause, time.Now)
}

// retryAntigravityStateRenameUntil retries access-denied and sharing-violation
// until hookLockBudget has elapsed or the attempt cap is reached, whichever
// comes first. now is the clock, so a test can advance it without sleeping.
// MoveFileEx still cannot replace a file a reader holds open once the budget ends.
func retryAntigravityStateRenameUntil(rename func() error, pause func(time.Duration), now func() time.Time) error {
	start := now()
	for attempt := 0; ; attempt++ {
		err := rename()
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrPermission) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		if !now().Before(start.Add(hookLockBudget)) || attempt+1 == antigravityRenameAttempts {
			return err
		}
		pause(antigravityRenamePause)
	}
}

func openAntigravityStateFile(path string) (*os.File, error) {
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func openRegularTranscript(path string) (*os.File, string, int64, error) {
	return openRegularFileWindows(path, windows.GetFileInformationByHandle, windows.GetFileType)
}

func openRegularFileWindows(
	path string,
	fileInfo func(windows.Handle, *windows.ByHandleFileInformation) error,
	fileType func(windows.Handle) (uint32, error),
) (*os.File, string, int64, error) {
	if !filepath.IsAbs(path) {
		return nil, "", 0, errors.New("transcript path is not absolute")
	}
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, "", 0, err
	}
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, "", 0, err
	}
	var bhfi windows.ByHandleFileInformation
	if err := fileInfo(h, &bhfi); err != nil {
		_ = windows.CloseHandle(h)
		return nil, "", 0, err
	}
	const nonRegular = windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT
	ft, ftErr := fileType(h)
	if ftErr != nil || ft != windows.FILE_TYPE_DISK || bhfi.FileAttributes&nonRegular != 0 {
		_ = windows.CloseHandle(h)
		return nil, "", 0, errors.New("transcript is not a regular file")
	}
	size := int64(bhfi.FileSizeHigh)<<32 | int64(bhfi.FileSizeLow)
	id := fmt.Sprintf("%08x:%08x%08x", bhfi.VolumeSerialNumber, bhfi.FileIndexHigh, bhfi.FileIndexLow)
	return os.NewFile(uintptr(h), path), id, size, nil
}
