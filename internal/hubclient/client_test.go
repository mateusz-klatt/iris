package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateURL(t *testing.T) {
	ok := []string{"http://127.0.0.1:7878", "http://[::1]:7878", "https://iris.example"}
	for _, raw := range ok {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	bad := []string{"http://localhost:7878", "http://10.0.0.1", "http://example.com", "ftp://127.0.0.1"}
	for _, raw := range bad {
		if err := ValidateURL(raw); err == nil {
			t.Errorf("%s: want error", raw)
		}
	}
}

func TestEnvConfig(t *testing.T) {
	cfg := EnvConfig{Getenv: func(k string) string {
		switch k {
		case "IRIS_HUB_URL":
			return "http://127.0.0.1:9"
		case "IRIS_TOKEN_FILE":
			return "/tmp/token"
		default:
			return ""
		}
	}, ReadFile: func(path string) ([]byte, error) {
		if filepath.Base(path) == "client.json" {
			return nil, os.ErrNotExist
		}
		return []byte(" secret \n"), nil
	}}
	if cfg.HubURL() != "http://127.0.0.1:9" {
		t.Fatal(cfg.HubURL())
	}
	tok, err := cfg.Token()
	if err != nil || tok != "secret" {
		t.Fatalf("token %q %v", tok, err)
	}
	blank := EnvConfig{Getenv: func(string) string { return "" }}
	if blank.HubURL() != DefaultHubURL {
		t.Fatal(blank.HubURL())
	}
	tok, err = blank.Token()
	if err != nil || tok != "" {
		t.Fatalf("empty token %q %v", tok, err)
	}
}

func TestRoundTrip(t *testing.T) {
	var gotPath, gotAuth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/v1/sessions":
			_, _ = w.Write([]byte(`{"execution_id":"ex_1","generation":2,"handle":"ex_1","agent":{"name":"claude-host"},"unread":1}`))
		case "/v1/claims/touch":
			_, _ = w.Write([]byte(`{"action":"deny","message":"held"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	sess, err := c.RegisterSession(context.Background(), SessionRequest{Client: "claude", SessionID: "s", Source: "hook"})
	if err != nil || sess.ExecutionID != "ex_1" || sess.Generation != 2 {
		t.Fatalf("%+v %v", sess, err)
	}
	if gotAuth != "Bearer tok" || got["session_id"] != "s" || got["source"] != "hook" {
		t.Fatalf("path %s auth %s body %#v", gotPath, gotAuth, got)
	}
	touch, err := c.Touch(context.Background(), TouchRequest{ExecutionID: "ex_1", Generation: 2, Paths: []string{"a.go"}, ToolCallID: "t"})
	if err != nil || touch.Action != "deny" || touch.Message != "held" {
		t.Fatalf("%+v %v", touch, err)
	}
	if err := c.EndExecution(context.Background(), "ex/1", 2); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/executions/ex%2F1/end" {
		t.Fatalf("end path %s", gotPath)
	}
	if got["generation"] != float64(2) {
		t.Fatalf("generation %#v", got["generation"])
	}
}

func TestCleartextRemoteRefused(t *testing.T) {
	c := &Client{Config: staticConfig{url: "http://example.com", token: "tok"}}
	_, err := c.RegisterSession(context.Background(), SessionRequest{SessionID: "s"})
	if err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatal(err)
	}
}

type staticConfig struct {
	url   string
	token string
}

func (s staticConfig) HubURL() string         { return s.url }
func (s staticConfig) Token() (string, error) { return s.token, nil }
