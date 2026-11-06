package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHookFailOpenOnBadJSON(t *testing.T) {
	var out, err bytes.Buffer
	code := runHookIO(
		[]string{"--client", "kimi", "--event", "PreToolUse"},
		strings.NewReader("{"),
		&out, &err,
		func(string) string { return "" },
		200*time.Millisecond,
	)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.Len() != 0 || err.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", out.String(), err.String())
	}
}
