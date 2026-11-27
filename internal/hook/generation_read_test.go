package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func TestReadGenerationBytesBoundsAndReadFailures(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.ended")
	if err := os.WriteFile(valid, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readGenerationBytes(valid); err != nil || string(raw) != "4\n" {
		t.Fatalf("valid generation bytes = %q, %v", raw, err)
	}
	full := filepath.Join(dir, "full.ended")
	if err := os.WriteFile(full, []byte(strings.Repeat(" ", GenerationReadLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGenerationBytes(full); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("full-bound generation read = %v", err)
	}

	t.Run("open failure", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		openErr := errors.New("open failure")
		generationFileOpen = func(string) (*os.File, error) { return nil, openErr }
		if _, err := readGenerationBytes(valid); !errors.Is(err, openErr) {
			t.Fatalf("open failure = %v", err)
		}
	})
	t.Run("stat failure", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		generationFileOpen = func(string) (*os.File, error) {
			f, err := os.Open(valid)
			if err != nil {
				return nil, err
			}
			return f, f.Close()
		}
		if _, err := readGenerationBytes(valid); err == nil {
			t.Fatal("closed-file stat succeeded")
		}
	})
	t.Run("non-regular file", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.Close() }()
		generationFileOpen = func(string) (*os.File, error) { return reader, nil }
		if _, err := readGenerationBytes(valid); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("pipe was not damaged generation input: %v", err)
		}
		if err := reader.Close(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("non-regular descriptor was not closed: %v", err)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		generationFileOpen = func(string) (*os.File, error) {
			return os.OpenFile(valid, os.O_WRONLY, 0)
		}
		if _, err := readGenerationBytes(valid); err == nil {
			t.Fatal("write-only file read succeeded")
		}
	})
}

func TestReadRegularBoundedBytesDeadlineStopsAtEachCheck(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Default now (nil) with future deadline succeeds.
	if raw, err := readRegularBoundedBytesDeadline(valid, GenerationReadLimit, nil, time.Now().Add(time.Minute)); err != nil || string(raw) != "7\n" {
		t.Fatalf("nil-now bounded read = %q, %v", raw, err)
	}

	// Exact RecordReadLimit bounds: 4095 succeeds, 4096 fails.
	okExact := filepath.Join(dir, "ok.execution")
	if err := os.WriteFile(okExact, []byte(strings.Repeat("a", sessionproof.RecordReadLimit-1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readRegularBoundedBytesDeadline(okExact, sessionproof.RecordReadLimit, time.Now, time.Now().Add(time.Minute)); err != nil || len(raw) != sessionproof.RecordReadLimit-1 {
		t.Fatalf("4095-byte bounded read = %d, %v", len(raw), err)
	}
	fullExact := filepath.Join(dir, "full.execution")
	if err := os.WriteFile(fullExact, []byte(strings.Repeat("a", sessionproof.RecordReadLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularBoundedBytesDeadline(fullExact, sessionproof.RecordReadLimit, time.Now, time.Now().Add(time.Minute)); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("4096-byte bounded read = %v, want ErrInvalidGeneration", err)
	}

	t.Run("expired before open", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		opened := false
		generationFileOpen = func(path string) (*os.File, error) {
			opened = true
			return original(path)
		}
		now := time.Unix(1_000, 0)
		deadline := now
		if _, err := readRegularBoundedBytesDeadline(valid, GenerationReadLimit, func() time.Time { return now }, deadline); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expired before open = %v, want ErrDeadlineExceeded", err)
		}
		if opened {
			t.Fatal("generationFileOpen ran past deadline")
		}
	})

	t.Run("expired before stat", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		var openedFile *os.File
		generationFileOpen = func(path string) (*os.File, error) {
			clock = deadline
			f, err := original(path)
			openedFile = f
			return f, err
		}
		if _, err := readRegularBoundedBytesDeadline(valid, GenerationReadLimit, func() time.Time { return clock }, deadline); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expired before stat = %v, want ErrDeadlineExceeded", err)
		}
		if openedFile == nil || openedFile.Fd() != ^uintptr(0) {
			t.Fatalf("open file leaked after pre-stat deadline expiry")
		}
	})

	t.Run("expired before read", func(t *testing.T) {
		original := generationFileOpen
		t.Cleanup(func() { generationFileOpen = original })
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		calls := 0
		var openedFile *os.File
		nowFn := func() time.Time {
			calls++
			// Call 1: before open; Call 2: before Stat; Call 3: before ReadAll.
			if calls == 3 {
				clock = deadline
			}
			return clock
		}
		generationFileOpen = func(path string) (*os.File, error) {
			f, err := original(path)
			openedFile = f
			return f, err
		}
		if _, err := readRegularBoundedBytesDeadline(valid, GenerationReadLimit, nowFn, deadline); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expired before read = %v, want ErrDeadlineExceeded", err)
		}
		if openedFile == nil || openedFile.Fd() != ^uintptr(0) {
			t.Fatalf("open file leaked after pre-read deadline expiry")
		}
	})
}
