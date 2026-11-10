package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// idHub answers a lookup by session from sessions, the Execution a hook registered for each
// session id, and any other lookup with unbound. It answers every refresh, and records each
// lookup and heartbeat.
type idHub struct {
	mu       sync.Mutex
	sessions map[string]string
	lookups  []string
	beats    []string
}

func (h *idHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		h.beats = append(h.beats, r.URL.Path)
		_, _ = w.Write([]byte(`{"lease_ms":60000}`))
	case r.URL.Path != "/v1/bind":
		_, _ = w.Write([]byte(`{}`))
	case req["execution"] != nil:
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": req["execution"], "generation": 1, "repository_id": "rp"})
	default:
		session, _ := req["session_id"].(string)
		h.lookups = append(h.lookups, session)
		if ex := h.sessions[session]; ex != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": ex, "generation": 1, "repository_id": "rp"})
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"unbound","message":"no positive binding"}}`))
	}
}

func (h *idHub) seen() (lookups, beats []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lookups...), append([]string(nil), h.beats...)
}

// idSession connects an MCP client to a shim of client against h, with nothing else to bind by,
// and returns what the shim observes.
func idSession(t *testing.T, h http.Handler, client string) (*mcp.ClientSession, func() []string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var observed []string
	resolver := &binding.Resolver{
		Hub:    &hubclient.Client{Config: config{srv.URL}},
		Client: client, Host: "h", CWD: "/repo",
		Getenv:   func(string) string { return "" },
		Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Observe: func(text string) {
			mu.Lock()
			defer mu.Unlock()
			observed = append(observed, text)
		},
	}
	serverSide, clientSide := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := New(resolver).Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), observed...)
	}
}

// useWith calls tool with args and the call metadata meta.
func useWith(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any, meta mcp.Meta) (map[string]any, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Meta: meta, Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out, res.IsError
}

// capturedMeta is the _meta of a tool call captured from Codex 0.167.0.
func capturedMeta(t *testing.T, name string) mcp.Meta {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	var call struct {
		Params struct {
			Meta mcp.Meta `json:"_meta"`
		} `json:"params"`
	}
	if err = json.Unmarshal(raw, &call); err != nil {
		t.Fatal(err)
	}
	return call.Params.Meta
}

// hookSession is the session_id of a hook payload captured from the same Codex runs.
func hookSession(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "hook", "testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.SessionID
}

// A thread the daemon loaded names, on its tool call, the session its hooks registered: the call
// binds that session's Execution, and whoami renews it.
func TestACapturedRootCallBindsTheSessionItsHooksRegistered(t *testing.T) {
	meta := capturedMeta(t, "tools-call-root-0.167.0.json")
	session := hookSession(t, "SessionStart-resume.json")
	if meta["sessionId"] != session || meta["threadId"] != session {
		t.Fatalf("the captures disagree: %v, hooks %s", meta, session)
	}
	h := &idHub{sessions: map[string]string{session: "ex_root"}}
	s, observed := idSession(t, h, "codex")
	out, failed := useWith(t, s, "whoami", map[string]any{}, meta)
	if failed || out["execution_id"] != "ex_root" || out["binding_rung"] != float64(2) {
		t.Fatal(out)
	}
	if lookups, beats := h.seen(); len(lookups) != 1 || lookups[0] != session || len(beats) != 1 || len(observed()) != 0 {
		t.Fatalf("lookups %v beats %v observed %v", lookups, beats, observed())
	}
}

// A sub-agent's captured call names the root's session, as its SubagentStart hook does, and its
// own thread: it binds the root's Execution, and its whoami renews nothing.
func TestACapturedSubagentCallBindsTheRootAndRenewsNothing(t *testing.T) {
	meta := capturedMeta(t, "tools-call-subagent-0.167.0.json")
	root := hookSession(t, "SubagentStart.json")
	if meta["sessionId"] != root || meta["threadId"] == root {
		t.Fatalf("the captures disagree: %v, hooks %s", meta, root)
	}
	h := &idHub{sessions: map[string]string{root: "ex_root"}}
	s, _ := idSession(t, h, "codex")
	out, failed := useWith(t, s, "whoami", map[string]any{}, meta)
	if failed || out["execution_id"] != "ex_root" {
		t.Fatal(out)
	}
	if _, beats := h.seen(); len(beats) != 0 {
		t.Fatalf("a sub-agent's whoami renewed %v", beats)
	}
}

// Only a Codex shim reads a call's ids: the same call to another client's shim binds as today.
func TestOnlyACodexShimReadsTheCallsIds(t *testing.T) {
	meta := capturedMeta(t, "tools-call-root-0.167.0.json")
	h := &idHub{sessions: map[string]string{meta["sessionId"].(string): "ex_root"}}
	s, _ := idSession(t, h, "claude")
	out, failed := useWith(t, s, "whoami", map[string]any{}, meta)
	if !failed || out["error"].(map[string]any)["code"] != "unbound" {
		t.Fatal(out)
	}
	if lookups, _ := h.seen(); len(lookups) != 0 {
		t.Fatalf("looked up %v", lookups)
	}
}

// A call is noted before its arguments are decoded, so a malformed one that names a second
// session is still observed. Another session's whoami renews nothing of it.
func TestACallIsNotedBeforeItsArgumentsAreDecoded(t *testing.T) {
	h := &idHub{sessions: map[string]string{"a": "ex_a", "b": "ex_b"}}
	s, observed := idSession(t, h, "codex")
	if out, failed := useWith(t, s, "whoami", map[string]any{}, mcp.Meta{"sessionId": "a", "threadId": "a"}); failed || out["execution_id"] != "ex_a" {
		t.Fatal(out)
	}
	out, failed := useWith(t, s, "inbox", map[string]any{"limit": "many"}, mcp.Meta{"sessionId": "b", "threadId": "b"})
	if !failed || out["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatal(out)
	}
	if got := observed(); len(got) != 1 || got[0] != binding.ObservedTwoSessions {
		t.Fatalf("observed %v", got)
	}
	if out, failed = useWith(t, s, "whoami", map[string]any{}, mcp.Meta{"sessionId": "b", "threadId": "b"}); failed || out["execution_id"] != "ex_b" {
		t.Fatal(out)
	}
	if _, beats := h.seen(); len(beats) != 1 {
		t.Fatalf("another session's whoami renewed it: %v", beats)
	}
}

// Only a non-empty string at the top of a Codex call's _meta is an id: null, a number, an object,
// or an id nested in the turn metadata alone, is none.
func TestOnlyStringsAtTheTopOfMetaAreIds(t *testing.T) {
	for _, meta := range []mcp.Meta{
		{"sessionId": nil, "threadId": nil},
		{"sessionId": float64(7), "threadId": float64(7)},
		{"sessionId": map[string]any{"id": "s"}},
		{"x-codex-turn-metadata": map[string]any{"session_id": "s", "thread_id": "s"}},
	} {
		if session, thread := callIDs("codex", meta); session != "" || thread != "" {
			t.Fatalf("%v gave %q %q", meta, session, thread)
		}
	}
	if session, thread := callIDs("codex", mcp.Meta{"sessionId": "s", "threadId": float64(7)}); session != "s" || thread != "" {
		t.Fatalf("%q %q", session, thread)
	}
	if session, thread := callIDs("claude", mcp.Meta{"sessionId": "s", "threadId": "s"}); session != "" || thread != "" {
		t.Fatalf("%q %q", session, thread)
	}
}
