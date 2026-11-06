//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStalledClientConfigCannotOutliveTheHookDeadline(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "client.json")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "hub.token")
	done := make(chan int, 1)
	go func() {
		var out, err bytes.Buffer
		done <- runHookIO(
			[]string{"--client", "claude", "--event", "SessionStart"},
			strings.NewReader(`{"session_id":"s","cwd":"/workspace/project","hook_event_name":"SessionStart"}`),
			&out, &err,
			func(key string) string {
				if key == "IRIS_TOKEN_FILE" {
					return token
				}
				return ""
			},
			80*time.Millisecond,
		)
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stalled client.json escaped the hook deadline")
	}
	// Release the reader that the deadline abandoned.
	if w, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
		_, _ = w.Write([]byte("{}"))
		_ = w.Close()
	}
}
