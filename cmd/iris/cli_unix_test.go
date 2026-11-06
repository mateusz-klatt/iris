//go:build !windows

package main

import (
	"os"
	"path/filepath"
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
	go func() {
		errCh <- run([]string{"serve", "--listen", "127.0.0.1:0", "--db", filepath.Join(dir, "live.db"), "--token-file", filepath.Join(dir, "live.token"), "--allowed-hosts", " iris.example:443, ,other.example:443", "--public-url", "https://iris.example"})
	}()
	select {
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestWatchStopsWhenTheProcessIsSignalled(t *testing.T) {
	errCh := make(chan error, 1)
	go func() { errCh <- runWatch([]string{"--client", "claude"}) }()
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop")
	}
}
