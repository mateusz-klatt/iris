package config

import (
	"golang.org/x/sys/windows"
)

func secureToken(path string) error {
	return restrictTo(path, windows.GetCurrentProcessToken().GetTokenUser)
}

// restrictTo replaces the file's DACL with one entry for its owner and drops inheritance.
func restrictTo(path string, owner func() (*windows.Tokenuser, error)) error {
	user, err := owner()
	if err != nil {
		return err
	}
	dacl, err := ownerDACL(user.User.Sid.String())
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func ownerDACL(sid string) (*windows.ACL, error) {
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	dacl, _, err := descriptor.DACL()
	return dacl, err
}
