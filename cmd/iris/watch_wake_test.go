package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWatchWakeRequiresExactAntigravityArgvBeforeLaunching(t *testing.T) {
	origRun := runAntigravityWake
	origArgs := watchProcessArgs
	t.Cleanup(func() {
		runAntigravityWake = origRun
		watchProcessArgs = origArgs
	})

	var calls atomic.Int32
	runAntigravityWake = func() error {
		calls.Add(1)
		return nil
	}

	// Default watchProcessArgs inspects real os.Args[1:], which under go test are test flags and fail closed.
	if err := runWatch([]string{"--client", "antigravity", "--wake"}); err == nil {
		t.Fatal("accepted go test os.Args[1:]")
	}

	watchProcessArgs = func() []string {
		return []string{"watch", "--client", "antigravity", "--wake"}
	}

	for _, bad := range [][]string{
		{"--client", "claude", "--wake"},
		{"--client", "agy", "--wake"},
		{"--wake", "--client", "antigravity"},
		{"--client=antigravity", "--wake"},
		{"--client", "antigravity", "--wake=true"},
		{"--client", "antigravity", "--wake=false"},
		{"--client", "antigravity", "--wake", "extra"},
		{"--client", "antigravity", "--unknown"},
	} {
		if err := runWatch(bad); err == nil {
			t.Fatalf("accepted invalid watch wake args: %v", bad)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid watch wake args called runner %d times", calls.Load())
	}

	// Process argv mismatch (e.g. wrapper injecting extra top-level flags) also fails closed.
	watchProcessArgs = func() []string {
		return []string{"watch", "--client", "antigravity", "--wake", "--extra"}
	}
	if err := runWatch([]string{"--client", "antigravity", "--wake"}); err == nil {
		t.Fatal("accepted mismatched os.Args[1:]")
	}
	if calls.Load() != 0 {
		t.Fatal("mismatched os.Args[1:] called runner")
	}

	// Exact argv invokes runAntigravityWake and propagates its result.
	watchProcessArgs = func() []string {
		return []string{"watch", "--client", "antigravity", "--wake"}
	}
	if err := runWatch([]string{"--client", "antigravity", "--wake"}); err != nil || calls.Load() != 1 {
		t.Fatalf("exact watch wake = %v, calls=%d", err, calls.Load())
	}

	runAntigravityWake = func() error { return errors.New("preflight refused") }
	if err := runWatch([]string{"--client", "antigravity", "--wake"}); err == nil || !strings.Contains(err.Error(), "preflight refused") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}
