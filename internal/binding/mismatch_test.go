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
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "ex_wrong", "generation": 1})
	}))
	defer srv.Close()
	r := &Resolver{
		Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "claude", Host: "host", CWD: t.TempDir(),
		Getenv: func(key string) string {
			return map[string]string{"IRIS_EXECUTION": "ex_inherited", "CLAUDE_CODE_SESSION_ID": "inherited-session"}[key]
		},
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: 42, StartMS: 7, Name: "/opt/cursor-agent"}, nil
		},
		Recall: func(string, string) (int, bool, error) {
			t.Error("a mismatched shim tried generation-memory reattachment")
			return 1, true, nil
		},
	}
	var observed []string
	r.Observe = func(text string) { observed = append(observed, text) }
	if !r.CheckClient() {
		t.Fatal("a Claude shim under cursor-agent was accepted")
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
}

func TestCheckClientKeepsNativeAndUnknownAncestorsUsable(t *testing.T) {
	t.Run("real Claude nested in Cursor", func(t *testing.T) {
		h := &scriptedHub{}
		r := resolverFor(t, h, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "native-claude"}, proc.Ancestor{PID: 42, StartMS: 7, Name: "claude"})
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
