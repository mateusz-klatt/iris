package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
