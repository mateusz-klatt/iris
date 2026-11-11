package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Keep the expected prose independent of the production constant so both result forms
// assert the exact public wording.
const wantLostMessage = "This call's Execution or cached claim is not usable. " +
	"After a hub restart, a recovering session reattaches only when the hub accepts its " +
	"remembered generation within the 120 s recovery window. " +
	"Resume the session that owns the Execution in its Git worktree to run an enabled Iris " +
	"SessionStart hook, then retry. " +
	"A prompt also registers through working Iris hooks on Claude Code, Codex, Copilot and Kimi, " +
	"or through Antigravity's PreInvocation. Cursor and Grok prompts do not register. " +
	"A recognized pre-edit hook with paths inside the worktree also registers. " +
	"Hooks need hub access and the remembered generation. " +
	"A shell command is not a reliable remedy: a reconcile that times out before registration " +
	"reaches the hub does not reattach. " +
	"An MCP tool retry first looks for a usable binding; that lookup does not reattach. " +
	"A shim retaining its binding can reattach through its heartbeat. " +
	"Without that binding, including after confirmed loss, the shim cannot reattach through a heartbeat. " +
	"Codex id-mode sub-agents never renew the Execution. " +
	"Waiting past the recovery deadline cannot revive the old Execution; " +
	"a registering hook must obtain a new generation. " +
	"For IRIS_EXECUTION, hooks must belong to the session whose Execution the override names. " +
	"Another session's hooks do not reattach that Execution. " +
	"Legacy binding may need the current SessionStart execution handle, or a shim restart with " +
	"an updated IRIS_EXECUTION; Codex id mode binds by session id, never a handle. " +
	"For a replayed claim that is no longer live, check claims and request a new claim with a fresh key. " +
	"Run `iris doctor` if hooks or binding still fail."

// Both forms reach clients: some read text, others read structuredContent.
func assertLostResult(t *testing.T, result *mcp.CallToolResult, details any) {
	t.Helper()
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("lost was not a single tool error: %+v", result)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("lost was not text: %T", result.Content[0])
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"error": map[string]any{"code": "lost", "message": wantLostMessage, "details": details}}
	for name, raw := range map[string][]byte{"text": []byte(content.Text), "structured": structured} {
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s is not JSON: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s lost answer: %s, want %+v", name, raw, want)
		}
	}
}

func callLost(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any, meta mcp.Meta, details any) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	assertLostResult(t, result, details)
}

func TestLostAnswersKeepDetailsWithoutMutatingTheirSource(t *testing.T) {
	for _, wrap := range []string{"typed", "wrapped", "binding retry"} {
		t.Run(wrap, func(t *testing.T) {
			source := &hubclient.Error{Code: "lost", Message: "a specific hub diagnosis", Details: map[string]any{"key": "retry", "fence": float64(7)}}
			var err error = source
			details := source.Details
			switch wrap {
			case "wrapped":
				err = fmt.Errorf("request failed: %w", source)
			case "binding retry":
				err = &binding.LostError{Cause: source}
				// The stable outer lost answer is public; its cause stays diagnostic.
				details = nil
			}
			before := err.Error()
			assertLostResult(t, toolError(err), details)
			if source.Message != "a specific hub diagnosis" || err.Error() != before || !errors.Is(err, source) {
				t.Fatalf("formatting changed the diagnostic source: %+v, %v", source, err)
			}
			if !reflect.DeepEqual(source.Details, map[string]any{"key": "retry", "fence": float64(7)}) {
				t.Fatalf("formatting changed the source details: %+v", source)
			}
		})
	}
}

// A resolver is exposed here so a real maintenance tick can confirm a loss before
// the next MCP call; the test never changes binding's private state.
func lostSession(t *testing.T, handler http.Handler, client string, env map[string]string) (*mcp.ClientSession, *binding.Resolver) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	resolver := &binding.Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: client, Host: "h", CWD: t.TempDir(), Getenv: func(key string) string { return env[key] }}
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := New(resolver).Connect(context.Background(), serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, resolver
}

func writeLost(w http.ResponseWriter, message string, details any) {
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "lost", "message": message, "details": details}})
}

func TestInitialHandleAndOverrideLossExplainRecovery(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint("override=", override), func(t *testing.T) {
			var mu sync.Mutex
			var requests []map[string]any
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				requests = append(requests, body)
				mu.Unlock()
				writeLost(w, "Execution is unknown; register the session again", map[string]any{"reason": "unknown"})
			})
			env := map[string]string{}
			args := map[string]any{"execution": "ex"}
			if override {
				env["IRIS_EXECUTION"] = "ex"
				args = nil
			}
			session, resolver := lostSession(t, handler, "codex", env)
			// With an override, even Codex ids for another session cannot replace it.
			var meta mcp.Meta
			if override {
				meta = mcp.Meta{"sessionId": "other", "threadId": "other"}
			}
			callLost(t, session, "peers", args, meta, map[string]any{"reason": "unknown"})
			if resolver.Lost() {
				t.Fatal("a binding refusal confirmed a heartbeat loss")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0]["execution"] != "ex" || requests[0]["session_id"] != nil {
				t.Fatalf("the explicit identity was not used alone: %+v", requests)
			}
		})
	}
}

func TestCachedBindingsExplainLossAndKeepTheirHeartbeatProof(t *testing.T) {
	for _, mode := range []string{"legacy session", "override", "Codex root"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var binds []map[string]any
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/bind" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				binds = append(binds, body)
				count := len(binds)
				mu.Unlock()
				if count > 1 {
					writeLost(w, "Execution lease or generation is lost; reattach the session", nil)
					return
				}
				_, _ = w.Write([]byte(`{"execution_id":"ex","generation":3,"repository_id":"rp"}`))
			})
			client, env := "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}
			var meta mcp.Meta
			if mode == "override" {
				env = map[string]string{"IRIS_EXECUTION": "ex"}
			}
			if mode == "Codex root" {
				client, env, meta = "codex", nil, mcp.Meta{"sessionId": "s", "threadId": "s"}
			}
			session, resolver := lostSession(t, handler, client, env)
			if out, failed := useWith(t, session, "peers", nil, meta); failed {
				t.Fatal(out)
			}
			callLost(t, session, "peers", nil, meta, nil)
			if resolver.Lost() {
				t.Fatal("refresh discarded the generation before its heartbeat")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(binds) != 2 || binds[1]["execution"] != "ex" {
				t.Fatalf("a cached binding was not refreshed by Execution: %+v", binds)
			}
		})
	}
}

func TestInitialLegacyFallbackExplainsMissingRecoveryProof(t *testing.T) {
	var mu sync.Mutex
	var registration map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/bind" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"unbound","message":"no positive binding"}}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		registration = body
		mu.Unlock()
		writeLost(w, "recovery requires the prior session generation", nil)
	})
	session, _ := lostSession(t, handler, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"})
	callLost(t, session, "peers", nil, nil, nil)
	mu.Lock()
	defer mu.Unlock()
	if registration["session_id"] != "s" || registration["source"] != "mcp" || registration["generation"] != float64(0) {
		t.Fatalf("fallback did not attempt registration without recovery proof: %+v", registration)
	}
}

func TestConfirmedLegacyLossExplainsFailedRetriesWithoutRegistering(t *testing.T) {
	for _, retry := range []string{"unbound", "ambiguous", "unauthorized", "lost", "stale Execution"} {
		t.Run(retry, func(t *testing.T) {
			var failed atomic.Bool
			var operations, registrations, heartbeats atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					heartbeats.Add(1)
					writeLost(w, "wrong generation", nil)
				case r.URL.Path == "/v1/bind":
					if failed.Load() && retry != "stale Execution" {
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": retry, "message": "specific retry diagnosis must stay diagnostic"}})
						return
					}
					_, _ = w.Write([]byte(`{"execution_id":"ex","generation":3,"repository_id":"rp"}`))
				case r.URL.Path == "/v1/sessions":
					registrations.Add(1)
				default:
					operations.Add(1)
					_, _ = w.Write([]byte(`{}`))
				}
			})
			session, resolver := lostSession(t, handler, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"})
			if out, bad := use(t, session, "peers", nil); bad {
				t.Fatal(out)
			}
			failed.Store(true)
			// One maintenance heartbeat is enough to confirm and blacklist the generation.
			ticks := 0
			resolver.Maintain(context.Background(), "shim", time.Millisecond, func() bool { ticks++; return ticks == 1 }, func() {})
			if !resolver.Lost() {
				t.Fatal("the heartbeat did not confirm the loss")
			}
			callLost(t, session, "peers", nil, nil, nil)
			if !resolver.Lost() || heartbeats.Load() != 1 || operations.Load() != 1 || registrations.Load() != 0 {
				t.Fatalf("a retry reused or registered an identity: lost %v, heartbeats %d, operations %d, registrations %d", resolver.Lost(), heartbeats.Load(), operations.Load(), registrations.Load())
			}
		})
	}
}

func TestEveryToolOperationExplainsLostAndPreservesRetryKeys(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"whoami", nil}, {"peers", nil}, {"claims", nil}, {"inbox", nil}, {"history", nil},
		{"thread", map[string]any{"thread_id": "thread"}},
		{"read", map[string]any{"delivery_id": "delivery"}},
		{"read", map[string]any{"message_id": "message"}},
		{"receipts", map[string]any{"message_id": "message"}},
		{"send", map[string]any{"to": []string{"peer"}, "body": "hello", "key": "same-send"}},
		{"send", map[string]any{"to": []string{"peer"}, "body": "hello"}},
		{"claim", map[string]any{"kind": "exact", "path": "file", "key": "same-claim"}},
		{"claim", map[string]any{"kind": "exact", "path": "file"}},
		{"release", map[string]any{"all": true}},
		{"ack", map[string]any{"delivery_id": "delivery", "attempt": 1}},
		{"watch_ticket", nil},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			hubDetails := map[string]any{"diagnosis": "retained"}
			f := &fakeHub{status: http.StatusConflict, reply: `{"error":{"code":"lost","message":"cached or session state is lost","details":{"diagnosis":"retained"}}}`}
			session := connect(t, f)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			var details any = hubDetails
			if tc.tool == "send" || tc.tool == "claim" {
				key, ok := f.last(t).body["key"].(string)
				if !ok || key == "" || (tc.args["key"] != nil && key != tc.args["key"]) {
					t.Fatalf("a lost mutation discarded its retry key: %+v", f.last(t))
				}
				details = map[string]any{"key": key, "hub_details": hubDetails}
			}
			assertLostResult(t, result, details)
		})
	}
}

func TestLookupOnlyCodexOperationsExplainLossWithoutRenewing(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign=", foreign), func(t *testing.T) {
			var beats atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/bind":
					_, _ = w.Write([]byte(`{"execution_id":"ex","generation":3,"repository_id":"rp"}`))
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					beats.Add(1)
					_, _ = w.Write([]byte(`{}`))
				default:
					writeLost(w, "Execution lease or generation is lost; reattach the session", nil)
				}
			})
			session, _ := lostSession(t, handler, "codex", nil)
			meta := mcp.Meta{"sessionId": "root", "threadId": "child"}
			if foreign {
				// Establish root mode before a call naming another session is looked up.
				if _, failed := useWith(t, session, "whoami", nil, mcp.Meta{"sessionId": "root", "threadId": "root"}); failed {
					t.Fatal("root could not bind")
				}
				meta = mcp.Meta{"sessionId": "other", "threadId": "other"}
			}
			callLost(t, session, "peers", nil, meta, nil)
			want := int64(0)
			if foreign {
				want = 1
			}
			if beats.Load() != want {
				t.Fatalf("lookup-only binding renewed the root: got %d heartbeats, want %d", beats.Load(), want)
			}
		})
	}
}

// A lost cached claim does not mean the session was lost. The same session can
// inspect its claims and acquire a new one with a fresh key, as the advice says.
func TestReleasedClaimReplayExplainsRecoveryWithoutLosingTheSession(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	registration, err := s.Call(context.Background(), "sessions", store.Request{Client: "claude", Host: "h", SessionID: "s", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(s, "token", []string{"iris.test"})
	session, resolver := lostSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "iris.test"
		h.ServeHTTP(w, r)
	}), "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"})
	args := map[string]any{"kind": "exact", "path": "file", "key": "first"}
	if out, failed := use(t, session, "claim", args); failed {
		t.Fatal(out)
	}
	if out, failed := use(t, session, "release", map[string]any{"all": true}); failed {
		t.Fatal(out)
	}
	callLost(t, session, "claim", args, nil, map[string]any{"key": "first", "hub_details": map[string]any{}})
	if out, failed := use(t, session, "claims", nil); failed || len(out["claims"].([]any)) != 0 {
		t.Fatalf("released claims are not readable: %+v", out)
	}
	if out, failed := use(t, session, "whoami", nil); failed || out["execution_id"] != registration["execution_id"] || resolver.Lost() {
		t.Fatalf("a lost claim discarded a healthy session: %+v", out)
	}
	args["key"] = "new"
	if out, failed := use(t, session, "claim", args); failed || out["key"] != "new" {
		t.Fatalf("a fresh key could not obtain a new claim: %+v", out)
	}
}
