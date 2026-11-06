package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesTheShellAndFallsBack(t *testing.T) {
	handler := Handler()

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "Iris") {
		t.Fatalf("index: %d %q", index.Code, index.Body.String())
	}

	fallback := httptest.NewRecorder()
	handler.ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/threads/ms_1", nil))
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), "Iris") {
		t.Fatalf("fallback: %d %q", fallback.Code, fallback.Body.String())
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/threads/ms_1", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("head fallback: %d body %d", head.Code, head.Body.Len())
	}

	method := httptest.NewRecorder()
	handler.ServeHTTP(method, httptest.NewRequest(http.MethodPost, "/", nil))
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", method.Code)
	}
}
