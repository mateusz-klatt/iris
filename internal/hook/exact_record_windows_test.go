//go:build windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
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
}
