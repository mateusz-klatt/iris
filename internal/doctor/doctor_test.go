package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

func TestCodexNeedsHumanAndIsStored(t *testing.T) {
	var put []byte
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			w.WriteHeader(http.StatusOK)
		case "/v1/agents":
			auth = r.Header.Get("Authorization")
			if auth != "Bearer secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/v1/ui/doctor":
			put, _ = io.ReadAll(r.Body)
			if r.Header.Get("Authorization") != "Bearer secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	codex, _ := integrate.ByName(integrate.Codex)
	if err := codex.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{
		Layout:    layout,
		HubURL:    srv.URL,
		TokenFile: token,
		HTTP:      srv.Client(),
		Host:      "this-host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HubUp || !res.TokenOK || !res.Stored {
		t.Fatalf("hub=%v token=%v stored=%v detail=%s err=%s", res.HubUp, res.TokenOK, res.Stored, res.HubDetail, res.StoreError)
	}
	if auth != "Bearer secret" {
		t.Fatal(auth)
	}
	for _, row := range res.Report.Clients {
		if row.Client == integrate.Codex {
			if row.Status != StatusNeedsHuman || row.Detail != "Review hooks once in /hooks" {
				t.Fatalf("%+v", row)
			}
			continue
		}
		if row.Status != StatusNotConfigured {
			t.Fatalf("%s is %s", row.Client, row.Status)
		}
	}
	// The report names its host, so the hub keeps it beside other hosts' (ADR 007).
	if !strings.Contains(string(put), `"status":"needs_human"`) || !strings.Contains(string(put), `"host":"this-host"`) {
		t.Fatalf("put body: %s", put)
	}
	if res.ExitCode() != 0 {
		t.Fatal(res.Format())
	}
}

func TestRejectedTokenIsNotStored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ui/doctor" {
			t.Error("stored a report with a rejected token")
		}
		if r.URL.Path == "/v1/agents" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(token, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{
		Layout:    doctorLayout(t),
		HubURL:    srv.URL,
		TokenFile: token,
		HTTP:      srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TokenOK || res.Stored || res.ExitCode() == 0 {
		t.Fatalf("%+v", res)
	}
}

func TestRecentHookFailureIsSilent(t *testing.T) {
	var put []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ui/doctor" {
			put, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	home := t.TempDir()
	token := filepath.Join(home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := `{"time":"2026-11-06T12:00:00Z","client":"codex","event":"PreToolUse","stage":"touch","error":"hub client is not configured"}` + "\n"
	if err := os.WriteFile(filepath.Join(home, "hook-failures.log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := integrate.Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	now := func() time.Time { return time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC) }
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range res.Report.Clients {
		if row.Client == integrate.Codex {
			if row.Status != StatusSilent || !strings.Contains(row.Detail, "recent hook failure:") || !strings.Contains(row.Detail, "hub client is not configured") {
				t.Fatalf("%+v", row)
			}
			continue
		}
		if row.Status != StatusNotConfigured {
			t.Fatalf("%s is %s", row.Client, row.Status)
		}
	}
	if res.ExitCode() == 0 {
		t.Fatal("a recent hook failure should fail the run")
	}
	if !strings.Contains(string(put), `"status":"silent"`) {
		t.Fatalf("stored report: %s", put)
	}
}

func TestBrokenCanaryIsSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("IRIS_FAKE_STDOUT", "")
	t.Setenv("IRIS_FAKE_EXIT", "1")
	layout := doctorLayout(t)
	codex, _ := integrate.ByName(integrate.Codex)
	if err := codex.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range res.Report.Clients {
		if row.Client == integrate.Codex {
			if row.Status != StatusSilent {
				t.Fatalf("%+v", row)
			}
		}
	}
	if res.ExitCode() == 0 {
		t.Fatal("silent client should fail the run")
	}
}

func doctorLayout(t *testing.T) integrate.Layout {
	t.Helper()
	home := t.TempDir()
	iris, err := integrate.PlaceFake(home, "iris")
	if err != nil {
		t.Fatal(err)
	}
	return integrate.Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv:   func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if name == "iris" {
				return iris, nil
			}
			return "", errors.New("not found")
		},
		GOOS: runtime.GOOS,
	}
}

func TestDoctorReportsConfigurationSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	home := t.TempDir()
	layout := integrate.Layout{Home: home, GOOS: "linux", Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	token := defaultTokenFile(layout)
	path := filepath.Join(filepath.Dir(token), "client.json")
	if err := hubclient.WriteConfig(path, hubclient.FileConfig{Hub: srv.URL, TokenFile: token}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Layout: layout, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if res.HubURL != srv.URL || res.HubSource != "file ("+path+")" || !strings.Contains(res.Format(), "source: file (") {
		t.Fatal(res.Format())
	}
}

func TestFailuresCountOnlyWhenRecent(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	token := filepath.Join(t.TempDir(), "hub.token")
	log := filepath.Join(filepath.Dir(token), "hook-failures.log")
	line := func(client string, at time.Time) string {
		return fmt.Sprintf(`{"time":%q,"client":%q,"event":"pre_tool","stage":"session","error":"connection refused"}`+"\n",
			at.Format(time.RFC3339), client)
	}
	body := line(integrate.Claude, now.Add(-time.Hour)) +
		line(integrate.Codex, now.Add(-48*time.Hour))
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Result{}
	res.Report.Clients = []ClientReport{
		{Client: integrate.Claude, Status: StatusOK},
		{Client: integrate.Codex, Status: StatusOK},
	}
	if err := applyFailures(&res, token, now); err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSilent, StatusOK}
	for i, row := range res.Report.Clients {
		if row.Status != want[i] {
			t.Fatalf("%s: got %s, want %s", row.Client, row.Status, want[i])
		}
	}
}

// An observation is not a failure (gap review C4): the row keeps its status, and the note
// says what was seen in the last 24 hours and how often.
func TestObservationsAreNotedNotFailed(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	token := filepath.Join(t.TempDir(), "hub.token")
	log := filepath.Join(filepath.Dir(token), "hook-observed.log")
	line := func(client, text string, at time.Time) string {
		return fmt.Sprintf(`{"time":%q,"client":%q,"event":"preToolUse","stage":"observed","error":%q}`+"\n", at.Format(time.RFC3339), client, text)
	}
	body := line(integrate.Copilot, "unknown tool view", now.Add(-time.Hour)) +
		line(integrate.Copilot, "unknown tool view", now.Add(-2*time.Hour)) +
		line(integrate.Copilot, "cross-load: ran in a session of antigravity", now.Add(-time.Hour)) +
		line(integrate.Codex, "unknown tool old", now.Add(-48*time.Hour))
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Result{}
	res.Report.Clients = []ClientReport{
		{Client: integrate.Copilot, Status: StatusOK},
		{Client: integrate.Codex, Status: StatusOK},
	}
	if err := applyFailures(&res, token, now); err != nil {
		t.Fatal(err)
	}
	copilot, codex := res.Report.Clients[0], res.Report.Clients[1]
	if copilot.Status != StatusOK || copilot.Detail != "observed in 24 h: cross-load: ran in a session of antigravity (1); unknown tool view (2)" {
		t.Fatalf("%+v", copilot)
	}
	if codex.Status != StatusOK || codex.Detail != "" {
		t.Fatalf("an old observation counted: %+v", codex)
	}
}

// An observation log doctor cannot read is said, as an unreadable failure log is.
func TestAnUnreadableObservationLogIsReported(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.Mkdir(filepath.Join(filepath.Dir(token), "hook-observed.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := Result{}
	res.Report.Clients = []ClientReport{{Client: integrate.Copilot, Status: StatusOK}}
	if err := applyFailures(&res, token, time.Now()); err == nil {
		t.Fatal("a directory in place of the observation log was read as empty")
	}
}
