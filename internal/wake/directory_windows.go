package wake

import (
	"errors"
	"os"
	"unsafe"

	"github.com/mateusz-klatt/iris/internal/config"
	"golang.org/x/sys/windows"
)

var directorySecurity = windows.GetNamedSecurityInfo
var directoryUser = windows.GetCurrentProcessToken().GetTokenUser
var directoryRestrict = config.OwnerOnly
var directoryMake = os.MkdirAll
var directoryStat = os.Lstat
var directoryACE = windows.GetAce

func privateDirectory(path string) error {
	if err := directoryMake(path, 0700); err != nil {
		return err
	}
	info, err := directoryStat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("wake directory is not a directory")
	}
	user, err := directoryUser()
	if err != nil {
		return err
	}
	if directoryPrivate(path, user.User.Sid) {
		return nil
	}
	if err := directoryRestrict(path); err != nil {
		return err
	}
	if !directoryPrivate(path, user.User.Sid) {
		return errors.New("wake directory is not private")
	}
	return nil
}

func directoryPrivate(path string, user *windows.SID) bool {
	sd, err := directorySecurity(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !windows.EqualSid(owner, user) {
		return false
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if directoryACE(dacl, 0, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		return false
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return ace.Mask == windows.ACCESS_MASK(0x1f01ff) && windows.EqualSid(sid, user)
}

// Windows does not provide the Unix directory-sync durability guarantee. Reservation
// accounting survives cooperating process restarts; whole-host power loss is not promised.
func syncDirectory(string) error { return nil }
