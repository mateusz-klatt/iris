package binding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestClientMismatchDisablesEveryBindingRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ancestor string
		markers  bool
	}{
		{name: "cursor-agent", ancestor: "/opt/cursor-agent"},
		{name: "agent alias with Cursor markers", ancestor: "agent", markers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "ex_wrong", "generation": 1})
			}))
			defer srv.Close()
			env := map[string]string{"IRIS_EXECUTION": "ex_inherited", "CLAUDE_CODE_SESSION_ID": "inherited-session"}
			if tc.markers {
				env["CURSOR_PROJECT_DIR"] = "/scratch/project"
				env["CURSOR_VERSION"] = "2026.11.22-e9042b2"
			}
			r := &Resolver{
				Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "claude", Host: "host", CWD: t.TempDir(),
				Getenv: func(key string) string { return env[key] },
				Ancestor: func() (proc.Ancestor, error) {
					return proc.Ancestor{PID: 42, StartMS: 7, Name: tc.ancestor}, nil
				},
				Recall: func(string, string) (int, bool, error) {
					t.Error("a mismatched shim tried generation-memory reattachment")
					return 1, true, nil
				},
			}
			var observed []string
			r.Observe = func(text string) { observed = append(observed, text) }
			if !r.CheckClient() {
				t.Fatal("a Claude shim cross-loaded by Cursor was accepted")
			}
			if !slices.Equal(observed, []string{ClientMismatchObservation("claude", "cursor")}) {
				t.Fatalf("observed %q", observed)
			}

			for _, call := range []struct {
				name string
				run  func() error
			}{
				{"ordinary resolve and registration", func() error {
					_, err := r.Resolve(context.Background(), "ex_explicit")
					return err
				}},
				{"tool call id mode", func() error {
					_, _, err := r.ResolveCall(context.Background(), "ex_explicit", "call-session", "call-session")
					return err
				}},
				{"cached heartbeat", func() error {
					r.bound = Bound{ExecutionID: "ex_cached", Generation: 1}
					return r.Heartbeat(context.Background(), "shim")
				}},
			} {
				t.Run(call.name, func(t *testing.T) {
					err := call.run()
					if !IsCode(err, "client_mismatch") || !strings.Contains(err.Error(), "native Iris MCP server (`iris mcp --client cursor`)") {
						t.Fatalf("%v", err)
					}
				})
			}

			stopped := false
			r.Maintain(context.Background(), "shim", time.Millisecond, func() bool {
				t.Error("maintenance checked the parent despite its mismatch")
				return true
			}, func() { stopped = true })
			if stopped {
				t.Fatal("a mismatched maintenance worker stopped the MCP server")
			}
			if requests != 0 {
				t.Fatalf("a mismatched shim made %d hub requests", requests)
			}
			if !slices.Equal(observed, []string{ClientMismatchObservation("claude", "cursor")}) {
				t.Fatalf("calls repeated the startup observation: %q", observed)
			}
		})
	}
}

func TestCheckClientCursorEvidenceOutranksInheritedClaudeSession(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ancestor proc.Ancestor
		markers  bool
		walk     bool
	}{
		{name: "cursor ancestor and environment", ancestor: proc.Ancestor{PID: 42, StartMS: 7, Name: "cursor-agent"}, markers: true},
		{name: "agent alias and environment before outer claude", markers: true, walk: true},
		{name: "cursor environment only", ancestor: proc.Ancestor{PID: 42, StartMS: 7, Name: "helper"}, markers: true},
		{name: "cursor ancestor only", ancestor: proc.Ancestor{PID: 42, StartMS: 7, Name: "cursor-agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &scriptedHub{}
			env := map[string]string{"CLAUDE_CODE_SESSION_ID": "inherited-parent-claude"}
			if tc.markers {
				env["CURSOR_PROJECT_DIR"] = "/scratch/project"
				env["CURSOR_VERSION"] = "2026.11.22-e9042b2"
			}
			ancestor := tc.ancestor
			if tc.walk {
				tree := map[int]proc.Snapshot{
					10: {PID: 10, PPID: 9, Name: "iris"},
					9:  {PID: 9, PPID: 2, Name: "agent"},
					2:  {PID: 2, PPID: 1, Name: "claude"},
				}
				got, ok := proc.NearestFrom(10, func(pid int) (proc.Snapshot, bool) {
					s, ok := tree[pid]
					return s, ok
				}, func(key string) string { return env[key] })
				if !ok || got.Name != "agent" {
					t.Fatalf("agent alias did not win the walk before outer Claude: %+v, %v", got, ok)
				}
				ancestor = proc.Ancestor(got)
			}
			r := resolverFor(t, h, "claude", env, ancestor)
			var observed []string
			r.Observe = func(text string) { observed = append(observed, text) }

			mismatched := r.CheckClient()
			_, err := r.Resolve(context.Background(), "")
			if !mismatched || !IsCode(err, "client_mismatch") {
				t.Errorf("inherited session escaped the Cursor lockout: mismatched %v, resolve error %v", mismatched, err)
			}
			if len(h.bound) != 0 || h.session != nil {
				t.Errorf("cross-loaded shim reached the hub: binds %v, registration %+v", h.bound, h.session)
			}
			want := []string{ClientMismatchObservation("claude", "cursor")}
			if !slices.Equal(observed, want) {
				t.Errorf("observed %q, want %q", observed, want)
			}
		})
	}
}

func TestCheckClientKeepsNativeAndUnknownAncestorsUsable(t *testing.T) {
	t.Run("real Claude nested in Cursor", func(t *testing.T) {
		h := &scriptedHub{}
		r := resolverFor(t, h, "claude", map[string]string{
			"CLAUDE_CODE_SESSION_ID": "native-claude",
			"CURSOR_PROJECT_DIR":     "/outer/cursor/project",
			"CURSOR_VERSION":         "2026.11.22-e9042b2",
		}, proc.Ancestor{PID: 42, StartMS: 7, Name: "claude"})
		if r.CheckClient() {
			t.Fatal("the nearest claude process was rejected because an outer client may be Cursor")
		}
		b, err := r.Resolve(context.Background(), "")
		if err != nil || b.Rung != 2 || len(h.bound) != 1 || h.bound[0]["session_id"] != "native-claude" {
			t.Fatalf("native Claude binding: %+v, %v, requests %v", b, err, h.bound)
		}
	})
	t.Run("bare agent walks on to native Claude", func(t *testing.T) {
		h := &scriptedHub{}
		env := map[string]string{"CLAUDE_CODE_SESSION_ID": "native-claude"}
		tree := map[int]proc.Snapshot{
			10: {PID: 10, PPID: 9, Name: "iris"},
			9:  {PID: 9, PPID: 2, Name: "agent"},
			2:  {PID: 2, PPID: 1, Name: "claude"},
		}
		got, ok := proc.NearestFrom(10, func(pid int) (proc.Snapshot, bool) {
			s, ok := tree[pid]
			return s, ok
		}, func(key string) string { return env[key] })
		if !ok || got.Name != "claude" {
			t.Fatalf("bare agent stopped the walk instead of reaching Claude: %+v, %v", got, ok)
		}
		r := resolverFor(t, h, "claude", env, proc.Ancestor(got))
		if r.CheckClient() {
			t.Fatal("a generic agent name without Cursor markers displaced native Claude")
		}
		b, err := r.Resolve(context.Background(), "")
		if err != nil || b.Rung != 2 || len(h.bound) != 1 || h.bound[0]["session_id"] != "native-claude" {
			t.Fatalf("native Claude after a bare agent: %+v, %v, requests %v", b, err, h.bound)
		}
	})
	// Grok installs its own `agent` alias (~/.grok/bin/agent, the same binary as grok), so
	// a native Grok shim can sit under a process named agent with no Cursor markers. It must
	// keep binding as Grok rather than be refused as a cross-loaded Cursor shim.
	t.Run("native Grok under its own agent alias binds as grok", func(t *testing.T) {
		h := &scriptedHub{}
		env := map[string]string{"GROK_SESSION_ID": "native-grok"}
		r := resolverFor(t, h, "grok", env, proc.Ancestor{PID: 42, StartMS: 7, Name: "/home/user/.grok/bin/agent"})
		if r.CheckClient() {
			t.Fatal("native Grok under its agent alias was refused as another client")
		}
		b, err := r.Resolve(context.Background(), "")
		if err != nil || b.Rung != 2 || len(h.bound) != 1 || h.bound[0]["session_id"] != "native-grok" {
			t.Fatalf("native Grok binding under agent: %+v, %v, requests %v", b, err, h.bound)
		}
	})
	// Grok started from a Cursor terminal inherits Cursor's marker pair. Cursor cross-loads
	// Claude plugins only, so for a grok shim the pair is no evidence of Cursor.
	t.Run("native Grok under agent with inherited Cursor markers binds as grok", func(t *testing.T) {
		h := &scriptedHub{}
		env := map[string]string{
			"GROK_SESSION_ID":    "native-grok",
			"CURSOR_PROJECT_DIR": "/outer/cursor/project",
			"CURSOR_VERSION":     "2026.11.22-e9042b2",
		}
		r := resolverFor(t, h, "grok", env, proc.Ancestor{PID: 42, StartMS: 7, Name: "/home/user/.grok/bin/agent"})
		if r.CheckClient() {
			t.Fatal("inherited Cursor markers refused native Grok as a cross-loaded Cursor shim")
		}
		b, err := r.Resolve(context.Background(), "")
		if err != nil || b.Rung != 2 || len(h.bound) != 1 || h.bound[0]["session_id"] != "native-grok" {
			t.Fatalf("native Grok binding with inherited markers: %+v, %v, requests %v", b, err, h.bound)
		}
	})
	// The shim's walk reads Cursor's markers only for Claude, so with an inherited pair a
	// cursor shim under `agent` under grok still reaches grok, as v0.1.13 did.
	t.Run("only a Claude shim walks with Cursor markers", func(t *testing.T) {
		if reflect.ValueOf(AncestorFor("claude")).Pointer() != reflect.ValueOf(proc.Nearest).Pointer() {
			t.Fatal("a Claude shim does not walk with the environment")
		}
		for _, client := range []string{"cursor", "grok", "codex", "copilot", "kimi", "antigravity"} {
			if reflect.ValueOf(AncestorFor(client)).Pointer() != reflect.ValueOf(proc.NearestWithoutMarkers).Pointer() {
				t.Fatalf("%s shim walks with Cursor markers", client)
			}
		}
		env := map[string]string{"CURSOR_PROJECT_DIR": "/outer/cursor/project", "CURSOR_VERSION": "2026.11.22-e9042b2"}
		tree := map[int]proc.Snapshot{
			10: {PID: 10, PPID: 9, Name: "iris"},
			9:  {PID: 9, PPID: 2, Name: "agent"},
			2:  {PID: 2, PPID: 1, Name: "grok"},
		}
		get := func(pid int) (proc.Snapshot, bool) { s, ok := tree[pid]; return s, ok }
		if got, ok := proc.NearestFrom(10, get, nil); !ok || got.Name != "grok" {
			t.Fatalf("walk without markers stopped before grok: %+v, %v", got, ok)
		}
		if got, ok := proc.NearestFrom(10, get, func(k string) string { return env[k] }); !ok || got.Name != "agent" {
			t.Fatalf("walk with markers did not stop at the Cursor agent alias: %+v, %v", got, ok)
		}
		h := &scriptedHub{}
		r := resolverFor(t, h, "cursor", env, proc.Ancestor{PID: 2, StartMS: 7, Name: "grok"})
		if !r.CheckClient() {
			t.Fatal("a cursor shim under grok was not refused as a cross-load")
		}
	})
	t.Run("unknown helper is not a mismatched client", func(t *testing.T) {
		h := &scriptedHub{}
		r := resolverFor(t, h, "claude", map[string]string{"IRIS_EXECUTION": "ex_explicit"}, proc.Ancestor{PID: 42, StartMS: 7, Name: "helper"})
		if r.CheckClient() {
			t.Fatal("an unknown ancestor was treated as another client")
		}
		b, err := r.Resolve(context.Background(), "")
		if err != nil || b.Rung != 1 || len(h.bound) != 1 || h.bound[0]["execution"] != "ex_explicit" {
			t.Fatalf("explicit binding: %+v, %v, requests %v", b, err, h.bound)
		}
	})
}
