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
	got := dacl(t, path)
	if !strings.HasPrefix(got, "D:P") || strings.Count(got, "(") != 1 || !strings.Contains(got, ";"+user.User.Sid.String()+")") {
		t.Fatalf("not owner-only and protected: %s", got)
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
	if _, err := ownerDACL("not-a-sid"); err == nil {
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
