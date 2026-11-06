package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
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

// One lost packet must not cost an edit its claim. The first request never answers,
// so only the second can make this hook finish without failing open.
func TestAStalledRequestDoesNotCostTheEditItsClaim(t *testing.T) {
	var calls, touches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions" && calls.Add(1) == 1 {
			// The server notices a closed connection only after the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/v1/claims/touch" {
			touches.Add(1)
			_, _ = w.Write([]byte(`{"action":"allow"}`))
			return
		}
		_, _ = w.Write([]byte(`{"execution_id":"ex","generation":1}`))
	}))
	defer srv.Close()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"session_id": "s", "cwd": repo, "tool_name": "Edit", "tool_use_id": "t", "tool_input": map[string]string{"file_path": filepath.Join(repo, "a.go")}})
	if err != nil {
		t.Fatal(err)
	}
	var out, errText bytes.Buffer
	code := runHookIO([]string{"--client", "claude", "--event", "PreToolUse"}, bytes.NewReader(payload), &out, &errText, func(key string) string {
		switch key {
		case "IRIS_HUB_URL":
			return srv.URL
		case "IRIS_TOKEN_FILE":
			return token
		}
		return ""
	}, hook.DefaultDeadline)
	if code != 0 || touches.Load() != 1 {
		t.Fatalf("code %d touches %d stdout %q stderr %q", code, touches.Load(), out.String(), errText.String())
	}
}
