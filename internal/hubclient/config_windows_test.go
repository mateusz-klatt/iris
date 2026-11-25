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

func TestConfigWindowsReparseAndFileIndexVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, []byte(`{"session_succession":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info, id, err := statRegularConfig(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("statRegularConfig = (%v, %+v, %v)", info, id, err)
	}
	f, actual, err := openRegularConfig(path, id)
	if err != nil || !sameConfigFileID(id, actual) {
		t.Fatalf("openRegularConfig = (%v, %+v, %v)", f, actual, err)
	}
	_ = f.Close()

	// Volume serial / file index mismatch rejects a swapped file between Lstat and open.
	if _, _, err := openRegularConfig(path, configFileID{vol: id.vol + 1, idxHi: id.idxHi, idxLo: id.idxLo}); err == nil {
		t.Fatal("accepted mismatched volume serial number")
	}

	origInfo := winConfigFileInfo
	origType := winConfigFileType
	t.Cleanup(func() {
		winConfigFileInfo = origInfo
		winConfigFileType = origType
	})
	winConfigFileInfo = func(h windows.Handle, bhfi *windows.ByHandleFileInformation) error {
		if err := origInfo(h, bhfi); err != nil {
			return err
		}
		bhfi.FileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
		return nil
	}
	if _, _, err := openRegularConfig(path, id); err == nil {
		t.Fatal("accepted reparse-point attribute")
	}
	if _, _, err := statRegularConfig(path); err == nil {
		t.Fatal("statRegularConfig accepted reparse-point attribute")
	}
	winConfigFileInfo = origInfo
	winConfigFileType = func(windows.Handle) (uint32, error) {
		return windows.FILE_TYPE_PIPE, nil
	}
	if _, _, err := openRegularConfig(path, id); err == nil {
		t.Fatal("accepted non-disk file type")
	}
	winConfigFileType = origType

	// Non-regular directory entry, NUL paths, CreateFile errors, and GetFileInformationByHandle errors.
	if info, _, err := statRegularConfig(dir); err != nil || info.Mode().IsRegular() {
		t.Fatalf("statRegularConfig(dir) = (%v, %v)", info, err)
	}
	if _, _, err := statRegularConfig("bad\x00cfg"); err == nil {
		t.Fatal("statRegularConfig accepted NUL byte path")
	}
	if _, _, err := openRegularConfig("bad\x00cfg", id); err == nil {
		t.Fatal("openRegularConfig accepted NUL byte path")
	}

	origCreate := winConfigCreateFile
	t.Cleanup(func() { winConfigCreateFile = origCreate })
	winConfigCreateFile = func(*uint16, uint32, uint32, *windows.SecurityAttributes, uint32, uint32, windows.Handle) (windows.Handle, error) {
		return 0, windows.ERROR_ACCESS_DENIED
	}
	if _, _, err := statRegularConfig(path); err == nil {
		t.Fatal("statRegularConfig succeeded after CreateFile error")
	}
	if _, _, err := openRegularConfig(path, id); err == nil {
		t.Fatal("openRegularConfig succeeded after CreateFile error")
	}
	winConfigCreateFile = origCreate

	winConfigFileInfo = func(windows.Handle, *windows.ByHandleFileInformation) error {
		return windows.ERROR_INVALID_HANDLE
	}
	if _, _, err := openRegularConfig(path, id); err == nil {
		t.Fatal("openRegularConfig accepted GetFileInformationByHandle error")
	}
}
