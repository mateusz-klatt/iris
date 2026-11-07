package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestFailOpenEveryClient(t *testing.T) {
	for _, c := range Clients {
		t.Run(string(c)+"/panic", func(t *testing.T) {
			out := Runner{
				ClientName: string(c),
				EventName:  "PreToolUse",
				Getenv:     failureEnv(t),
				Stdin:      strings.NewReader(editPayload(c, t.TempDir())),
				Find: func(string) (gitinfo.Repo, error) {
					panic("boom")
				},
				Deadline: 200 * time.Millisecond,
			}.Run()
			assertAllow(t, c, out)
		})
		t.Run(string(c)+"/timeout", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				time.Sleep(2 * time.Second)
			}))
			t.Cleanup(func() { srv.CloseClientConnections() })
			start := time.Now()
			out := editRunner(t, c, srv.URL, 40*time.Millisecond).Run()
			if time.Since(start) > 500*time.Millisecond {
				t.Fatalf("deadline missed after %s", time.Since(start))
			}
			assertAllow(t, c, out)
		})
		t.Run(string(c)+"/unreachable", func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			out := editRunner(t, c, "http://"+addr, time.Second).Run()
			assertAllow(t, c, out)
		})
	}
}

func TestEndToEndFakeHub(t *testing.T) {
	// The subtests assert the decision each client receives, not the hook's budget, so the
	// deadline only bounds a hang. Under full CPU load on Windows the repository stage alone
	// passed one second, and the hook rightly failed open.
	deadline := time.Minute
	var mu sync.Mutex
	var sessions []map[string]any
	var touches []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode %s: %v", r.URL.Path, err)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth %s", r.Header.Get("Authorization"))
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/sessions":
			sessions = append(sessions, body)
			_, _ = fmt.Fprintf(w, `{"execution_id":"ex_1","generation":3,"handle":"ex_1","agent":{"name":"claude-host"},"peers":[{"name":"codex-host","client":"codex","state":"active","branch":"main"}],"claims":[{"path":"src/a.go","holder":"codex-host","origin":"auto","expires_at":"14:32"}],"unread":2}`)
		case "/v1/claims/touch":
			touches = append(touches, body)
			_, _ = fmt.Fprint(w, `{"action":"deny","message":"src/a.go is held by codex-host"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	root := gitRepo(t)
	for _, c := range Clients {
		t.Run(string(c), func(t *testing.T) {
			mu.Lock()
			beforeS, beforeT := len(sessions), len(touches)
			mu.Unlock()
			out := Runner{
				ClientName: string(c),
				EventName:  "PreToolUse",
				Stdin:      strings.NewReader(editPayload(c, root)),
				Getenv: func(k string) string {
					switch k {
					case "IRIS_HUB_URL":
						return srv.URL
					case "IRIS_TOKEN_FILE":
						return tokenFile(t)
					default:
						return ""
					}
				},
				Hub:      &hubclient.Client{Config: hubURL(srv.URL)},
				Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{PID: 42, StartMS: 99}, nil },
				Host:     func() string { return "testhost" },
				Deadline: deadline,
			}.Run()
			if !denied(c, out) {
				t.Fatalf("deny output stdout %q stderr %q exit %d", out.Stdout, out.Stderr, out.Exit)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(sessions) != beforeS+1 || len(touches) != beforeT+1 {
				t.Fatalf("sessions %d touches %d", len(sessions)-beforeS, len(touches)-beforeT)
			}
			touch := touches[len(touches)-1]
			paths, _ := touch["paths"].([]any)
			if len(paths) != 1 || paths[0] != "src/a.go" {
				t.Fatalf("paths %#v", touch["paths"])
			}
			wantCall := "t1"
			if c == ClientCopilot {
				hash := sha256.Sum256([]byte(editPayload(c, root)))
				wantCall = "payload:" + hex.EncodeToString(hash[:])
			}
			if c == ClientAntigravity {
				wantCall = "1"
			}
			if touch["execution_id"] != "ex_1" || touch["tool_call_id"] != wantCall {
				t.Fatalf("touch %#v", touch)
			}
			sess := sessions[len(sessions)-1]
			if sess["client"] != string(c) || sess["session_id"] != "s1" || sess["source"] != "hook" {
				t.Fatalf("session %#v", sess)
			}
			if sess["worktree"] != root || sess["branch"] != "main" {
				t.Fatalf("repo %#v", sess)
			}
		})
	}

	t.Run("session-start", func(t *testing.T) {
		out := Runner{
			ClientName: "claude",
			EventName:  "SessionStart",
			Stdin:      strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"SessionStart"}`, root)),
			Hub:        &hubclient.Client{Config: hubURL(srv.URL)},
			Deadline:   deadline,
		}.Run()
		if out.Exit != 0 || !strings.Contains(string(out.Stdout), "Iris handle: ex_1") || !strings.Contains(string(out.Stdout), "untrusted data") {
			t.Fatalf("%s", out.Stdout)
		}
	})

	t.Run("ask-on-codex", func(t *testing.T) {
		askSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/claims/touch" {
				_, _ = fmt.Fprint(w, `{"action":"ask","message":"conflict"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"execution_id":"ex_1","generation":1,"handle":"ex_1"}`)
		}))
		defer askSrv.Close()
		out := Runner{
			ClientName: "codex",
			EventName:  "PreToolUse",
			Stdin:      strings.NewReader(editPayload(ClientCodex, root)),
			Hub:        &hubclient.Client{Config: hubURL(askSrv.URL)},
			Deadline:   deadline,
		}.Run()
		if out.Exit != 0 || !strings.Contains(string(out.Stdout), "permissionDecision\":\"deny") || !strings.Contains(string(out.Stdout), "Ask the human") {
			t.Fatalf("%s", out.Stdout)
		}
	})
}

func TestNoNetworkForHarmlessTools(t *testing.T) {
	root := gitRepo(t)
	var calls int
	hub := &countingHub{fn: func() { calls++ }}
	cases := []struct {
		name, event, body string
	}{
		{"read", "PreToolUse", fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":%q}}`, root, filepath.Join(root, "src/a.go"))},
		{"shell", "PreToolUse", fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"pwd"}}`, root)},
		{"outside", "SessionStart", `{"session_id":"s1","cwd":"/definitely/not/a/repo","hook_event_name":"SessionStart"}`},
		{"escape", "PreToolUse", fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"../secret"}}`, root)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			out := Runner{
				ClientName: "claude",
				EventName:  tc.event,
				Stdin:      strings.NewReader(tc.body),
				Hub:        hub,
				Deadline:   time.Second,
			}.Run()
			if calls != 0 || out.Exit != 0 {
				t.Fatalf("calls %d exit %d", calls, out.Exit)
			}
		})
	}
}

func TestCrossLoad(t *testing.T) {
	root := gitRepo(t)
	var calls int
	patch := "*** Begin Patch\n*** Add File: src/a.go\n+x\n*** End Patch\n"
	body := fmt.Sprintf(`{"sessionId":"s1","cwd":%q,"toolName":"apply_patch","toolArgs":%q}`, root, patch)
	out := Runner{
		ClientName: "copilot",
		EventName:  "preToolUse",
		Stdin:      strings.NewReader(body),
		Getenv: func(k string) string {
			if k == "ANTIGRAVITY_CONVERSATION_ID" {
				return "s1"
			}
			if k == "CLAUDE_CODE_SESSION_ID" {
				return "s1"
			}
			return ""
		},
		Hub:      &countingHub{fn: func() { calls++ }},
		Deadline: time.Second,
	}.Run()
	if calls != 0 {
		t.Fatal("cross-load called the hub")
	}
	if string(out.Stdout) != "{\"decision\":\"allow\"}\n" || out.Exit != 0 {
		t.Fatalf("runner allow %q exit %d", out.Stdout, out.Exit)
	}

	calls = 0
	out = Runner{
		ClientName: "copilot",
		EventName:  "preToolUse",
		Stdin:      strings.NewReader(body),
		Getenv: func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ID" || k == "COPILOT_AGENT_SESSION_ID" {
				return "s1"
			}
			return ""
		},
		Hub:      denyHub{calls: &calls},
		Deadline: time.Second,
	}.Run()
	if calls == 0 {
		t.Fatal("matching copilot session should call the hub; inherited claude id is ignored")
	}
	if out.Exit != 0 || !strings.Contains(string(out.Stdout), `"permissionDecision":"deny"`) {
		t.Fatalf("%q", out.Stdout)
	}
}

func TestUnknownToolReported(t *testing.T) {
	payload, err := os.ReadFile("testdata/copilot/preToolUse-iris_probe_probe.json")
	if err != nil {
		t.Fatal(err)
	}
	out := Runner{
		ClientName: "copilot",
		EventName:  "preToolUse",
		Stdin:      strings.NewReader(string(payload)),
		Deadline:   time.Second,
	}.Run()
	if string(out.Stderr) != "iris: unknown tool iris-probe-probe\n" || out.Exit != 0 {
		t.Fatalf("stdout %q stderr %q", out.Stdout, out.Stderr)
	}
}

func TestDefaultDeadline(t *testing.T) {
	if DefaultDeadline != 1500*time.Millisecond {
		t.Fatal(DefaultDeadline)
	}
}

func editRunner(t *testing.T, c Client, url string, deadline time.Duration) Runner {
	t.Helper()
	root := gitRepo(t)
	return Runner{
		ClientName: string(c),
		EventName:  "PreToolUse",
		Stdin:      strings.NewReader(editPayload(c, root)),
		Hub:        &hubclient.Client{Config: hubURL(url)},
		Getenv:     failureEnv(t),
		Deadline:   deadline,
	}
}

func editPayload(c Client, root string) string {
	file := filepath.Join(root, "src", "a.go")
	switch c {
	case ClientCopilot:
		return fmt.Sprintf(`{"sessionId":"s1","cwd":%q,"toolName":"apply_patch","toolArgs":%q}`, root, "*** Begin Patch\n*** Add File: src/a.go\n+x\n*** End Patch\n")
	case ClientCursor:
		return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"workspace_roots":[%q],"hook_event_name":"preToolUse","tool_name":"Write","tool_input":{"file_path":%q},"tool_use_id":"t1"}`, root, root, file)
	case ClientAntigravity:
		return fmt.Sprintf(`{"conversationId":"s1","workspacePaths":[%q],"stepIdx":1,"toolCall":{"name":"write_file","args":{"Path":%q}}}`, root, file)
	case ClientKimi:
		return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"path":"src/a.go"},"tool_call_id":"t1"}`, root)
	case ClientGrok:
		return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"write","tool_input":{"file_path":%q},"tool_use_id":"t1"}`, root, file)
	default:
		return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":%q},"tool_use_id":"t1"}`, root, file)
	}
}

func assertAllow(t *testing.T, c Client, out Output) {
	t.Helper()
	want := FallbackAllow(c)
	if out.Exit != want.Exit || string(out.Stdout) != string(want.Stdout) {
		t.Fatalf("got stdout %q stderr %q exit %d; want stdout %q exit %d", out.Stdout, out.Stderr, out.Exit, want.Stdout, want.Exit)
	}
}

func denied(c Client, out Output) bool {
	switch c {
	case ClientKimi:
		return out.Exit == 2 && strings.Contains(string(out.Stderr), "codex-host")
	case ClientGrok:
		return out.Exit == 2 && strings.Contains(string(out.Stdout), `"decision":"deny"`)
	case ClientCursor:
		return out.Exit == 0 && strings.Contains(string(out.Stdout), `"permission":"deny"`)
	case ClientAntigravity:
		return out.Exit == 0 && strings.Contains(string(out.Stdout), `"decision":"deny"`)
	case ClientCopilot:
		return out.Exit == 0 && strings.Contains(string(out.Stdout), `"permissionDecision":"deny"`)
	default:
		return out.Exit == 0 && strings.Contains(string(out.Stdout), `"permissionDecision":"deny"`)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(git, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(git, "refs", "heads", "main"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type staticURL struct{ url string }

func (s staticURL) HubURL() string         { return s.url }
func (s staticURL) Token() (string, error) { return "secret", nil }

func hubURL(url string) staticURL { return staticURL{url: url} }

type countingHub struct{ fn func() }

func (c countingHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	c.fn()
	return hubclient.SessionResponse{}, context.Canceled
}
func (c countingHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	c.fn()
	return hubclient.TouchResponse{}, context.Canceled
}
func (c countingHub) EndExecution(context.Context, string, int) error {
	c.fn()
	return context.Canceled
}

type denyHub struct{ calls *int }

func (d denyHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	*d.calls++
	return hubclient.SessionResponse{ExecutionID: "ex_1", Generation: 1, Handle: "ex_1"}, nil
}
func (d denyHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	*d.calls++
	return hubclient.TouchResponse{Action: "deny", Message: "held"}, nil
}
func (d denyHub) EndExecution(context.Context, string, int) error { return nil }
