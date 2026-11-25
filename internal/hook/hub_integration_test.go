package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestTouchRetryKeysAreNamespacedPerConversation(t *testing.T) {
	cases := []struct {
		client                Client
		fixture, event, field string
		call, path            string
	}{
		{client: ClientAntigravity, fixture: "antigravity/pre_tool_use.json", event: "pre_tool_use", field: "conversationId", call: "3", path: "test_edit.txt"},
		{client: ClientKimi, fixture: "kimi/pre_tool_use_write_denied.json", event: "PreToolUse", field: "session_id", call: "Write_0_a89dd82f", path: "deny-me.txt"},
	}
	for _, tc := range cases {
		t.Run(string(tc.client), func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command("git", "init", "--initial-branch=main", root)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
			if err := os.WriteFile(filepath.Join(root, tc.path), []byte("before\n"), 0600); err != nil {
				t.Fatal(err)
			}
			capturedRaw, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			captured, err := Decode(tc.client, tc.event, capturedRaw)
			if err != nil {
				t.Fatal(err)
			}
			if captured.ToolCallID != tc.call {
				t.Fatalf("captured tool call ID = %q, want %q", captured.ToolCallID, tc.call)
			}
			payload := func(conversation string) []byte {
				var value any
				if err := json.Unmarshal(capturedRaw, &value); err != nil {
					t.Fatal(err)
				}
				value = rewriteCapturedPaths(value, captured.CWD, filepath.ToSlash(root))
				value.(map[string]any)[tc.field] = conversation
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}

			database, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = database.Close() }()
			keys := make(chan string, 4)
			var realHub http.Handler
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && (r.URL.Path == "/v1/claims/touch" || r.URL.Path == "/v1/claims/touch/files") {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read touch: %v", err)
						http.Error(w, "read failed", http.StatusBadRequest)
						return
					}
					var request struct {
						ToolCallID string `json:"tool_call_id"`
					}
					if err := json.Unmarshal(raw, &request); err != nil {
						t.Errorf("decode touch: %v", err)
						http.Error(w, "decode failed", http.StatusBadRequest)
						return
					}
					keys <- request.ToolCallID
					r.Body = io.NopCloser(bytes.NewReader(raw))
				}
				realHub.ServeHTTP(w, r)
			}))
			realHub = hub.New(database, "secret", []string{server.Listener.Addr().String()})
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
			runHook := func(raw []byte) Output {
				return Runner{
					ClientName: string(tc.client), EventName: tc.event, Stdin: bytes.NewReader(raw),
					Getenv: getenv, Hub: client, Host: func() string { return "testhost" },
					Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
					after:    never, logAfter: never,
				}.Run()
			}
			nextKey := func() string {
				t.Helper()
				select {
				case key := <-keys:
					return key
				case <-time.After(10 * time.Second):
					t.Fatal("real hub did not receive a touch")
					return ""
				}
			}
			ctx := context.Background()
			bind := func(conversation string) hubclient.SessionResponse {
				t.Helper()
				var bound hubclient.SessionResponse
				if err := client.Do(ctx, http.MethodPost, "/v1/bind", map[string]any{
					"client": tc.client, "host": "testhost", "session_id": conversation,
				}, &bound); err != nil {
					t.Fatal(err)
				}
				return bound
			}
			assertGranted := func(bound hubclient.SessionResponse) {
				t.Helper()
				var listed struct {
					Claims []struct{ Path, Origin string }
				}
				if err := client.Do(ctx, http.MethodGet, "/v1/claims?repository_id="+url.QueryEscape(bound.RepositoryID), nil, &listed); err != nil {
					t.Fatal(err)
				}
				if len(listed.Claims) != 1 || listed.Claims[0].Path != tc.path || listed.Claims[0].Origin != "auto" {
					t.Fatalf("claims after grant = %+v", listed.Claims)
				}
			}

			conversations := []string{"conversation-one", "conversation-two"}
			gotKeys := make([]string, 0, len(conversations))
			var reusedAgent string
			for i, conversation := range conversations {
				raw := payload(conversation)
				assertAllow(t, tc.client, runHook(raw))
				firstKey := nextKey()
				wantKey := namespaceToolCallID(Event{Client: tc.client, ConversationID: conversation, ToolCallID: tc.call, NamespaceVerified: true})
				if firstKey != wantKey || len(firstKey) != 64 || strings.Contains(firstKey, conversation) {
					t.Fatalf("touch key = %q, want %q", firstKey, wantKey)
				}
				assertAllow(t, tc.client, runHook(raw))
				if retryKey := nextKey(); retryKey != firstKey {
					t.Fatalf("same-conversation retry key = %q, first = %q", retryKey, firstKey)
				}
				gotKeys = append(gotKeys, firstKey)
				bound := bind(conversation)
				assertGranted(bound)
				if i == 0 {
					reusedAgent = bound.Agent.ID
					if err := client.EndExecution(ctx, bound.ExecutionID, bound.Generation); err != nil {
						t.Fatal(err)
					}
				} else if bound.Agent.ID != reusedAgent {
					t.Fatalf("second Agent = %q, want reused %q", bound.Agent.ID, reusedAgent)
				}
			}
			if gotKeys[0] == gotKeys[1] {
				t.Fatalf("two conversations reused key %q", gotKeys[0])
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

func TestTouchReplayRefusalAgainstRealHub(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--initial-branch=main", root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	for _, name := range []string{"allowed.txt", "other.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("before\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "claude", "PreToolUse-write.json"))
	if err != nil {
		t.Fatal(err)
	}
	captured, err := Decode(ClientClaude, "PreToolUse", raw)
	if err != nil {
		t.Fatal(err)
	}
	marshalPayload := func(from, to string) []byte {
		var payload any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		payload = rewriteCapturedPaths(payload, captured.CWD, filepath.ToSlash(root))
		if from != "" {
			payload = rewriteCapturedPaths(payload, from, to)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	origRaw := marshalPayload("", "")
	changedPathRaw := marshalPayload("allowed.txt", "other.txt")
	freshRaw := marshalPayload(captured.ToolCallID, captured.ToolCallID+"_fresh")
	conflictRaw := marshalPayload("allowed.txt", "other.txt")
	{
		var payload any
		if err := json.Unmarshal(conflictRaw, &payload); err != nil {
			t.Fatal(err)
		}
		conflictRaw, err = json.Marshal(rewriteCapturedPaths(payload, captured.ToolCallID, captured.ToolCallID+"_conflict"))
		if err != nil {
			t.Fatal(err)
		}
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
	runHook := func(stdin []byte) Output {
		return Runner{
			ClientName: string(ClientClaude),
			EventName:  "PreToolUse",
			Stdin:      bytes.NewReader(stdin),
			Getenv:     getenv,
			Hub:        client,
			Host:       func() string { return "testhost" },
			Ancestor:   func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
			after:      never,
			logAfter:   never,
		}.Run()
	}

	// 1. Grant an edit on allowed.txt.
	out := runHook(origRaw)
	assertAllow(t, ClientClaude, out)
	if len(out.Stderr) != 0 {
		t.Fatalf("grant stderr: %s", out.Stderr)
	}

	ctx := context.Background()
	var bound struct {
		ExecutionID  string `json:"execution_id"`
		Generation   int    `json:"generation"`
		RepositoryID string `json:"repository_id"`
	}
	if err := client.Do(ctx, "POST", "/v1/bind", map[string]any{
		"client": "claude", "host": "testhost", "session_id": captured.SessionID,
	}, &bound); err != nil {
		t.Fatal(err)
	}
	listClaims := func() []struct {
		Fence  int64  `json:"fence"`
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Origin string `json:"origin"`
		Holder string `json:"holder"`
	} {
		var listed struct {
			Claims []struct {
				Fence  int64  `json:"fence"`
				Path   string `json:"path"`
				Kind   string `json:"kind"`
				Origin string `json:"origin"`
				Holder string `json:"holder"`
			} `json:"claims"`
		}
		if err := client.Do(ctx, "GET", "/v1/claims?repository_id="+url.QueryEscape(bound.RepositoryID), nil, &listed); err != nil {
			t.Fatal(err)
		}
		return listed.Claims
	}
	claims := listClaims()
	if len(claims) != 1 || claims[0].Path != "allowed.txt" || claims[0].Origin != "auto" {
		t.Fatalf("claims after grant = %+v", claims)
	}
	grantFence := claims[0].Fence

	// 2. Replay the exact edit unchanged: allowed.
	out = runHook(origRaw)
	assertAllow(t, ClientClaude, out)
	if claims := listClaims(); len(claims) != 1 || claims[0].Fence != grantFence {
		t.Fatalf("claims after replay = %+v", claims)
	}
	if failures, err := ReadRecentFailures(token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("failures after replay = %+v %v", failures, err)
	}
	if obs, err := ReadRecentObservations(token, 10); err != nil || len(obs) != 0 {
		t.Fatalf("observations after replay = %+v %v", obs, err)
	}

	// 3. Change the path under the same tool-call ID: key_reused and deny.
	out = runHook(changedPathRaw)
	wantKeyReused := Encode(Event{Client: ClientClaude, Kind: KindPreTool}, Decision{Action: ActionDeny, Reason: touchKeyReusedRefused})
	if out.Exit != wantKeyReused.Exit || string(out.Stdout) != string(wantKeyReused.Stdout) || len(out.Stderr) != 0 {
		t.Fatalf("key_reused output stdout=%q stderr=%q exit=%d; want stdout=%q exit=%d",
			out.Stdout, out.Stderr, out.Exit, wantKeyReused.Stdout, wantKeyReused.Exit)
	}
	if claims := listClaims(); len(claims) != 1 || claims[0].Path != "allowed.txt" {
		t.Fatalf("key_reused mutated claims = %+v", claims)
	}
	obs := observedOnly(t, token)
	if obs.Error != obsTouchKeyReused {
		t.Fatalf("observation = %+v, want %q", obs, obsTouchKeyReused)
	}

	// 4. Release the grant explicitly, replay the original: lost and deny.
	if err := client.Do(ctx, "POST", "/v1/claims/release", map[string]any{
		"execution_id": bound.ExecutionID, "generation": bound.Generation, "fence": grantFence,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if claims := listClaims(); len(claims) != 0 {
		t.Fatalf("claims after release = %+v", claims)
	}
	out = runHook(origRaw)
	wantLost := Encode(Event{Client: ClientClaude, Kind: KindPreTool}, Decision{Action: ActionDeny, Reason: touchLostRefused})
	if out.Exit != wantLost.Exit || string(out.Stdout) != string(wantLost.Stdout) || len(out.Stderr) != 0 {
		t.Fatalf("lost output stdout=%q stderr=%q exit=%d; want stdout=%q exit=%d",
			out.Stdout, out.Stderr, out.Exit, wantLost.Stdout, wantLost.Exit)
	}
	if claims := listClaims(); len(claims) != 0 {
		t.Fatalf("lost replay resurrected claim = %+v", claims)
	}
	if failures, err := ReadRecentFailures(token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("failures after lost = %+v %v", failures, err)
	}
	observations, err := ReadRecentObservations(token, 10)
	if err != nil || len(observations) != 2 || observations[0].Error != obsTouchKeyReused || observations[1].Error != obsTouchLost {
		t.Fatalf("observations = %+v %v", observations, err)
	}

	// 5. Fresh tool-call IDs get normal arbitration (grant when unclaimed, conflict deny when held).
	out = runHook(freshRaw)
	assertAllow(t, ClientClaude, out)
	if claims := listClaims(); len(claims) != 1 || claims[0].Path != "allowed.txt" || claims[0].Origin != "auto" {
		t.Fatalf("fresh allow claims = %+v", claims)
	}

	holder, err := client.RegisterSession(ctx, hubclient.SessionRequest{Client: "codex", SessionID: "holder", Host: "testhost", CWD: root, Worktree: root, Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Do(ctx, "POST", "/v1/claims", map[string]any{
		"execution_id": holder.ExecutionID, "generation": holder.Generation, "key": "hold", "kind": "exact", "path": "other.txt",
	}, nil); err != nil {
		t.Fatal(err)
	}
	out = runHook(conflictRaw)
	assertRealDeny(t, ClientClaude, out, "other.txt", holder.Agent.Name)
	if failures, err := ReadRecentFailures(token, 10); err != nil || len(failures) != 0 {
		t.Fatalf("failures after fresh arbitration = %+v %v", failures, err)
	}
	if observations, err := ReadRecentObservations(token, 10); err != nil || len(observations) != 2 {
		t.Fatalf("fresh conflict changed observations = %+v %v", observations, err)
	}
}
