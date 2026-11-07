package hook

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The session files must be as private as the token: one entry, nothing inherited.
func TestFileMemoryIsOwnerOnlyOnWindows(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	want := expected.String()
	want = want[strings.Index(want, "("):]
	for _, path := range []string{m.dir, m.name("claude", "s")} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		got := sd.String()
		if !strings.HasPrefix(got, "D:P") || got[strings.Index(got, "("):] != want {
			t.Fatalf("%s is not owner-only: %s, want %s", path, got, want)
		}
	}
	if m.Recall("claude", "s") != 1 {
		t.Fatal("the owner lost access")
	}
}
