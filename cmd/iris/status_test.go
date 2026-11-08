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

type statusConfig struct {
	url, token string
	err        error
}

func (c statusConfig) HubURL() string         { return c.url }
func (c statusConfig) Token() (string, error) { return c.token, c.err }

// statusHub answers as a hub does: the full document for the right token, only that it is up
// for any other.
func statusHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/status" && r.Header.Get("Authorization") == "Bearer right":
			_, _ = w.Write([]byte(`{"version":"dev","stream":"s1","boot_epoch":3,"database_bytes":4096,"counts":{"Agent":2,"Claim":0}}`))
		case r.URL.Path == "/v1/status":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func statusRun(t *testing.T, cfg statusConfig, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runStatusIO(context.Background(), args, cfg, &out, &errOut)
	return code, out.String(), errOut.String()
}

// With --json the hub's document is printed under "status" beside the hub it came from.
func TestStatusJSONCarriesTheHubsDocument(t *testing.T) {
	srv := statusHub(t)
	code, out, _ := statusRun(t, statusConfig{url: srv.URL, token: "right"}, "--json")
	var got struct {
		Hub    string         `json:"hub"`
		Status map[string]any `json:"status"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %s", code, err, out)
	}
	if got.Hub != srv.URL || got.Status["stream"] != "s1" || got.Status["database_bytes"] != float64(4096) || got.Error != nil {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(out, "right") {
		t.Fatal("the token was printed")
	}
}

// Without --json the same document is readable lines, sorted, with the counts on one line.
func TestStatusTextIsSortedLines(t *testing.T) {
	srv := statusHub(t)
	code, out, _ := statusRun(t, statusConfig{url: srv.URL, token: "right"})
	want := "hub: " + srv.URL + "\nboot_epoch: 3\ncounts: Agent=2 Claim=0\ndatabase_bytes: 4096\nstream: s1\nversion: dev\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A failure is one JSON object with its code under --json, and one line on stderr without it;
// both exit 1.
func TestStatusFailureInBothModes(t *testing.T) {
	srv := statusHub(t)
	cfg := statusConfig{url: srv.URL, token: "wrong"}
	code, out, _ := statusRun(t, cfg, "--json")
	var got struct {
		Error  hubclient.StatusFailure `json:"error"`
		Status any                     `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 1 || got.Error.Code != "token_rejected" || got.Status != nil {
		t.Fatalf("exit %d, %v: %s", code, err, out)
	}
	code, out, errOut := statusRun(t, cfg)
	if code != 1 || out != "" || errOut != "iris status: token_rejected: the hub is up but did not accept the token\n" {
		t.Fatalf("text: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestStatusUsage(t *testing.T) {
	if code, _, errOut := statusRun(t, statusConfig{}, "extra"); code != 2 || !strings.Contains(errOut, "usage: iris status") {
		t.Fatal(code, errOut)
	}
	if code, _, _ := statusRun(t, statusConfig{}, "--nope"); code != 2 {
		t.Fatal(code)
	}
}
