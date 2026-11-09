package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// tokenOwner is the process boundary for the account a token is restricted to. No real
// account fails this for a file it has just created, and the partial file must still go.
var tokenOwner = windows.GetCurrentProcessToken().GetTokenUser

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
	inherit := ""
	if info.IsDir() {
		inherit = "OICI"
	}
	return restrictWith(path, inherit, windows.GetCurrentProcessToken().GetTokenUser)
}

// restrictTo replaces the file's DACL with one entry for its owner and drops inheritance.
func restrictTo(path string, owner func() (*windows.Tokenuser, error)) error {
	return restrictWith(path, "", owner)
}

// restrictWith is restrictTo with ACE inheritance flags, such as OICI for a directory.
func restrictWith(path, inherit string, owner func() (*windows.Tokenuser, error)) error {
	user, err := owner()
	if err != nil {
		return err
	}
	dacl, err := ownerDACL(inherit, user.User.Sid.String())
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func ownerDACL(inherit, sid string) (*windows.ACL, error) {
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	dacl, _, err := descriptor.DACL()
	return dacl, err
}
