package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type statusConfig struct {
	url, token string
	err        error
}

func (c statusConfig) HubURL() string         { return c.url }
func (c statusConfig) Token() (string, error) { return c.token, c.err }

// A hub answers the right token with its document and any other with only that it is up.
func TestStatusReturnsTheHubsDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" && r.Header.Get("Authorization") == "Bearer right" {
			_, _ = w.Write([]byte(`{"stream":"s1","counts":{"Agent":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	got := Status(context.Background(), statusConfig{url: srv.URL, token: "right"})
	if got.Hub != srv.URL || got.Error != nil || got.Status["stream"] != "s1" {
		t.Fatalf("%+v", got)
	}
	got = Status(context.Background(), statusConfig{url: srv.URL, token: "wrong"})
	if got.Status != nil || got.Error == nil || got.Error.Code != "token_rejected" {
		t.Fatalf("%+v", got)
	}
}

// Each way of having no document says which it is, with a stable code.
func TestStatusNamesEachFailure(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "envelope") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"database is locked"}}`))
			return
		}
		_, _ = w.Write([]byte(`not json`))
	}))
	defer broken.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	cases := []struct {
		name       string
		cfg        statusConfig
		code, says string
	}{
		{"no token", statusConfig{url: broken.URL}, "no_token", "no token is configured"},
		{"unreadable config", statusConfig{url: broken.URL, err: errors.New("read client.json: bad")}, "config", "client.json"},
		{"no hub URL", statusConfig{token: "t"}, "config", "no hub URL"},
		{"cleartext remote hub", statusConfig{url: "http://hub.example", token: "t"}, "config", "refusing cleartext"},
		{"hub not listening", statusConfig{url: deadURL, token: "t"}, "unreachable", "/v1/status"},
		{"hub error", statusConfig{url: broken.URL, token: "envelope"}, "hub_error", "database is locked"},
		{"not JSON", statusConfig{url: broken.URL, token: "t"}, "hub_error", "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Status(context.Background(), tc.cfg)
			if got.Status != nil || got.Error == nil || got.Error.Code != tc.code || !strings.Contains(got.Error.Message, tc.says) {
				t.Fatalf("%+v %+v", got, got.Error)
			}
		})
	}
}

// Credentials written into the hub URL appear neither in the report's hub nor in an error
// message that quotes the URL.
func TestStatusNeverShowsCredentialsInTheHubURL(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	got := Status(context.Background(), statusConfig{url: "http://user:secret@" + host + "/", token: "t"})
	if got.Hub != "http://"+host+"/" || got.Error == nil || got.Error.Code != "unreachable" {
		t.Fatalf("%+v %+v", got, got.Error)
	}
	if strings.Contains(got.Error.Message, "secret") || strings.Contains(got.Error.Message, "user") || !strings.Contains(got.Error.Message, host) {
		t.Fatalf("message: %s", got.Error.Message)
	}
	if redact("://bad") != "://bad" {
		t.Fatal("an unparsable URL was changed")
	}
}

// A hub that does not answer in time is unreachable, not a hang.
func TestStatusGivesUpOnASilentHub(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	old := StatusTimeout
	StatusTimeout = 50 * time.Millisecond
	t.Cleanup(func() { StatusTimeout = old })
	got := Status(context.Background(), statusConfig{url: srv.URL, token: "t"})
	if got.Error == nil || got.Error.Code != "unreachable" || !strings.Contains(got.Error.Message, "deadline exceeded") {
		t.Fatalf("%+v", got.Error)
	}
}
