package hub

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

type generationClock struct {
	now  time.Time
	tick int64
}

func (c *generationClock) Now() time.Time { return c.now }
func (c *generationClock) Tick() int64    { return c.tick }
func (c *generationClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	c.tick += d.Milliseconds()
}

func TestGenerationBumpLogsItsCauseWithoutSessionContent(t *testing.T) {
	clock := &generationClock{now: time.Unix(1800000000, 0)}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := New(s, "sensitive-token", []string{"127.0.0.1"})
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)
	call := func(method, path string, req map[string]any, want int) map[string]any {
		t.Helper()
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer sensitive-token")
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
	registration := map[string]any{"client": "sensitive-client", "session_id": "sensitive-session", "host": "sensitive-host", "cwd": "/sensitive-path", "source": "hook", "body": "sensitive-body"}
	first := call("POST", "/v1/sessions", registration, 200)
	initialID := first["execution_id"].(string)
	for range 5 {
		call("POST", "/v1/sessions", registration, 200)
	}
	if strings.Contains(logs.String(), "session generation advanced") {
		t.Fatalf("ordinary registration logged a bump: %s", logs.String())
	}
	call("POST", "/v1/executions/"+initialID+"/heartbeat", map[string]any{"generation": 1, "source": "watch"}, 200)
	clock.advance(61 * time.Second)
	registration["generation"] = 1
	call("POST", "/v1/sessions", registration, http.StatusConflict)
	registration["generation"], registration["retry_generation"] = 0, 1
	second := call("POST", "/v1/sessions", registration, 200)
	if second["generation"] != float64(2) || second["execution_id"] == initialID {
		t.Fatalf("replacement: %v", second)
	}
	for range 5 {
		call("POST", "/v1/sessions", registration, 200)
	}
	line := logs.String()
	for _, want := range []string{"session generation advanced", "reason=lease_expired", "proof=stale_retry", "source=hook", "operation=sessions", "previous_generation=1", "generation=2", "previous_execution=" + initialID, "execution=" + second["execution_id"].(string)} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %s", want, line)
		}
	}
	if strings.Count(line, "session generation advanced") != 1 {
		t.Fatalf("duplicate bump diagnostics: %s", line)
	}
	if strings.Contains(line, "sensitive-") {
		t.Fatalf("session data leaked to diagnostic: %s", line)
	}
	call("POST", "/v1/executions/"+second["execution_id"].(string)+"/end", map[string]any{"generation": 2}, 200)
	delete(registration, "retry_generation") // An older client sends no diagnostic marker.
	registration["source"] = "sensitive-source"
	registration["kind"] = "before"
	registration["status"] = map[string]string{}
	third := call("POST", "/v1/shell/reconcile", registration, 200)["session"].(map[string]any)
	if third["generation"] != float64(3) || third["execution_id"] == second["execution_id"] {
		t.Fatalf("shell registration did not replace its ended session: %v", third)
	}
	line = logs.String()
	for _, want := range []string{"operation=reconcile reason=ended proof=absent source=other", "previous_generation=2", "generation=3"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %s", want, line)
		}
	}
	if strings.Count(line, "session generation advanced") != 2 || strings.Contains(line, "sensitive-") {
		t.Fatalf("shell diagnostics duplicated or leaked input: %s", line)
	}
}
