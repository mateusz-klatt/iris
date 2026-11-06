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
