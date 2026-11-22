package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestSessionsRecallGeneration(t *testing.T) {
	parent := t.TempDir()
	token := filepath.Join(parent, "hub.token")
	s := NewSessions(token)

	// Missing directory and file report absent without creating anything.
	if gen, present, err := s.RecallGeneration("claude", "s1"); gen != 0 || present || err != nil {
		t.Fatalf("missing file: gen=%d present=%v err=%v", gen, present, err)
	}
	if _, err := os.Stat(s.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RecallGeneration created sessions directory: %v", err)
	}
	if gen, present, err := s.RecallGeneration("", "s1"); gen != 0 || present || err != nil {
		t.Fatalf("empty client: gen=%d present=%v err=%v", gen, present, err)
	}
	if gen, present, err := s.RecallGeneration("claude", ""); gen != 0 || present || err != nil {
		t.Fatalf("empty session: gen=%d present=%v err=%v", gen, present, err)
	}

	// Valid generation written by hook memory is recalled accurately.
	s.memory.Remember("claude", "s1", 4)
	if gen, present, err := s.RecallGeneration("claude", "s1"); gen != 4 || !present || err != nil {
		t.Fatalf("valid file: gen=%d present=%v err=%v", gen, present, err)
	}

	// Malformed contents report present=true with ErrInvalidGeneration.
	for _, raw := range []string{"", "0\n", "-3\n", "abc\n", "4\nextra\n"} {
		if err := os.WriteFile(s.memory.name("claude", "bad"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		gen, present, err := s.RecallGeneration("claude", "bad")
		if gen != 0 || !present || !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("malformed %q: gen=%d present=%v err=%v", raw, gen, present, err)
		}
	}

	// An unreadable entry (directory at the session's path) reports ErrInvalidGeneration.
	if err := os.Mkdir(s.memory.name("claude", "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if gen, present, err := s.RecallGeneration("claude", "dir"); gen != 0 || !present || !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("directory entry: gen=%d present=%v err=%v", gen, present, err)
	}
}

func TestAntigravityHookDeadlineRecordsAntigravityStage(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	origSync := syncAntigravityTemp
	syncAntigravityTemp = func(f *os.File) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return origSync(f)
	}
	t.Cleanup(func() {
		syncAntigravityTemp = origSync
	})

	fire := make(chan time.Time, 1)
	logged := make(chan struct{})
	execDone := make(chan struct{})
	var findOnce sync.Once
	r := Runner{
		ClientName: "antigravity",
		EventName:  "PreInvocation",
		Stdin:      strings.NewReader(`{"conversationId":"conv-stage","workspacePaths":["` + filepath.ToSlash(dir) + `"]}`),
		Getenv: func(k string) string {
			if k == "IRIS_TOKEN_FILE" {
				return token
			}
			return ""
		},
		PinnedConfigDir: func() (string, error) { return dir, nil },
		Ancestor:        func() (proc.Ancestor, error) { return proc.Ancestor{PID: 4242, StartMS: 99, Name: "agy"}, nil },
		Find: func(string) (gitinfo.Repo, error) {
			findOnce.Do(func() { close(execDone) })
			return gitinfo.Repo{}, errors.New("stop after antigravity")
		},
		after:    func(time.Duration) <-chan time.Time { return fire },
		logAfter: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
		logged:   func() { close(logged) },
	}
	done := make(chan Output, 1)
	go func() { done <- r.Run() }()
	select {
	case <-entered:
	case out := <-done:
		close(release)
		t.Fatalf("hook finished before entering antigravity state sync: %+v", out)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for antigravity state sync")
	}
	fire <- time.Now()
	out := <-done
	close(release)
	<-logged
	select {
	case <-execDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for execute goroutine to leave antigravity stage")
	}

	if out.Exit != 0 {
		t.Fatalf("deadline did not fail open: %+v", out)
	}
	failures, err := ReadRecentFailures(token, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Stage != "antigravity" || failures[0].Error != "hook deadline exceeded" {
		t.Fatalf("expected antigravity deadline stage, got %+v", failures)
	}
	if got := FailureStage(failures[0].Stage); got != "antigravity" {
		t.Fatalf("FailureStage(%q) = %q, want antigravity", failures[0].Stage, got)
	}
}
