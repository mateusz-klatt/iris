//go:build !windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGenerationReaderRejectsFIFOAndSymlinkPromptly(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "generation.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(fifo), "generation.link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := ReadGeneration(path)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrInvalidGeneration) {
					t.Fatalf("FIFO was not damaged generation input: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("generation read blocked waiting for a FIFO writer")
			}
		})
	}
}
