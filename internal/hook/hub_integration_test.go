package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestCapturedEditsAgainstRealHub(t *testing.T) {
	// Every client has a captured edit fixture; no synthetic tool payload is needed.
	cases := []struct {
		client               Client
		fixture, event, path string
	}{
		{ClientClaude, "claude/PreToolUse-write.json", "PreToolUse", "allowed.txt"},
		{ClientCodex, "codex/PreToolUse-apply_patch.json", "PreToolUse", "allowed.txt"},
		{ClientCopilot, "copilot/preToolUse-apply_patch.json", "preToolUse", "allowed.txt"},
		{ClientCursor, "cursor/preToolUse.json", "preToolUse", "fixtures/deny-target.txt"},
		{ClientAntigravity, "antigravity/pre_tool_use.json", "pre_tool_use", "test_edit.txt"},
		{ClientGrok, "grok/PreToolUse.json", "PreToolUse", "note.txt"},
		{ClientKimi, "kimi/pre_tool_use_write_denied.json", "PreToolUse", "deny-me.txt"},
	}
	for _, tc := range cases {
		t.Run(string(tc.client), func(t *testing.T) {
			for _, conflict := range []bool{true, false} {
				name := "unclaimed"
				if conflict {
					name = "conflict"
				}
				t.Run(name, func(t *testing.T) {
					root := t.TempDir()
					cmd := exec.Command("git", "init", "--initial-branch=main", root)
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git init: %v: %s", err, output)
					}
					target := filepath.Join(root, filepath.FromSlash(tc.path))
					if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, []byte("before\n"), 0600); err != nil {
						t.Fatal(err)
					}
					raw, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
					if err != nil {
						t.Fatal(err)
					}
					captured, err := Decode(tc.client, tc.event, raw)
					if err != nil {
						t.Fatal(err)
					}
					var payload any
					if err = json.Unmarshal(raw, &payload); err != nil {
						t.Fatal(err)
					}
					payload = rewriteCapturedPaths(payload, captured.CWD, filepath.ToSlash(root))
					raw, err = json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}

					database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = database.Close() }()
					server := httptest.NewUnstartedServer(nil)
					server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
					server.Start()
					defer server.Close()
					token := tokenFile(t)
					getenv := func(key string) string {
						switch key {
						case "IRIS_TOKEN_FILE":
							return token
						case "IRIS_HUB_URL":
							return server.URL
						}
						return ""
					}
					client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}}
					// Setup against the real hub is not timed. The test binary's own timeout bounds a hang.
					ctx := context.Background()
					otherClient := "codex"
					if tc.client == ClientCodex {
						otherClient = "claude"
					}
					holder, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: otherClient, SessionID: "holder", Host: "testhost", CWD: root, Worktree: root, Source: "hook"})
					if err != nil {
						t.Fatal(err)
					}
					if conflict {
						if err = client.Do(ctx, "POST", "/v1/claims", map[string]any{"execution_id": holder.ExecutionID, "generation": holder.Generation, "key": "hold", "kind": "exact", "path": tc.path}, nil); err != nil {
							t.Fatal(err)
						}
					}

					out := (Runner{ClientName: string(tc.client), EventName: tc.event, Stdin: bytes.NewReader(raw), Getenv: getenv, Hub: client, Host: func() string { return "testhost" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }, after: never, logAfter: never}).Run()
					var listed struct {
						Claims []struct{ Path, Kind, Origin, Holder string }
					}
					if err = client.Do(ctx, "GET", "/v1/claims?repository_id="+url.QueryEscape(holder.RepositoryID), nil, &listed); err != nil {
						t.Fatal(err)
					}
					if len(listed.Claims) != 1 {
						t.Fatalf("claims after hook: %+v; stdout=%s stderr=%s", listed, out.Stdout, out.Stderr)
					}
					claim := listed.Claims[0]
					if claim.Path != tc.path || claim.Kind != "exact" {
						t.Fatal(claim)
					}
					if conflict {
						assertRealDeny(t, tc.client, out, tc.path, holder.Agent.Name)
						if claim.Origin != "explicit" || claim.Holder != holder.Agent.Name {
							t.Fatalf("conflict created an auto claim: %+v", claim)
						}
					} else {
						assertAllow(t, tc.client, out)
						if len(out.Stderr) != 0 {
							t.Fatalf("successful hook stderr: %s", out.Stderr)
						}
						if claim.Origin != "auto" || claim.Holder == holder.Agent.Name {
							t.Fatalf("edit did not take its own auto claim: %+v", claim)
						}
					}
					failures, err := ReadRecentFailures(token, 20)
					if err != nil || len(failures) != 0 {
						t.Fatalf("successful hook logged failures: %+v %v", failures, err)
					}
				})
			}
		})
	}
}

// Substitute paths only in memory. Timestamp, tool IDs, casing, argument encoding
// and all other captured fields remain as supplied by the real client.
func rewriteCapturedPaths(value any, oldRoot, newRoot string) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, oldRoot, newRoot)
	case []any:
		for i, item := range v {
			v[i] = rewriteCapturedPaths(item, oldRoot, newRoot)
		}
	case map[string]any:
		for k, item := range v {
			v[k] = rewriteCapturedPaths(item, oldRoot, newRoot)
		}
	}
	return value
}
func assertRealDeny(t *testing.T, client Client, out Output, path, holder string) {
	t.Helper()
	wantExit := 0
	if client == ClientKimi || client == ClientGrok {
		wantExit = 2
	}
	if out.Exit != wantExit {
		t.Fatalf("deny exit=%d want=%d stdout=%s stderr=%s", out.Exit, wantExit, out.Stdout, out.Stderr)
	}
	if client == ClientKimi {
		if len(out.Stdout) != 0 || !strings.Contains(string(out.Stderr), path) || !strings.Contains(string(out.Stderr), holder) {
			t.Fatalf("Kimi deny: stdout=%s stderr=%s", out.Stdout, out.Stderr)
		}
		return
	}
	var body map[string]any
	if err := json.Unmarshal(out.Stdout, &body); err != nil {
		t.Fatal(err)
	}
	key := "decision"
	switch client {
	case ClientClaude, ClientCodex:
		nested, ok := body["hookSpecificOutput"].(map[string]any)
		if !ok || nested["hookEventName"] != "PreToolUse" {
			t.Fatal(body)
		}
		body = nested
		key = "permissionDecision"
	case ClientCopilot:
		key = "permissionDecision"
	case ClientCursor:
		key = "permission"
	}
	if body[key] != "deny" || !strings.Contains(string(out.Stdout), path) || !strings.Contains(string(out.Stdout), holder) {
		t.Fatalf("%s real deny missing: %s", client, out.Stdout)
	}
}
