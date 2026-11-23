package binding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestClientMismatchDisablesEveryBindingRoute(t *testing.T) {
	for _, tc := range []struct {
		name, ancestor, exe string
		markers             bool
	}{
		{name: "cursor-agent", ancestor: "/opt/cursor-agent"},
		{name: "agent alias with Cursor markers", ancestor: "agent", markers: true},
		{name: "cursor agent executable with an empty environment", ancestor: "agent", exe: "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node"},
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
					return proc.Ancestor{PID: 42, StartMS: 7, Name: tc.ancestor, Exe: tc.exe}, nil
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
		exe      bool
	}{
		{name: "cursor ancestor and environment", ancestor: proc.Ancestor{PID: 42, StartMS: 7, Name: "cursor-agent"}, markers: true},
		{name: "agent alias and environment before outer claude", markers: true, walk: true},
		{name: "cursor agent executable with empty environment before outer claude", walk: true, exe: true},
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
				agent := proc.Snapshot{PID: 9, PPID: 2, Name: "agent"}
				if tc.exe {
					agent.Exe = "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/node"
					agent.Launcher = "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
				}
				tree := map[int]proc.Snapshot{
					10: {PID: 10, PPID: 9, Name: "iris"},
					9:  agent,
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

func TestGrokAgentAliasBindsAsGrok(t *testing.T) {
	for _, exe := range []string{
		"/home/user/.grok/downloads/grok-1.0.92-linux-x86_64",
		"/home/user/.grok/bin/agent",
		`C:\Users\me\.grok\downloads\grok-1.0.92-windows-x86_64.exe`,
	} {
		t.Run(exe, func(t *testing.T) {
			h := &scriptedHub{}
			r := resolverFor(t, h, "grok", map[string]string{
				"GROK_SESSION_ID":        "native-grok",
				"CLAUDE_CODE_SESSION_ID": "inherited-from-claude",
				"CURSOR_PROJECT_DIR":     "/outer/cursor",
				"CURSOR_VERSION":         "2026.11.22-e9042b2",
			}, proc.Ancestor{PID: 42, StartMS: 7, Name: "agent", Exe: exe})
			if r.CheckClient() {
				t.Fatal("Grok's agent alias was refused")
			}
			b, err := r.Resolve(context.Background(), "")
			if err != nil || b.Rung != 2 || len(h.bound) != 1 || h.bound[0]["session_id"] != "native-grok" {
				t.Fatalf("binding %+v, %v, requests %v", b, err, h.bound)
			}
		})
	}
}

func TestUnidentifiedAgentRefusesWithoutAClient(t *testing.T) {
	for _, client := range []string{"claude", "cursor", "grok"} {
		t.Run(client, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "ex_wrong", "generation": 1})
			}))
			defer srv.Close()
			r := &Resolver{
				Hub: &hubclient.Client{Config: config{srv.URL}}, Client: client, Host: "host", CWD: t.TempDir(),
				Getenv: func(key string) string {
					return map[string]string{
						"IRIS_EXECUTION":         "ex_inherited",
						"CLAUDE_CODE_SESSION_ID": "inherited-session",
						"GROK_SESSION_ID":        "inherited-grok",
					}[key]
				},
				Ancestor: func() (proc.Ancestor, error) {
					return proc.Ancestor{PID: 42, StartMS: 7, Name: "/opt/agent"}, nil
				},
			}
			var observed []string
			r.Observe = func(text string) { observed = append(observed, text) }
			if !r.CheckClient() {
				t.Fatal("an unidentified agent was accepted")
			}
			err := error(nil)
			_, err = r.Resolve(context.Background(), "ex_explicit")
			if !IsCode(err, "client_mismatch") || strings.Contains(err.Error(), "iris mcp --client") || !strings.Contains(err.Error(), "unidentified agent") {
				t.Fatalf("%v", err)
			}
			r.bound = Bound{ExecutionID: "ex_cached", Generation: 1}
			if hb := r.Heartbeat(context.Background(), "shim"); !IsCode(hb, "client_mismatch") {
				t.Fatalf("heartbeat %v", hb)
			}
			stopped := false
			r.Maintain(context.Background(), "shim", time.Millisecond, func() bool { return true }, func() { stopped = true })
			if stopped || requests != 0 {
				t.Fatalf("stopped %v requests %d", stopped, requests)
			}
			if !slices.Equal(observed, []string{UnidentifiedAgentObservation(client)}) {
				t.Fatalf("observed %q", observed)
			}
		})
	}
}

func TestCursorMarkersKeepACursorShimOnAnUnresolvedAgent(t *testing.T) {
	h := &scriptedHub{}
	r := resolverFor(t, h, "cursor", map[string]string{
		"CURSOR_PROJECT_DIR": "/scratch/project",
		"CURSOR_VERSION":     "2026.11.22-e9042b2",
	}, proc.Ancestor{PID: 42, StartMS: 7, Name: "agent"})
	if r.CheckClient() {
		t.Fatal("Cursor's own markers were treated as another client")
	}
	if _, err := r.Resolve(context.Background(), ""); err != nil || len(h.bound) != 1 {
		t.Fatalf("cursor shim did not bind: %v requests %v", err, h.bound)
	}
}

func TestCursorLauncherArgumentWithAnEmptyEnvironmentRefusesTheShim(t *testing.T) {
	h := &scriptedHub{}
	r := resolverFor(t, h, "claude", map[string]string{
		"CLAUDE_CODE_SESSION_ID": "inherited-parent-claude",
	}, proc.Ancestor{
		PID: 42, StartMS: 7, Name: "agent", Exe: "/usr/bin/node",
		Launcher: "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js",
	})
	if !r.CheckClient() {
		t.Fatal("Cursor's index.js argument was not Cursor")
	}
	if _, err := r.Resolve(context.Background(), ""); !IsCode(err, "client_mismatch") || len(h.bound) != 0 {
		t.Fatalf("err %v requests %v", err, h.bound)
	}
}
