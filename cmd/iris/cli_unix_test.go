//go:build !windows

package main

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Windows has no way to send a process SIGTERM, so this stops serve only here.
func TestServeStopsOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))

	errCh := make(chan error, 1)
	_, stderr := capture(t, func() {
		go func() {
			errCh <- run([]string{"serve", "--listen", "127.0.0.1:0", "--db", filepath.Join(dir, "live.db"), "--token-file", filepath.Join(dir, "live.token"), "--allowed-hosts", " iris.example:443, ,other.example:443", "--public-url", "https://iris.example"})
		}()
		if err := signalUntilStopped(syscall.SIGTERM, errCh); err != nil {
			t.Fatal(err)
		}
	})
	// Port 0 proves the line reports the bound socket, not the requested address.
	if !strings.Contains(stderr, "Iris listening on 127.0.0.1:") || strings.Contains(stderr, "127.0.0.1:0\n") {
		t.Fatalf("%q", stderr)
	}
}

func TestWatchStopsWhenTheProcessIsSignalled(t *testing.T) {
	errCh := make(chan error, 1)
	go func() { errCh <- runWatch([]string{"--client", "claude"}) }()
	if err := signalUntilStopped(syscall.SIGINT, errCh); err != nil {
		t.Fatal(err)
	}
}

// signalUntilStopped sends this process sig until the command reporting on done stops, and
// returns what it returned. The test holds its own handler for sig meanwhile: a signal sent
// before the command has installed its handler would otherwise take the default action and
// end the whole test binary with "signal: terminated" and no failing test named, as it did on
// a slow CI runner when the test waited a fixed 300 ms instead.
func signalUntilStopped(sig syscall.Signal, done <-chan error) error {
	held := make(chan os.Signal, 8)
	signal.Notify(held, sig)
	defer signal.Stop(held)
	deadline := time.After(10 * time.Second)
	for {
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			return err
		}
		select {
		case err := <-done:
			return err
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			return errStopped
		}
	}
}

var errStopped = errors.New("the command did not stop on the signal")
