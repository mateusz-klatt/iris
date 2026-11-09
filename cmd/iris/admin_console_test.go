package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAdminConsoleSendsExactStdinPasswordAndPrintsSafeAccount(t *testing.T) {
	const password = "a sufficiently long password\n"
	var requests int
	client := tokenClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		path := "/v1/admin/console/accounts"
		if requests == 2 {
			path += "/reset"
		}
		if r.Method != http.MethodPost || r.URL.Path != path || r.URL.RawQuery != "" {
			t.Error("unexpected owner account route")
		}
		var got map[string]string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["username"] != "Owner" || got["password"] != password || len(got) != 2 {
			t.Error("owner request changed the password or included authority fields")
		}
		_, _ = w.Write([]byte(`{"account":{"id":"ca_test","username":"Owner","role":"operator"}}`))
	})
	for _, action := range []string{"create", "reset"} {
		var out bytes.Buffer
		if err := adminConsoleCommand(context.Background(), client, []string{action, "Owner"}, strings.NewReader(password), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "ca_test") || strings.Contains(out.String(), password) {
			t.Fatal("owner report missing account or exposed password")
		}
	}
	if requests != 2 {
		t.Fatal("missing owner setup request")
	}
}

type consoleBrokenReader struct{}

func (consoleBrokenReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type consoleBrokenWriter struct{}

func (consoleBrokenWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestAdminConsoleRefusesInvalidInputBeforeCallingHub(t *testing.T) {
	for _, args := range [][]string{nil, {"create"}, {"delete", "Owner"}, {"reset", "Owner", "password"}, {"create", "Owner", "--password", "secret"}} {
		if err := adminConsoleCommand(context.Background(), nil, args, nil, io.Discard); err == nil || err.Error() != adminConsoleUsage {
			t.Fatal("accepted password argument or invalid command")
		}
	}
	if err := adminConsoleCommand(context.Background(), nil, []string{"create", " Owner"}, nil, io.Discard); err == nil || err.Error() != "invalid console username" {
		t.Fatal("invalid username accepted")
	}
	for _, password := range []string{"too short", strings.Repeat("a", 1025), "a sufficiently long password\xff"} {
		if err := adminConsoleCommand(context.Background(), nil, []string{"create", "Owner"}, strings.NewReader(password), io.Discard); err == nil || !strings.HasPrefix(err.Error(), "new password must") {
			t.Fatal("invalid password accepted")
		}
	}
	if err := adminConsoleCommand(context.Background(), nil, []string{"reset", "Owner"}, consoleBrokenReader{}, io.Discard); err == nil || err.Error() != "could not read password from standard input" {
		t.Fatal("password read failure not sanitized")
	}
	if err := adminCommand(context.Background(), nil, []string{"console"}, io.Discard); err == nil || err.Error() != adminConsoleUsage {
		t.Fatal("console command not dispatched")
	}
}

func TestAdminConsoleReportsHubAndOutputFailures(t *testing.T) {
	if err := adminConsoleCommand(context.Background(), nil, []string{"reset", "Owner"}, strings.NewReader("long enough password"), io.Discard); err == nil || !strings.Contains(err.Error(), "hub client is not configured") {
		t.Fatal("hub failure hidden")
	}
	client := tokenClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"account":{"id":"ca_test"}}`)) })
	if err := adminConsoleCommand(context.Background(), client, []string{"create", "Owner"}, strings.NewReader("long enough password"), consoleBrokenWriter{}); err == nil || err.Error() != "write failed" {
		t.Fatal("output failure hidden")
	}
}
