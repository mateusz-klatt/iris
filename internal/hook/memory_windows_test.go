package hook

import (
	"os"
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
	// The window holzera asked about: a temporary file is owner-only before its own call.
	tmp, err := os.CreateTemp(m.dir, ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	sd, err := windows.GetNamedSecurityInfo(tmp.Name(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if got := sd.String(); strings.Count(got, "(") != 1 || !strings.Contains(got, user.User.Sid.String()) && !strings.Contains(got, ";;;LA)") {
		t.Fatalf("a file was readable by others before it was protected: %s", got)
	}
	// Pruning and SessionEnd removal still work under the protected DACL.
	m.Forget("claude", "s")
	if m.Recall("claude", "s") != 0 {
		t.Fatal("forget failed")
	}
	m.prune()
}
