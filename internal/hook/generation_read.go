package hook

import (
	"io"
	"os"
	"time"
)

// GenerationReadLimit bounds a generation or end-marker read. A file filling the bound is invalid.
const GenerationReadLimit = 64

var generationFileOpen = OpenGenerationFile

func readGenerationBytes(path string) ([]byte, error) {
	return readRegularBoundedBytesDeadline(path, GenerationReadLimit, nil, time.Time{})
}

func deadlineExpired(now func() time.Time, deadline time.Time) bool {
	if deadline.IsZero() {
		return false
	}
	if now == nil {
		now = time.Now
	}
	return !now().Before(deadline)
}

func readRegularBoundedBytesDeadline(path string, limit int64, now func() time.Time, deadline time.Time) ([]byte, error) {
	if deadlineExpired(now, deadline) {
		return nil, os.ErrDeadlineExceeded
	}
	f, err := generationFileOpen(path)
	if err != nil {
		return nil, err
	}
	if deadlineExpired(now, deadline) {
		_ = f.Close()
		return nil, os.ErrDeadlineExceeded
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrInvalidGeneration
	}
	if deadlineExpired(now, deadline) {
		_ = f.Close()
		return nil, os.ErrDeadlineExceeded
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, limit))
	_ = f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if int64(len(raw)) == limit {
		return nil, ErrInvalidGeneration
	}
	return raw, nil
}
