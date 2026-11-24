package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadGenerationBytesBoundsAndReadFailures(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.ended")
	if err := os.WriteFile(valid, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readGenerationBytes(valid, GenerationReadLimit); err != nil || string(raw) != "4\n" {
		t.Fatalf("valid generation bytes = %q, %v", raw, err)
	}
	full := filepath.Join(dir, "full.ended")
	if err := os.WriteFile(full, []byte(strings.Repeat(" ", GenerationReadLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGenerationBytes(full, GenerationReadLimit); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("full-bound generation read = %v", err)
	}

	t.Run("open failure", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		openErr := errors.New("open failure")
		generationFileOpen = func(string) (*os.File, error) { return nil, openErr }
		if _, err := readGenerationBytes(valid, GenerationReadLimit); !errors.Is(err, openErr) {
			t.Fatalf("open failure = %v", err)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		generationFileOpen = func(string) (*os.File, error) {
			f, err := os.Open(valid)
			if err != nil {
				return nil, err
			}
			return f, f.Close()
		}
		if _, err := readGenerationBytes(valid, GenerationReadLimit); err == nil {
			t.Fatal("closed-file read succeeded")
		}
	})
}
