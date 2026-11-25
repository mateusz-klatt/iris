package hubclient

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

type configFileID struct {
	vol   uint32
	idxHi uint32
	idxLo uint32
}

func sameConfigFileID(a, b configFileID) bool {
	return a.vol == b.vol && a.idxHi == b.idxHi && a.idxLo == b.idxLo
}

var (
	winConfigCreateFile   = windows.CreateFile
	winConfigFileInfo     = windows.GetFileInformationByHandle
	winConfigFileType     = windows.GetFileType
	winConfigLockFileEx   = windows.LockFileEx
	winConfigUnlockFileEx = windows.UnlockFileEx
)

func verifyWindowsRegularHandle(h windows.Handle) (configFileID, error) {
	var bhfi windows.ByHandleFileInformation
	if err := winConfigFileInfo(h, &bhfi); err != nil {
		return configFileID{}, err
	}
	const nonRegular = windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT
	ft, err := winConfigFileType(h)
	if err != nil || ft != windows.FILE_TYPE_DISK || bhfi.FileAttributes&nonRegular != 0 {
		return configFileID{}, errors.New("client config is not a bounded regular file")
	}
	return configFileID{
		vol:   bhfi.VolumeSerialNumber,
		idxHi: bhfi.FileIndexHigh,
		idxLo: bhfi.FileIndexLow,
	}, nil
}

func statRegularConfig(path string) (os.FileInfo, configFileID, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, configFileID{}, err
	}
	if !info.Mode().IsRegular() {
		return info, configFileID{}, nil
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, configFileID{}, err
	}
	h, err := winConfigCreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, configFileID{}, err
	}
	id, verr := verifyWindowsRegularHandle(h)
	_ = windows.CloseHandle(h)
	if verr != nil {
		return nil, configFileID{}, verr
	}
	return info, id, nil
}

func openRegularConfig(path string, expected configFileID) (*os.File, configFileID, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, configFileID{}, err
	}
	h, err := winConfigCreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, configFileID{}, err
	}
	actual, err := verifyWindowsRegularHandle(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, configFileID{}, err
	}
	if !sameConfigFileID(expected, actual) {
		_ = windows.CloseHandle(h)
		return nil, configFileID{}, errors.New("client config replaced concurrently")
	}
	return os.NewFile(uintptr(h), path), actual, nil
}

func lockConfigFile(f *os.File) error {
	var ov windows.Overlapped
	return winConfigLockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ov)
}

func unlockConfigFile(f *os.File) {
	var ov windows.Overlapped
	_ = winConfigUnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ov)
}
