package gitinfo

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// waiting is a real git that waits for its input until it is killed: hash-object reads stdin to
// its end, and the pipe's writing end stays open. The pipe is the operating system's, so git
// reads it directly and Wait has no copy of its own to wait for.
func waiting(t *testing.T, ctx context.Context) (cmd interface{ Wait() error }, finish func(), err error) {
	t.Helper()
	c := gitCommand(ctx, t.TempDir(), "hash-object", "--stdin")
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = w.Close() })
	c.Stdin = stdin
	finish, err = start(ctx, c)
	return c, finish, err
}

// After Stop, a git that would wait for ever ends within a bound. Once a Stopper has stopped, no
// git starts under it.
func TestAStopperStopsItsGitsAndStartsNoMore(t *testing.T) {
	ctx, stopper := WithStopper(context.Background())
	cmd, finish, err := waiting(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	stopper.Stop()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stopped git reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped git still runs")
	}
	finish()
	if len(stopper.running) != 0 {
		t.Fatalf("a finished git is still held: %d", len(stopper.running))
	}
	if _, _, err := waiting(t, ctx); !errors.Is(err, errStopped) {
		t.Fatalf("a git started after Stop: %v", err)
	}
}

// Stop and os/exec may both cancel a git, and only the first signals. Here os/exec cancels it,
// Wait reaps it, and Stop finds it still held, as when Stop takes the lock before finish can: a
// second signal then could reach another process group that took git's number.
func TestAGitIsSignalledOnceWhoeverCancelsIt(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, stopper := WithStopper(parent)
	c := gitCommand(ctx, t.TempDir(), "hash-object", "--stdin")
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = w.Close() })
	c.Stdin = stdin
	var signals atomic.Int32
	real := c.Cancel
	c.Cancel = func() error {
		signals.Add(1)
		return real()
	}
	finish, err := start(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := c.Wait(); err == nil {
		t.Fatal("a cancelled git reported success")
	}
	stopper.Stop()
	finish()
	if got := signals.Load(); got != 1 {
		t.Fatalf("git was signalled %d times", got)
	}
}

// A git that finishes is removed from the Stopper, so a later Stop does not signal it. A failed
// start returns an error and leaves nothing registered. Without a Stopper, git runs as before.
func TestAStopperLetsGoOfWhatEnded(t *testing.T) {
	ctx, stopper := WithStopper(context.Background())
	if err := run(ctx, gitCommand(ctx, t.TempDir(), "version")); err != nil {
		t.Fatal(err)
	}
	missing := gitCommand(ctx, t.TempDir(), "version")
	missing.Dir = missing.Dir + "/not here"
	if err := run(ctx, missing); err == nil {
		t.Fatal("a git in a directory that is not there started")
	}
	if len(stopper.running) != 0 {
		t.Fatalf("held %d gits after they ended", len(stopper.running))
	}
	stopper.Stop()
	if err := run(context.Background(), gitCommand(context.Background(), t.TempDir(), "version")); err != nil {
		t.Fatalf("without a Stopper: %v", err)
	}
}
