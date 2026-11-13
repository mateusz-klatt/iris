package main

import (
	"context"
	"testing"

	"github.com/mateusz-klatt/iris/internal/wake"
)

// `iris watch --client kimi --once` is the exact command a model starts as a managed background task
// (ADR 030). Its outcome is its exit status, which Kimi reports in the task's notification: 0 only
// for attention, and the watcher's own code otherwise.
func TestWatchOnceExitsWithTheWatchersOutcome(t *testing.T) {
	origRun, origArgs := runKimiOnce, watchProcessArgs
	t.Cleanup(func() { runKimiOnce, watchProcessArgs = origRun, origArgs })
	once := []string{"watch", "--client", "kimi", "--once"}
	watchProcessArgs = func() []string { return once }
	for outcome, code := range map[wake.Outcome]int{
		wake.Attention: 0, wake.Disabled: 10, wake.Duplicate: 11, wake.Failed: 12, wake.Expired: 13, wake.Lost: 14, wake.NotCompleted: 15,
	} {
		runKimiOnce = func(ctx context.Context) wake.Outcome {
			if ctx.Err() != nil {
				t.Error("the watcher started with an ended context")
			}
			return outcome
		}
		if got := dispatch(once); got != code {
			t.Fatalf("outcome %d exited %d, want %d", outcome, got, code)
		}
	}
	if got := exitStatus(12).Error(); got != "exit status 12" {
		t.Fatalf("%q", got)
	}
}

// Any other spelling, or a process started with other arguments than the ones parsed, is a usage
// error, and the watcher never starts.
func TestWatchOnceAcceptsOnlyItsExactCommand(t *testing.T) {
	origRun, origArgs := runKimiOnce, watchProcessArgs
	t.Cleanup(func() { runKimiOnce, watchProcessArgs = origRun, origArgs })
	runKimiOnce = func(context.Context) wake.Outcome {
		t.Error("the watcher started")
		return wake.Attention
	}
	for _, args := range [][]string{
		{"watch", "--once", "--client", "kimi"},
		{"watch", "--client", "grok", "--once"},
		{"watch", "--client", "kimi", "--once", "--wake"},
		{"watch", "--client=kimi", "--once"},
		{"watch", "--client", "kimi", "--once=true"},
	} {
		watchProcessArgs = func() []string { return args }
		if got := dispatch(args); got != 1 {
			t.Fatalf("%v exited %d", args, got)
		}
	}
	watchProcessArgs = func() []string { return []string{"watch", "--client", "kimi", "--once", "extra"} }
	if got := dispatch([]string{"watch", "--client", "kimi", "--once"}); got != 1 {
		t.Fatalf("a process with other arguments exited %d", got)
	}
}

// The real watcher runs from the pinned configuration, which the tests' pinned directory lacks, so
// it fails with its own code.
func TestWatchOnceRunsTheRealWatcher(t *testing.T) {
	origArgs := watchProcessArgs
	t.Cleanup(func() { watchProcessArgs = origArgs })
	once := []string{"watch", "--client", "kimi", "--once"}
	watchProcessArgs = func() []string { return once }
	if got := dispatch(once); got != int(wake.Failed) {
		t.Fatalf("exited %d", got)
	}
}
