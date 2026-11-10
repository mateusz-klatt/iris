package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

// Reduced shell snapshots have a route of their own, which a hub from before it answers
// not_found (ADR 018). This hub serves it, keeps each kind of snapshot on its route, and names
// the capability in its registration answers.
func TestReducedSnapshotsHaveARouteOfTheirOwn(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), &generationClock{now: time.Unix(1800000000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := New(s, "token", []string{"127.0.0.1"})
	h.Log = log.New(io.Discard, "", 0)
	post := func(path string, req map[string]any, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: status %d: %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	registration := func() map[string]any {
		return map[string]any{"client": "codex", "session_id": "one", "host": "host", "cwd": "/repo", "worktree": "/repo", "remote_url": "https://example.com/team/project.git", "source": "hook", "kind": "before", "status": map[string]string{}}
	}
	capabilities := []any{"shell_scope"}
	if got := post("/v1/sessions", registration(), http.StatusOK)["capabilities"]; !reflect.DeepEqual(got, capabilities) {
		t.Fatalf("registration answer: %v", got)
	}
	scoped := registration()
	scoped["scope"] = map[string]any{"mode": "dirty", "submodules": []string{"lib"}}
	session := post("/v1/shell/scoped", scoped, http.StatusOK)["session"].(map[string]any)
	if !reflect.DeepEqual(session["capabilities"], capabilities) {
		t.Fatalf("scoped answer: %v", session)
	}
	post("/v1/shell/reconcile", scoped, http.StatusBadRequest)
	post("/v1/shell/scoped", registration(), http.StatusBadRequest)
	post("/v1/shell/unscoped", scoped, http.StatusNotFound)
}
