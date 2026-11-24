//go:build !windows

package hook

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestProcessWitnessFIFOAndSymlinkAreDamagedPromptly(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := successionSessionFiles(t, "cursor", "S1", 1, "ex_one")
			anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
			path := processWitnessPath(s.memory.dir, anc)
			target := path
			if kind == "symlink" {
				target += ".fifo"
			}
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := currentProcessSession("cursor", anc, s.memory.dir)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrInvalidGeneration) {
					t.Fatalf("FIFO witness was not damaged: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("process witness read blocked on a FIFO")
			}
		})
	}
}
