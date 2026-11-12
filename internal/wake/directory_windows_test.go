package wake

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStateWindowsDirectoryChecksActualOwnerAndEveryGrant(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	security := directorySecurity
	t.Cleanup(func() { directorySecurity = security })
	for name, sddl := range map[string]string{
		"correct":                    "O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")",
		"Everyone protected":         "O:" + sid + "D:P(A;OICI;FA;;;WD)",
		"wrong user protected":       "O:" + sid + "D:P(A;OICI;FA;;;SY)",
		"wrong owner":                "O:SYD:P(A;OICI;FA;;;" + sid + ")",
		"missing owner":              "D:P(A;OICI;FA;;;" + sid + ")",
		"null DACL":                  "O:" + sid + "D:PNO_ACCESS_CONTROL",
		"empty DACL":                 "O:" + sid + "D:P",
		"unprotected":                "O:" + sid + "D:(A;OICI;FA;;;" + sid + ")",
		"no inheritance":             "O:" + sid + "D:P(A;;FA;;;" + sid + ")",
		"object inheritance only":    "O:" + sid + "D:P(A;OI;FA;;;" + sid + ")",
		"container inheritance only": "O:" + sid + "D:P(A;CI;FA;;;" + sid + ")",
		"inherit only":               "O:" + sid + "D:P(A;OICIIO;FA;;;" + sid + ")",
		"insufficient access":        "O:" + sid + "D:P(A;OICI;FR;;;" + sid + ")",
		"extra grant":                "O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FR;;;WD)",
		"deny":                       "O:" + sid + "D:P(D;OICI;FA;;;" + sid + ")",
	} {
		t.Run(name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(sddl)
			if err != nil {
				t.Fatal(err)
			}
			directorySecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
				return sd, nil
			}
			if got := directoryPrivate("directory", user.User.Sid); got != (name == "correct") {
				t.Fatalf("ACL validation: %s accepted %t", sddl, got)
			}
		})
	}
	directorySecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
		return nil, errStateTest
	}
	if directoryPrivate("directory", user.User.Sid) {
		t.Fatal("security read failure accepted")
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	directorySecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
		return sd, nil
	}
	ace := directoryACE
	t.Cleanup(func() { directoryACE = ace })
	directoryACE = func(*windows.ACL, uint32, **windows.ACCESS_ALLOWED_ACE) error { return errStateTest }
	if directoryPrivate("directory", user.User.Sid) {
		t.Fatal("unreadable ACE accepted")
	}
}

func TestStateWindowsDirectoryCorrectsOrRefusesUnsafeACLsAndKeepsCorrectACLCheap(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	for name, sddl := range map[string]string{
		"Everyone":          "D:P(A;OICI;FA;;;WD)",
		"wrong user":        "D:P(A;OICI;FA;;;SY)(A;;FA;;;" + sid + ")",
		"null":              "D:PNO_ACCESS_CONTROL",
		"wrong inheritance": "D:P(A;;FA;;;" + sid + ")",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sd, err := windows.SecurityDescriptorFromString(sddl)
			if err != nil {
				t.Fatal(err)
			}
			acl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			if directoryPrivate(dir, user.User.Sid) {
				t.Fatal("unsafe real ACL was accepted")
			}
			if err := privateDirectory(dir); err != nil {
				t.Fatal(err)
			}
			if !directoryPrivate(dir, user.User.Sid) {
				t.Fatal("unsafe real ACL was not corrected")
			}
			// A protected wake directory passes its privacy to a temporary file before any bytes.
			f, err := os.CreateTemp(dir, "private-*")
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := windows.GetNamedSecurityInfo(f.Name(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			inherited, _, err := got.DACL()
			if err != nil || inherited == nil || inherited.AceCount != 1 {
				t.Fatal("temporary file inherited additional grants", err)
			}
			restrict := directoryRestrict
			t.Cleanup(func() { directoryRestrict = restrict })
			directoryRestrict = func(string) error { t.Fatal("correct inheritable DACL was reapplied"); return errStateTest }
			if err := privateDirectory(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestStateWindowsDirectoryProtectionPropagatesEveryFailure(t *testing.T) {
	for _, boundary := range []string{"mkdir", "stat", "not directory", "user", "restrict", "verify"} {
		t.Run(boundary, func(t *testing.T) {
			makeDir, stat, user, restrict, security := directoryMake, directoryStat, directoryUser, directoryRestrict, directorySecurity
			t.Cleanup(func() {
				directoryMake, directoryStat, directoryUser, directoryRestrict, directorySecurity = makeDir, stat, user, restrict, security
			})
			dir := t.TempDir()
			switch boundary {
			case "mkdir":
				directoryMake = func(string, os.FileMode) error { return errStateTest }
			case "stat":
				directoryStat = func(string) (os.FileInfo, error) { return nil, errStateTest }
			case "not directory":
				dir = filepath.Join(dir, "file")
				if err := os.WriteFile(dir, nil, 0600); err != nil {
					t.Fatal(err)
				}
				directoryMake = func(string, os.FileMode) error { return nil }
			case "user":
				directoryUser = func() (*windows.Tokenuser, error) { return nil, errStateTest }
			case "restrict":
				directorySecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
					return nil, errStateTest
				}
				directoryRestrict = func(string) error { return errStateTest }
			case "verify":
				directorySecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
					return nil, errStateTest
				}
				directoryRestrict = func(string) error { return nil }
			}
			if err := privateDirectory(dir); err == nil {
				t.Fatal("unsafe directory accepted")
			}
		})
	}
	// Failure to make a directory must also preserve an unrelated file.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := privateDirectory(file); err == nil {
		t.Fatal("file replaced by directory")
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "preserved" {
		t.Fatalf("directory protection changed an unrelated file: %q %v", got, err)
	}
}
