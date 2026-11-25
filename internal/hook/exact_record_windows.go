//go:build windows

package hook

import (
	"os"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"golang.org/x/sys/windows"
)

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
	info, err := lstatSessionFile(path)
	if err != nil {
		return nil, exactRecordID{}, err
	}
	if !info.Mode().IsRegular() {
		return info, exactRecordID{}, nil
	}
	p, err := windows.UTF16PtrFromString(extendedWindowsPath(path))
	if err != nil {
		return nil, exactRecordID{}, err
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
