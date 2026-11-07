package hub

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/store"
)

func TestLogLinesGoToTheConfiguredLogger(t *testing.T) {
	var buf bytes.Buffer
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := New(s, "secret", []string{"example.com"})
	h.Log = log.New(&buf, "", 0)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), "authentication failed") {
		t.Fatalf("log %q", buf.String())
	}
}
