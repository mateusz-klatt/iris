//go:build windows

package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

const antigravityRenamePause = sessionFileRenamePause
const antigravityRenameAttempts = sessionFileRenameAttempts

type antigravityRenameInfo = sessionRenameInfo

var renameAntigravityStateFile = replaceSessionFile

func replaceAntigravityState(oldpath, newpath string, posix, classic func(string, string) error, pause func(time.Duration)) error {
	return replaceSessionFileWithRetry(oldpath, newpath, posix, classic, pause)
}

func posixRenameUnsupported(err error) bool {
	return sessionPOSIXRenameUnsupported(err)
}

func renameAntigravityStatePOSIX(oldpath, newpath string) error {
	return renameSessionFilePOSIX(oldpath, newpath)
}

func retryAntigravityStateRename(rename func() error, pause func(time.Duration)) error {
	return retrySessionFileRename(rename, pause)
}

func retryAntigravityStateRenameUntil(rename func() error, pause func(time.Duration), now func() time.Time) error {
	return retrySessionFileRenameUntil(rename, pause, now)
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
