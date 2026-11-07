package hub

import (
	"bytes"
	"context"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	before := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(before) })
	return &output
}

func get(h *Hub, s *httptest.Server, token string) int {
	r := httptest.NewRequest("GET", s.URL+"/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestEachTokenIsLoggedOncePerStartByDefault(t *testing.T) {
	h, s := setup(t)
	laptop, err := h.store.AddToken(context.Background(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	output := captureLog(t)
	for range 3 {
		if get(h, s, "test-secret") != 200 || get(h, s, laptop) != 200 {
			t.Fatal("a valid token was refused")
		}
	}
	text := output.String()
	if strings.Contains(text, "authenticated request") {
		t.Fatalf("a request was logged without --access-log:\n%s", text)
	}
	if strings.Count(text, "token first used since start") != 2 || !strings.Contains(text, `token="laptop"`) {
		t.Fatalf("each token is named once:\n%s", text)
	}
	if strings.Contains(text, laptop) || strings.Contains(text, "test-secret") {
		t.Fatal("a credential reached the log")
	}
	// A refused token is still logged every time.
	before := output.Len()
	for range 2 {
		get(h, s, "wrong")
	}
	if strings.Count(output.String()[before:], "authentication failed") != 2 {
		t.Fatal(output.String()[before:])
	}
}

func TestAccessLogNamesEveryRequest(t *testing.T) {
	h, s := setup(t)
	h.AccessLog = true
	output := captureLog(t)
	for range 3 {
		get(h, s, "test-secret")
	}
	text := output.String()
	if strings.Count(text, "authenticated request") != 3 || strings.Contains(text, "first used") {
		t.Fatalf("%s", text)
	}
}

func TestRefusalsTellMissingUnknownAndRevokedApart(t *testing.T) {
	h, s := setup(t)
	laptop, err := h.store.AddToken(context.Background(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.store.RevokeToken(context.Background(), "laptop"); err != nil {
		t.Fatal(err)
	}
	output := captureLog(t)
	for header, want := range map[string]string{
		"":                           `reason="bearer token required"`,
		"Bearer never-issued":        `reason="unknown bearer token"`,
		"Bearer " + laptop:           `reason="revoked token \"laptop\""`,
		"Basic dXNlcjpwYXNzd29yZA==": `reason="bearer token required"`,
	} {
		before := output.Len()
		r := httptest.NewRequest("GET", s.URL+"/v1/agents", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := output.String()[before:]
		if w.Code != 401 || !strings.Contains(got, want) {
			t.Fatalf("%q: %d %s", header, w.Code, got)
		}
		if strings.Contains(got, laptop) {
			t.Fatal("a secret reached the log")
		}
	}
}
