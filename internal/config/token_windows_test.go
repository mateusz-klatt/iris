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
	if err := secureToken(path); err != nil {
		t.Fatal(err)
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
	if err := OwnerOnly(dir); err != nil {
		t.Fatal(err)
	}
	got := dacl(t, dir)
	if want := entries(t, "D:P(A;OICI;FA;;;"+sid+")"); !strings.HasPrefix(got, "D:P") || got[strings.Index(got, "("):] != want {
		t.Fatalf("directory: %s, want %s", got, want)
	}
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
