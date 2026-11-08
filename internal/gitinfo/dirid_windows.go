package gitinfo

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// directoryID names a directory by an id every path to it shares: a junction, a mapped or subst
// drive, or a path spelt in another case gives the same directory another string, and a hub
// keyed on the path alone made two worktrees of one (D6). It is "" when the directory cannot be
// opened or no id can be read.
func directoryID(path string) string {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	// FILE_FLAG_BACKUP_SEMANTICS is what lets CreateFile open a directory.
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	for _, read := range idReaders {
		if id := read(h); id != "" {
			return id
		}
	}
	return ""
}

// idReaders are tried in order. The 128-bit file id is unique on every file system that has
// one, ReFS and Dev Drives included, where the 64-bit index is not guaranteed to be. A file
// system without it, such as FAT, falls back to the volume serial and the 64-bit index.
var idReaders = []func(windows.Handle) string{fileID128, fileID64}

// fileIDInfo is FILE_ID_INFO, which x/sys does not define: a 64-bit volume serial number and a
// 128-bit file id.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// fileID128 is the volume serial and the 128-bit file id, written "v128:<serial>:<id>" so it
// can never equal an id of the 64-bit form.
func fileID128(h windows.Handle) string {
	var info fileIDInfo
	if windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))) != nil {
		return ""
	}
	return fmt.Sprintf("v128:%x:%x", info.VolumeSerialNumber, info.FileID)
}

// fileID64 is the volume serial and the 64-bit file index.
func fileID64(h windows.Handle) string {
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil {
		return ""
	}
	return fmt.Sprintf("%x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
}
