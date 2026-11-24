//go:build !windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGenerationReaderRejectsFIFOAndSymlinkPromptly(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "generation.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	endedFIFO := fifo + ".ended"
	if err := syscall.Mkfifo(endedFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(fifo), "generation.link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, endedFIFO, link} {
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
			case <-time.After(200 * time.Millisecond):
				t.Fatal("generation read blocked waiting for a FIFO writer")
			}
		})
	}
}

func TestGenerationReaderFollowsRegularFileSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "generation")
	if err := os.WriteFile(target, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(target), "generation.link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if generation, err := ReadGeneration(link); generation != 4 || err != nil {
		t.Fatalf("regular-file symlink = G%d, %v; want G4", generation, err)
	}
}

func TestFIFOEndMarkerAllowsPreToolPromptly(t *testing.T) {
	root := gitRepo(t)
	getenv := failureEnv(t)
	m := sessionsFor(getenv)
	m.Remember("claude", "s1", 4)
	marker := m.name("claude", "s1") + ".ended"
	if err := syscall.Mkfifo(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &recoveryHub{generation: 1}
	type outcome struct {
		out     Output
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(writePayload(root, "s1", "a.go")),
			Hub: h, Memory: m, Getenv: getenv, after: never, logAfter: never,
		}.Run()
		done <- outcome{out: out, elapsed: time.Since(start)}
	}()
	const runLimit = 500 * time.Millisecond
	select {
	case <-time.After(runLimit):
		t.Fatal("PreToolUse blocked on a FIFO end marker")
	case got := <-done:
		if got.elapsed >= runLimit {
			t.Fatalf("PreToolUse with a FIFO end marker took %s", got.elapsed)
		}
		assertAllow(t, ClientClaude, got.out)
	}
	if h.touched != 0 || len(h.sent) != 0 {
		t.Fatalf("FIFO marker allowed registration or touch: requests %v, touches %d", h.sent, h.touched)
	}
	notes, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "damaged generation proof at " + marker
	if len(notes) != 1 || notes[0].Error != want {
		t.Fatalf("observations = %+v, want %q", notes, want)
	}
}
