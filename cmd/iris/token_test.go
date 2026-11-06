package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type tokenConfig struct{ url string }

func (c tokenConfig) HubURL() string       { return c.url }
func (tokenConfig) Token() (string, error) { return "secret", nil }

func tokenClient(t *testing.T, handler http.HandlerFunc) *hubclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &hubclient.Client{Config: tokenConfig{server.URL}}
}

func TestTokenAddPrintsTheNewToken(t *testing.T) {
	var host string
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		host = body["host"]
		_, _ = w.Write([]byte(`{"token":"tok"}`))
	})
	var out bytes.Buffer
	if err := tokenCommand(context.Background(), client, []string{"add", "laptop"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "tok\n" || host != "laptop" {
		t.Fatalf("%q %q", out.String(), host)
	}
}

func TestTokenListShowsEachState(t *testing.T) {
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tokens":[{"name":"a"},{"name":"b","revoked_ms":5}]}`))
	})
	var out bytes.Buffer
	if err := tokenCommand(context.Background(), client, []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a\tactive\nb\trevoked\n" {
		t.Fatalf("%q", out.String())
	}
}

func TestTokenRevokeEscapesTheHost(t *testing.T) {
	var path string
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	})
	var out bytes.Buffer
	if err := tokenCommand(context.Background(), client, []string{"revoke", "a/b"}, &out); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/tokens/a%2Fb" || out.String() != "revoked a/b\n" {
		t.Fatalf("%q %q", path, out.String())
	}
}

func TestTokenCommandsReportTheHub(t *testing.T) {
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"operators only"}}`))
	})
	for _, args := range [][]string{{"add", "x"}, {"list"}, {"revoke", "x"}} {
		var out bytes.Buffer
		if err := tokenCommand(context.Background(), client, args, &out); err == nil || out.Len() != 0 {
			t.Errorf("%v: %v %q", args, err, out.String())
		}
	}
}

func TestTokenUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"add"}, {"list", "x"}, {"revoke"}, {"nope"}} {
		if err := runToken(args); err == nil || err.Error() != tokenUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := run([]string{"token"}); err == nil || !strings.Contains(err.Error(), "iris token add") {
		t.Fatal(err)
	}
}

func TestServeRefusesANegativeLimit(t *testing.T) {
	err := run([]string{"serve", "--token-rate", "-1"})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatal(err)
	}
}
