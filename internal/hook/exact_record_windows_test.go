//go:build windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"golang.org/x/sys/windows"
)

func TestExactRecordWindowsReparseAndFileIndexVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.execution")
	raw, err := sessionproof.EncodeRecord(sessionproof.Record{
		Version:     sessionproof.FormatVersion,
		Origin:      sessionproof.OriginHook,
		Generation:  2,
		ExecutionID: testExecutionID("1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	info, id, err := statExactRecordFile(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("statExactRecordFile = (%v, %+v, %v)", info, id, err)
	}
	f, err := openExactRecordFile(path, id)
	if err != nil {
		t.Fatalf("openExactRecordFile = %v", err)
	}
	_ = f.Close()

	// Volume serial / file index mismatch rejects a file swapped after Lstat.
	if _, err := openExactRecordFile(path, exactRecordID{vol: id.vol + 1, idxHi: id.idxHi, idxLo: id.idxLo}); !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("mismatched volume serial = %v, want ErrInvalidRecord", err)
	}

	origInfo := winExactRecordFileInfo
	origType := winExactRecordFileType
	t.Cleanup(func() {
		winExactRecordFileInfo = origInfo
		winExactRecordFileType = origType
	})
	winExactRecordFileInfo = func(h windows.Handle, bhfi *windows.ByHandleFileInformation) error {
		if err := origInfo(h, bhfi); err != nil {
			return err
		}
		bhfi.FileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
		return nil
	}
	if _, err := openExactRecordFile(path, id); !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("reparse point open = %v, want ErrInvalidRecord", err)
	}
	if _, _, err := statExactRecordFile(path); !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("reparse point stat = %v, want ErrInvalidRecord", err)
	}
	winExactRecordFileInfo = origInfo
	winExactRecordFileType = func(windows.Handle) (uint32, error) {
		return windows.FILE_TYPE_PIPE, nil
	}
	if _, err := openExactRecordFile(path, id); !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("non-disk handle = %v, want ErrInvalidRecord", err)
	}
	winExactRecordFileType = origType

	// UTF-16 NUL path errors, CreateFile errors, and GetFileInformationByHandle errors.
	if _, _, err := statExactRecordFile("bad\x00.execution"); err == nil {
		t.Fatal("statExactRecordFile accepted NUL byte path")
	}
	if _, err := openExactRecordFile("bad\x00.execution", id); err == nil {
		t.Fatal("openExactRecordFile accepted NUL byte path")
	}

	origCreate := winExactRecordCreateFile
	t.Cleanup(func() { winExactRecordCreateFile = origCreate })
	winExactRecordCreateFile = func(*uint16, uint32, uint32, *windows.SecurityAttributes, uint32, uint32, windows.Handle) (windows.Handle, error) {
		return 0, windows.ERROR_ACCESS_DENIED
	}
	if _, _, err := statExactRecordFile(path); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("statExactRecordFile CreateFile error = %v", err)
	}
	if _, err := openExactRecordFile(path, id); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("openExactRecordFile CreateFile error = %v", err)
	}
	winExactRecordCreateFile = origCreate

	winExactRecordFileInfo = func(windows.Handle, *windows.ByHandleFileInformation) error {
		return windows.ERROR_INVALID_HANDLE
	}
	if _, err := openExactRecordFile(path, id); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("openExactRecordFile GetFileInformationByHandle error = %v", err)
	}
}

func TestWindowsSessionPathAbsentAndParentFileDamaged(t *testing.T) {
	dir := t.TempDir()
	// Missing intermediate directory still counts as absent.
	missingLeaf := filepath.Join(dir, "missing", "sessions")
	if _, err := os.Lstat(missingLeaf); !sessionPathAbsent(missingLeaf, err) {
		t.Fatalf("missing subdir graded non-absent: %v", err)
	}

	// File ancestor (ERROR_PATH_NOT_FOUND) grades as damaged/error, matching Unix ENOTDIR.
	fileParent := filepath.Join(dir, "file")
	if err := os.WriteFile(fileParent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	underFile := filepath.Join(fileParent, "sessions", "rec.execution")
	if _, err := os.Lstat(underFile); sessionPathAbsent(underFile, err) {
		t.Fatalf("file-ancestor path graded absent: %v", err)
	}
	if _, st, err := readExactRecordDeadline(underFile, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("readExactRecordDeadline under file ancestor = (%v, %v), want FileDamaged", st, err)
	}
	if _, err := readGenerationDeadline(filepath.Join(fileParent, "gen"), nil, timeZero); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("readGenerationDeadline under file ancestor = %v, want ErrInvalidGeneration", err)
	}

	// Non-ErrNotExist ancestor lstat error and walk to volume root branches.
	origLstat := lstatSessionFile
	t.Cleanup(func() { lstatSessionFile = origLstat })
	pathErr := &os.PathError{Op: "lstat", Path: missingLeaf, Err: windows.ERROR_PATH_NOT_FOUND}
	lstatSessionFile = func(string) (os.FileInfo, error) {
		return nil, os.ErrPermission
	}
	if sessionPathAbsent(missingLeaf, pathErr) {
		t.Fatal("permission error on ancestor graded absent")
	}
	lstatSessionFile = func(string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	}
	if !sessionPathAbsent(missingLeaf, pathErr) {
		t.Fatal("missing ancestors to volume root did not grade absent")
	}
}

func TestReadExactRecordDeadlineWindowsOpenRacesAndDamage(t *testing.T) {
	dir := t.TempDir()
	raw, err := sessionproof.EncodeRecord(sessionproof.Record{
		Version:     sessionproof.FormatVersion,
		Origin:      sessionproof.OriginHook,
		Generation:  4,
		ExecutionID: testExecutionID("f"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Open-time ErrNotExist race where Lstat finds a directory (non-absent) grades FileDamaged.
	vanishDir := filepath.Join(dir, "vanish-dir.execution")
	if err := os.WriteFile(vanishDir, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	origOpen := exactRecordOpen
	t.Cleanup(func() { exactRecordOpen = origOpen })
	exactRecordOpen = func(p string, _ exactRecordID) (*os.File, error) {
		_ = os.Remove(p)
		if mkErr := os.Mkdir(p, 0o700); mkErr != nil {
			t.Fatal(mkErr)
		}
		return nil, os.ErrNotExist
	}
	if _, st, err := readExactRecordDeadline(vanishDir, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("open ErrNotExist directory race = (%v, %v), want FileDamaged/ErrInvalidRecord", st, err)
	}
	exactRecordOpen = origOpen

	// Real NTFS file swapped between statExactRecordFile and openExactRecordFile grades FileDamaged.
	realExec := filepath.Join(dir, "swap.execution")
	otherExec := filepath.Join(dir, "other.execution")
	if err := os.WriteFile(realExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherExec, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	origCreate := winExactRecordCreateFile
	t.Cleanup(func() { winExactRecordCreateFile = origCreate })
	swapped := false
	winExactRecordCreateFile = func(name *uint16, access, share uint32, sa *windows.SecurityAttributes, disp, flags uint32, tmpl windows.Handle) (windows.Handle, error) {
		if !swapped && flags == windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT {
			swapped = true
			if rerr := os.Rename(otherExec, realExec); rerr != nil {
				t.Fatal(rerr)
			}
		}
		return origCreate(name, access, share, sa, disp, flags, tmpl)
	}
	if _, st, err := readExactRecordDeadline(realExec, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("swapped file identity = (%v, %v), want FileDamaged/ErrInvalidRecord", st, err)
	}
	winExactRecordCreateFile = origCreate

	// Open-time ERROR_ACCESS_DENIED and over-limit (4096-byte) regular file both grade FileDamaged.
	winExactRecordCreateFile = func(name *uint16, access, share uint32, sa *windows.SecurityAttributes, disp, flags uint32, tmpl windows.Handle) (windows.Handle, error) {
		if flags == windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT {
			return 0, windows.ERROR_ACCESS_DENIED
		}
		return origCreate(name, access, share, sa, disp, flags, tmpl)
	}
	if _, st, err := readExactRecordDeadline(realExec, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) || !strings.Contains(err.Error(), windows.ERROR_ACCESS_DENIED.Error()) {
		t.Fatalf("open ERROR_ACCESS_DENIED = (%v, %v), want FileDamaged wrapping ErrInvalidRecord + Access is denied", st, err)
	}
	winExactRecordCreateFile = origCreate

	oversized := filepath.Join(dir, "oversized.execution")
	if err := os.WriteFile(oversized, make([]byte, sessionproof.RecordReadLimit), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, st, err := readExactRecordDeadline(oversized, nil, timeZero); st != sessionproof.FileDamaged || !errors.Is(err, sessionproof.ErrInvalidRecord) {
		t.Fatalf("oversized exact record = (%v, %v), want FileDamaged/ErrInvalidRecord", st, err)
	}
}
