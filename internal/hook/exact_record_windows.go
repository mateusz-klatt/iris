//go:build windows

package hook

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"golang.org/x/sys/windows"
)

func sessionPathAbsent(path string, err error) bool {
	if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return true
	}
	// On Windows, ERROR_PATH_NOT_FOUND can mean either a missing parent directory or an
	// existing non-directory ancestor (Unix ENOTDIR); only missing directories count as absent.
	for cur := filepath.Dir(filepath.Clean(path)); ; {
		info, lerr := lstatSessionFile(cur)
		if lerr == nil {
			return info.IsDir()
		}
		if !errors.Is(lerr, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return true
		}
		cur = parent
	}
}

type exactRecordID struct {
	vol   uint32
	idxHi uint32
	idxLo uint32
}

var (
	winExactRecordCreateFile = windows.CreateFile
	winExactRecordFileInfo   = windows.GetFileInformationByHandle
	winExactRecordFileType   = windows.GetFileType
)

func verifyWindowsExactRecordHandle(h windows.Handle) (exactRecordID, error) {
	var bhfi windows.ByHandleFileInformation
	if err := winExactRecordFileInfo(h, &bhfi); err != nil {
		return exactRecordID{}, err
	}
	const nonRegular = windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT
	ft, err := winExactRecordFileType(h)
	if err != nil || ft != windows.FILE_TYPE_DISK || bhfi.FileAttributes&nonRegular != 0 {
		return exactRecordID{}, sessionproof.ErrInvalidRecord
	}
	return exactRecordID{
		vol:   bhfi.VolumeSerialNumber,
		idxHi: bhfi.FileIndexHigh,
		idxLo: bhfi.FileIndexLow,
	}, nil
}

func statExactRecordFile(path string) (os.FileInfo, exactRecordID, error) {
	p, err := windows.UTF16PtrFromString(extendedWindowsPath(path))
	if err != nil {
		return nil, exactRecordID{}, err
	}
	info, err := lstatSessionFile(path)
	if err != nil {
		return nil, exactRecordID{}, err
	}
	if !info.Mode().IsRegular() {
		return info, exactRecordID{}, nil
	}
	h, err := winExactRecordCreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, exactRecordID{}, err
	}
	id, verr := verifyWindowsExactRecordHandle(h)
	_ = windows.CloseHandle(h)
	if verr != nil {
		return nil, exactRecordID{}, verr
	}
	return info, id, nil
}

func openExactRecordFile(path string, expected exactRecordID) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(extendedWindowsPath(path))
	if err != nil {
		return nil, err
	}
	h, err := winExactRecordCreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	actual, err := verifyWindowsExactRecordHandle(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if actual != expected {
		_ = windows.CloseHandle(h)
		return nil, sessionproof.ErrInvalidRecord
	}
	return os.NewFile(uintptr(h), path), nil
}
