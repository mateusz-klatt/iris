package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LastHookAt was declared and never filled (gap review C4). Doctor now asks the hub when this
// host's hooks last reached it, and an older hub that cannot say leaves it empty.
func TestTheLastHookComesFromTheHubForThisHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   string
	}{
		{"a hub that knows", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"clients":{"codex":"2026-11-07T21:00:00Z"}}`))
		}, "2026-11-07T21:00:00Z"},
		{"a hub from before", func(w http.ResponseWriter) { http.NotFound(w, nil) }, ""},
		{"a broken answer", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{`)) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var host string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/hooks/seen":
					host = r.URL.Query().Get("host")
					tc.answer(w)
				default:
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer srv.Close()
			layout := doctorLayout(t)
			token := filepath.Join(layout.Home, "hub.token")
			if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "this-host"})
			if err != nil {
				t.Fatal(err)
			}
			if host != "this-host" {
				t.Fatalf("asked about %q", host)
			}
			for _, row := range res.Report.Clients {
				want := ""
				if row.Client == "codex" {
					want = tc.want
				}
				if row.LastHookAt != want {
					t.Fatalf("%s: %q, want %q", row.Client, row.LastHookAt, want)
				}
			}
			if tc.want != "" && !strings.Contains(res.Format(), "\tlast hook "+tc.want) {
				t.Fatal(res.Format())
			}
		})
	}
	// Unreachable once the token is checked, the hub says nothing either.
	if got := lastHooks(context.Background(), "http://127.0.0.1:9", "t", "h", http.DefaultClient); got != nil {
		t.Fatal(got)
	}
	// Without an override, doctor names this host as iris hook does, by os.Hostname.
	if want, _ := os.Hostname(); (Options{}).host() != want {
		t.Fatalf("host %q, want %q", (Options{}).host(), want)
	}
}

// The hub's database grows with mail, which nothing prunes (ADR 006), so doctor says its size.
func TestDoctorSaysHowLargeTheHubDatabaseIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			_, _ = w.Write([]byte(`{"database_bytes":3145728}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	layout := doctorLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if res.DatabaseBytes != 3145728 || !strings.Contains(res.Format(), "hub: ok (database 3.0 MiB)") {
		t.Fatalf("%d\n%s", res.DatabaseBytes, res.Format())
	}
}
