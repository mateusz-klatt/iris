package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func dacl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecureTokenLeavesOnlyTheCurrentUser(t *testing.T) {
	path := tokenFile(t)
	set := setOwnerSecurity
	t.Cleanup(func() { setOwnerSecurity = set })
	writes := 0
	setOwnerSecurity = func(path string, typ windows.SE_OBJECT_TYPE, info windows.SECURITY_INFORMATION, owner, group *windows.SID, dacl, sacl *windows.ACL) error {
		writes++
		return set(path, typ, info, owner, group, dacl, sacl)
	}
	for range 2 {
		if err := secureToken(path); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 2 {
		t.Fatalf("secureToken wrote the DACL %d times, want 2", writes)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// SDDL writes well-known accounts as aliases, so a CI administrator appears as LA.
	// Build the expected entry the same way instead of comparing raw SIDs.
	expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	want := expected.String()
	want = want[strings.Index(want, "("):]
	got := dacl(t, path)
	if !strings.HasPrefix(got, "D:P") || got[strings.Index(got, "("):] != want {
		t.Fatalf("not owner-only and protected: %s, want %s", got, want)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "secret" {
		t.Fatalf("the owner lost access: %q %v", raw, err)
	}
}

func TestRestrictToChangesNothingWithoutAnOwner(t *testing.T) {
	path := tokenFile(t)
	before := dacl(t, path)
	broken := errors.New("no token")
	if err := restrictTo(path, func() (*windows.Tokenuser, error) { return nil, broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
	if after := dacl(t, path); after != before {
		t.Fatalf("%s became %s", before, after)
	}
}

func TestTokenRemovesAPartialFileWhenItsOwnerCannotBeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.token")
	broken := errors.New("no token")
	original := tokenOwner
	t.Cleanup(func() { tokenOwner = original })
	tokenOwner = func() (*windows.Tokenuser, error) { return nil, broken }
	if _, err := Token(path); !errors.Is(err, broken) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial token left behind", err)
	}
}

func TestOwnerDACLRefusesAnInvalidSID(t *testing.T) {
	if _, err := ownerDACL("", "not-a-sid"); err == nil {
		t.Fatal("no error")
	}
}

func TestRestrictToRefusesAnInvalidOwner(t *testing.T) {
	path := tokenFile(t)
	before := dacl(t, path)
	empty := func() (*windows.Tokenuser, error) { return &windows.Tokenuser{}, nil }
	if err := restrictTo(path, empty); err == nil {
		t.Fatal("no error")
	}
	if after := dacl(t, path); after != before {
		t.Fatalf("%s became %s", before, after)
	}
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid.String()
}

// entries is the DACL's ACE list written the way SDDL writes these flags and this SID.
func entries(t *testing.T, sddl string) string {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	return s[strings.Index(s, "("):]
}

func TestOwnerOnlyAppliesTheTokenDACL(t *testing.T) {
	sid := currentUserSID(t)
	dir := t.TempDir()
	// Start with a protected DACL that is missing the directory's inheritance flags.
	if err := restrictTo(dir, tokenOwner); err != nil {
		t.Fatal(err)
	}
	if err := OwnerOnly(dir); err != nil {
		t.Fatal(err)
	}
	got := dacl(t, dir)
	if want := entries(t, "D:P(A;OICI;FA;;;"+sid+")"); !strings.HasPrefix(got, "D:P") || got[strings.Index(got, "("):] != want {
		t.Fatalf("directory: %s, want %s", got, want)
	}
	t.Run("matching directory is left alone", func(t *testing.T) {
		set := setOwnerSecurity
		t.Cleanup(func() { setOwnerSecurity = set })
		setOwnerSecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION, *windows.SID, *windows.SID, *windows.ACL, *windows.ACL) error {
			return windows.ERROR_ACCESS_DENIED
		}
		if err := OwnerOnly(dir); err != nil {
			t.Fatalf("matching directory attempted to rewrite its DACL: %v", err)
		}
	})
	// A file created inside, before any call on it, already has only the owner's entry.
	file := filepath.Join(dir, "new")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = dacl(t, file)
	if want := entries(t, "D:(A;ID;FA;;;"+sid+")"); got[strings.Index(got, "("):] != want {
		t.Fatalf("a new file inherited more than the owner: %s, want %s", got, want)
	}
	if err := OwnerOnly(file); err != nil {
		t.Fatal(err)
	}
	got = dacl(t, file)
	if want := entries(t, "D:P(A;;FA;;;"+sid+")"); !strings.HasPrefix(got, "D:P") || got[strings.Index(got, "("):] != want {
		t.Fatalf("file: %s, want %s", got, want)
	}
	if err := OwnerOnly(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing path was protected")
	}
}

func TestOwnerOnlySkipsOnlyTheExactDACL(t *testing.T) {
	sid := currentUserSID(t)
	dir, file := t.TempDir(), tokenFile(t)
	dirDACL := "D:P(A;OICI;FA;;;" + sid + ")"
	fileDACL := "D:P(A;;FA;;;" + sid + ")"
	for _, test := range []struct {
		name    string
		sddl    string
		file    bool
		skip    bool
		readErr error
		aceErr  bool
	}{
		{name: "exact directory", sddl: dirDACL, skip: true},
		{name: "exact file", sddl: fileDACL, file: true, skip: true},
		{name: "exact DACL with different owner", sddl: "O:WD" + dirDACL, skip: true},
		{name: "unprotected directory", sddl: "D:(A;OICI;FA;;;" + sid + ")"},
		{name: "directory without inheritance", sddl: fileDACL},
		{name: "directory with object inheritance only", sddl: "D:P(A;OI;FA;;;" + sid + ")"},
		{name: "directory with container inheritance only", sddl: "D:P(A;CI;FA;;;" + sid + ")"},
		{name: "directory with inherit-only grant", sddl: "D:P(A;OICIIO;FA;;;" + sid + ")"},
		{name: "directory with no propagation", sddl: "D:P(A;OICINP;FA;;;" + sid + ")"},
		{name: "file with inheritance flags", sddl: dirDACL, file: true},
		{name: "file with inherited grant", sddl: "D:(A;ID;FA;;;" + sid + ")", file: true},
		{name: "protected file with inherited grant", sddl: "D:P(A;ID;FA;;;" + sid + ")", file: true},
		{name: "null DACL", sddl: "D:PNO_ACCESS_CONTROL"},
		{name: "empty DACL", sddl: "D:P"},
		{name: "extra grant", sddl: dirDACL + "(A;OICI;FR;;;WD)"},
		{name: "deny", sddl: "D:P(D;OICI;FA;;;" + sid + ")"},
		{name: "wrong mask", sddl: "D:P(A;OICI;FR;;;" + sid + ")"},
		{name: "wrong SID", sddl: "D:P(A;OICI;FA;;;WD)"},
		{name: "missing security descriptor"},
		{name: "security read failed", readErr: windows.ERROR_ACCESS_DENIED},
		{name: "ACE read failed", sddl: dirDACL, aceErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := dir
			if test.file {
				path = file
			}
			var sd *windows.SECURITY_DESCRIPTOR
			if test.sddl != "" {
				var err error
				sd, err = windows.SecurityDescriptorFromString(test.sddl)
				if err != nil {
					t.Fatal(err)
				}
			}
			get, getACE, set := ownerSecurity, ownerACE, setOwnerSecurity
			t.Cleanup(func() { ownerSecurity, ownerACE, setOwnerSecurity = get, getACE, set })
			reads, writes := 0, 0
			ownerSecurity = func(name string, typ windows.SE_OBJECT_TYPE, info windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
				reads++
				if name != path || typ != windows.SE_FILE_OBJECT || info != windows.DACL_SECURITY_INFORMATION {
					t.Fatalf("unexpected security read for %q: type %v, information %v", name, typ, info)
				}
				return sd, test.readErr
			}
			if test.aceErr {
				ownerACE = func(*windows.ACL, uint32, **windows.ACCESS_ALLOWED_ACE) error { return windows.ERROR_INVALID_ACL }
			}
			setOwnerSecurity = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION, *windows.SID, *windows.SID, *windows.ACL, *windows.ACL) error {
				writes++
				return nil
			}
			if err := OwnerOnly(path); err != nil {
				t.Fatal(err)
			}
			wantWrites := 1
			if test.skip {
				wantWrites = 0
			}
			if reads != 1 || writes != wantWrites {
				t.Fatalf("DACL reads and writes: got %d and %d, want 1 and %d", reads, writes, wantWrites)
			}
		})
	}
}

func TestOwnerOnlyRefusesWhenTheUserCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	before := dacl(t, dir)
	broken := errors.New("no token")
	original := tokenOwner
	t.Cleanup(func() { tokenOwner = original })
	tokenOwner = func() (*windows.Tokenuser, error) { return nil, broken }
	if err := OwnerOnly(dir); !errors.Is(err, broken) {
		t.Fatalf("OwnerOnly returned %v, want %v", err, broken)
	}
	if after := dacl(t, dir); after != before {
		t.Fatalf("%s became %s", before, after)
	}
}

func TestTokenRefusesAnExistingFileMovedBeforeProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.token")
	want, err := Token(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := path + ".moved"
	original, originalRead := lstatToken, readTokenFile
	t.Cleanup(func() { lstatToken, readTokenFile = original, originalRead })
	readTokenFile = func(string) ([]byte, error) {
		t.Fatal("token was read after protection failed")
		return nil, nil
	}
	// Model a concurrent move after the regular-file check. Unlike a DACL denial,
	// a missing path also prevents an administrator from protecting the file.
	lstatToken = func(name string) (os.FileInfo, error) {
		info, err := original(name)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(name, moved); err != nil {
			t.Fatal(err)
		}
		return info, nil
	}
	if token, err := Token(path); token != "" || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("token whose protection failed was accepted: %q %v", token, err)
	}
	if raw, err := os.ReadFile(moved); err != nil || strings.TrimSpace(string(raw)) != want {
		t.Fatalf("refusal altered the moved token: %q %v", raw, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal recreated the moved token: %v", err)
	}
}
