package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestIsCodeLooksThroughWrapping(t *testing.T) {
	lost := &Error{Code: "lost", Message: "recovery requires the prior session generation"}
	if !IsCode(lost, "lost") || !IsCode(fmt.Errorf("register: %w", lost), "lost") {
		t.Fatal("a hub error was not recognised")
	}
	if IsCode(lost, "conflict") || IsCode(errors.New("lost"), "lost") || IsCode(nil, "lost") {
		t.Fatal("only a hub error with that code matches")
	}
}

func TestRegistrationRestoreFloorRequiresAnIntegerInAConflict(t *testing.T) {
	for _, tc := range []struct {
		name, details  string
		status, want   int
		valid, present bool
	}{
		{"known", `,"details":{"highest_generation":3}`, 409, 3, true, true},
		{"no retained history", `,"details":{"highest_generation":0}`, 409, 0, true, true},
		{"old hub", `,"details":{}`, 409, 0, false, false},
		{"missing details", "", 409, 0, false, false},
		{"non-object details", `,"details":[]`, 409, 0, false, true},
		{"string", `,"details":{"highest_generation":"3"}`, 409, 0, false, true},
		{"decimal", `,"details":{"highest_generation":3.0}`, 409, 0, false, true},
		{"negative", `,"details":{"highest_generation":-1}`, 409, 0, false, true},
		{"overflow", `,"details":{"highest_generation":9223372036854775808}`, 409, 0, false, true},
		{"null", `,"details":{"highest_generation":null}`, 409, 0, false, true},
		{"wrong status", `,"details":{"highest_generation":3}`, 502, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"lost"%s}}`, tc.details)
			}))
			defer srv.Close()
			c := &Client{Config: staticConfig{url: srv.URL}}
			_, err := c.RegisterSession(context.Background(), SessionRequest{Generation: 3})
			var lost *Error
			if !errors.As(err, &lost) || lost.Code != "lost" {
				t.Fatalf("registration error: %v", err)
			}
			got, valid, present := lost.HighestGeneration()
			if got != tc.want || valid != tc.valid || present != tc.present {
				t.Fatalf("highest generation = %d, valid = %t, present = %t", got, valid, present)
			}
		})
	}
}

// A session outside git registers on its own route. A hub without that route answers not_found
// with "unknown API route", which OlderHub tells from every other refusal (ADR 034).
func TestAnOutsideSessionRegistersOnItsRoute(t *testing.T) {
	older := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/v1/sessions/outside":
			w.WriteHeader(http.StatusTeapot)
		case older:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown API route"}}`))
		default:
			_, _ = w.Write([]byte(`{"execution_id":"ex_1","generation":1,"outside_git":true}`))
		}
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	sess, err := c.RegisterOutsideSession(context.Background(), SessionRequest{Client: "codex", SessionID: "s"})
	if err != nil || sess.ExecutionID != "ex_1" || !sess.OutsideGit {
		t.Fatalf("%+v %v", sess, err)
	}
	older = true
	_, err = c.RegisterOutsideSession(context.Background(), SessionRequest{Client: "codex", SessionID: "s"})
	if !OlderHub(err) || !OlderHub(fmt.Errorf("register: %w", err)) {
		t.Fatalf("an older hub was not recognised: %v", err)
	}
	for _, other := range []error{&Error{Code: "not_found", Message: "unknown repository"}, &Error{Code: "unauthorized", Message: "unknown API route"}, context.DeadlineExceeded} {
		if OlderHub(other) {
			t.Fatalf("%v taken for an older hub", other)
		}
	}
}

func TestAnOutsideRegistrationWithoutAnExecutionFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	if _, err := c.RegisterOutsideSession(context.Background(), SessionRequest{}); !errors.Is(err, ErrNoExecution) {
		t.Fatal(err)
	}
}

func TestEndedThroughReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details any
		want    bool
	}{
		{name: "strict floor", details: json.RawMessage(`{"reason":"ended_through","highest_generation":2}`), want: true},
		{name: "ordinary recovery", details: json.RawMessage(`{"highest_generation":3}`)},
		{name: "other reason", details: json.RawMessage(`{"reason":"retry_generation"}`)},
		{name: "malformed details", details: json.RawMessage(`{`)},
		{name: "programmatic error", details: map[string]any{"reason": EndedThroughReason}},
		{name: "missing details"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &Error{Code: "lost", Details: tc.details}
			if got := err.HasEndedThroughReason(); got != tc.want {
				t.Fatalf("HasEndedThroughReason() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestEndedThroughRefusalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details json.RawMessage
		want    bool
	}{
		{name: "new hub reason", details: json.RawMessage(`{"reason":"ended_through","highest_generation":3}`), want: true},
		{name: "older hub below floor", details: json.RawMessage(`{"highest_generation":1}`), want: true},
		{name: "equal generation is not strict evidence", details: json.RawMessage(`{"highest_generation":2}`)},
		{name: "higher generation keeps recovery handling", details: json.RawMessage(`{"highest_generation":3}`)},
		{name: "missing history"},
		{name: "malformed history", details: json.RawMessage(`{"highest_generation":"low"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &Error{Code: "lost", Details: tc.details}
			if got := err.IsEndedThroughRefusal(2); got != tc.want {
				t.Fatalf("IsEndedThroughRefusal(2) = %t, want %t", got, tc.want)
			}
		})
	}
}
