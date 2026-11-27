package config

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tokenOwner is the process boundary for the account a token is restricted to. No real
// account fails this for a file it has just created, and the partial file must still go.
var tokenOwner = windows.GetCurrentProcessToken().GetTokenUser
var ownerSecurity = windows.GetNamedSecurityInfo
var ownerACE = windows.GetAce
var setOwnerSecurity = windows.SetNamedSecurityInfo

func secureToken(path string) error {
	return restrictTo(path, tokenOwner)
}

// OwnerOnly gives a file or directory the token's protected DACL: one entry for the current
// user, and nothing inherited. Files beside the token otherwise inherit SYSTEM and
// Administrators from the user's profile. On a directory the entry is also inherited, so a
// file created inside is owner-only from the moment it exists.
func OwnerOnly(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	user, err := tokenOwner()
	if err != nil {
		return err
	}
	inherit := ""
	var flags uint8
	if info.IsDir() {
		inherit = "OICI"
		flags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	// Reapplying an inheritable DACL propagates its entries to existing children.
	if ownerDACLMatches(path, flags, user.User.Sid) {
		return nil
	}
	return restrictWith(path, inherit, user.User.Sid)
}

func ownerDACLMatches(path string, flags uint8, user *windows.SID) bool {
	sd, err := ownerSecurity(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
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
	if ownerACE(dacl, 0, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != flags {
		return false
	}
	const fileAllAccess = 0x1f01ff // FILE_ALL_ACCESS
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return ace.Mask == fileAllAccess && windows.EqualSid(sid, user)
}

// restrictTo replaces the file's DACL with one entry for its owner and drops inheritance.
func restrictTo(path string, owner func() (*windows.Tokenuser, error)) error {
	user, err := owner()
	if err != nil {
		return err
	}
	return restrictWith(path, "", user.User.Sid)
}

// restrictWith writes the protected DACL with the requested ACE inheritance flags.
func restrictWith(path, inherit string, sid *windows.SID) error {
	dacl, err := ownerDACL(inherit, sid.String())
	if err != nil {
		return err
	}
	return setOwnerSecurity(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func ownerDACL(inherit, sid string) (*windows.ACL, error) {
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	dacl, _, err := descriptor.DACL()
	return dacl, err
}
