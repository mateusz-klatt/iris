package gitinfo

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// directoryID names a directory by its volume serial number and 64-bit file index, which every
// path to it shares: a junction, a mapped or subst drive, or a path spelt in another case gives
// the same directory another string, and a hub keyed on the path alone made two worktrees of
// one (D6). It is "" when the directory cannot be opened.
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
	id := ""
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) == nil {
		id = fmt.Sprintf("%x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
	}
	return id
}
