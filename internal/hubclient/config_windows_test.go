package hubclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"golang.org/x/sys/windows"
)

func TestWriteConfigRefusesADirectoryThatCannotCreateFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	const before = "existing settings\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deny FILE_ADD_FILE, but retain access to the existing config and DACL.
	sd, err := windows.SecurityDescriptorFromString("D:P(D;;0x2;;;WD)(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := config.OwnerOnly(dir); err != nil {
			t.Error(err)
		}
	})
	if err := WriteConfig(path, FileConfig{Hub: "https://iris.example"}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("a directory denying new files accepted a config: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != before {
		t.Fatalf("existing settings changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "client.json" {
		t.Fatalf("failed write left temporary files: %v, %v", entries, err)
	}
}
