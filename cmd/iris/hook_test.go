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

func TestHookFailOpenOnBadJSON(t *testing.T) {
	var out, err bytes.Buffer
	token := filepath.Join(t.TempDir(), "token")
	code := runHookIO(
		[]string{"--client", "kimi", "--event", "PreToolUse"},
		strings.NewReader("{"),
		&out, &err,
		func(key string) string {
			if key == "IRIS_TOKEN_FILE" {
				return token
			}
			return ""
		},
		200*time.Millisecond,
	)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.Len() != 0 || err.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", out.String(), err.String())
	}
}

func TestClientFromArgsRecoversTheNameTheFlagParserDropped(t *testing.T) {
	if got := clientFromArgs([]string{"--", "--client", "grok"}); got != "grok" {
		t.Fatal(got)
	}
	if got := clientFromArgs([]string{"--client=codex"}); got != "codex" {
		t.Fatal(got)
	}
	if got := clientFromArgs([]string{"--event", "Stop"}); got != "" {
		t.Fatal(got)
	}
	var out, err bytes.Buffer
	code := runHookIO([]string{"--", "--client", "kimi", "--event", "PreToolUse"}, strings.NewReader("{"), &out, &err, func(string) string { return "" }, 200*time.Millisecond)
	if code != 0 || out.Len() != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), err.String())
	}
}

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
