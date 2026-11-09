package accountimport

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type guardFileInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (info guardFileInfo) Mode() os.FileMode { return info.mode }

type guardReader struct {
	io.Reader
	closed bool
}

func (reader *guardReader) Close() error { reader.closed = true; return nil }

func TestSourceGuardRefusesFilesystemFailuresWithoutExposingDetails(t *testing.T) {
	file := sourceFile(t, sourceSchema, 1)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal("could not inspect synthetic source")
	}
	for _, failure := range []string{"-wal", "-shm", "-journal", "missing-main", "nonregular", "open", "read", "short", "magic", "read-version", "write-version"} {
		t.Run(failure, func(t *testing.T) {
			opened := false
			stat := func(path string) (os.FileInfo, error) {
				if path == file {
					switch failure {
					case "missing-main":
						return nil, os.ErrNotExist
					case "nonregular":
						return guardFileInfo{info, os.ModeNamedPipe}, nil
					}
					return info, nil
				}
				if path == file+failure {
					return nil, errors.New("private-filesystem-sentinel")
				}
				return nil, os.ErrNotExist
			}
			var header [20]byte
			copy(header[:16], "SQLite format 3\x00")
			header[18], header[19] = 1, 1
			switch failure {
			case "magic":
				header[0] = '?'
			case "write-version":
				header[18] = 2
			case "read-version":
				header[19] = 2
			}
			reader := &guardReader{Reader: bytes.NewReader(header[:])}
			if failure == "short" {
				reader.Reader = bytes.NewReader(header[:19])
			}
			if failure == "read" {
				reader.Reader = brokenReader{}
			}
			open := func(string) (io.ReadCloser, error) {
				opened = true
				if failure == "open" {
					return nil, errors.New("private-filesystem-sentinel")
				}
				return reader, nil
			}
			err := inspectCheckpointedSource(file, stat, open)
			if err == nil || !strings.Contains(err.Error(), "checkpointed") || strings.Contains(err.Error(), "private") {
				t.Fatal("filesystem refusal was missing, not actionable or disclosed details")
			}
			preOpen := strings.HasPrefix(failure, "-") || failure == "missing-main" || failure == "nonregular"
			if preOpen && opened {
				t.Fatal("unsafe source reached the file opener")
			}
			if opened && failure != "open" && !reader.closed {
				t.Fatal("rejected source handle remained open")
			}
		})
	}
	if _, err := Read(t.Context(), t.TempDir(), testManifest()); err == nil {
		t.Fatal("directory was treated as a SQLite source")
	}
}
