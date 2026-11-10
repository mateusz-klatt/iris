package hook

import (
	"os"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
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
	sid := user.User.Sid.String()
	// The directory's entry is inherited by what is created inside; the file's is not.
	for path, sddl := range map[string]string{m.dir: "D:P(A;OICI;FA;;;" + sid + ")", m.name("claude", "s"): "D:P(A;;FA;;;" + sid + ")"} {
		expected, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		want := expected.String()
		want = want[strings.Index(want, "("):]
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
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
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
	if err := m.prune(); err != nil {
		t.Fatal(err)
	}
}

func TestFileMemoryKeepsProofWhenItsDirectoryIsReplacedByAFile(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 3)
	t.Run("directory replaced after protection", func(t *testing.T) {
		saved := m.dir + ".saved"
		m.restrict = func(path string) error {
			if err := config.OwnerOnly(path); err != nil {
				return err
			}
			// A concurrent path replacement can refuse creation after protection succeeded.
			if path == m.dir {
				if err := os.Rename(m.dir, saved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(m.dir); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
					if err := os.Rename(saved, m.dir); err != nil {
						t.Error(err)
					}
				})
				if err := os.WriteFile(m.dir, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
		m.Remember("claude", "s", 4)
		preserved := m
		preserved.dir = saved
		if got := preserved.Recall("claude", "s"); got != 3 {
			t.Fatalf("a failed write replaced the previous proof with %d", got)
		}
		if entries := mustList(t, saved); len(entries) != 1 {
			t.Fatalf("a failed write left temporary files: %v", entries)
		}
		if raw, err := os.ReadFile(m.dir); err != nil || string(raw) != "replacement" {
			t.Fatalf("a failed write changed the replacement file: %q %v", raw, err)
		}
	})
	m.restrict = config.OwnerOnly
	m.Remember("claude", "s", 4)
	if got := m.Recall("claude", "s"); got != 4 {
		t.Fatalf("a write after access was restored did not replace the proof: %d", got)
	}
}
