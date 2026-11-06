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
	live := 0
	res, err := Run(context.Background(), Options{
		Layout:    layout,
		HubURL:    srv.URL,
		TokenFile: token,
		HTTP:      srv.Client(),
		LiveRun: func(context.Context, string, []string) error {
			live++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live ran %d times", live)
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
	if !strings.Contains(string(put), `"status":"needs_human"`) {
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
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client()})
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

func TestLiveIsOptIn(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := doctorLayout(t)
	iris, err := layout.LookPath("iris")
	if err != nil {
		t.Fatal(err)
	}
	layout.LookPath = func(name string) (string, error) {
		if name == "iris" || name == "grok" {
			return iris, nil
		}
		return "", errors.New("not found")
	}
	grok, _ := integrate.ByName(integrate.Grok)
	if err := grok.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opt := Options{
		Layout: layout,
		HubURL: "http://127.0.0.1:1",
		LiveRun: func(_ context.Context, name string, args []string) error {
			calls++
			for _, a := range args {
				if strings.Contains(a, "trust") || strings.Contains(a, "always-approve") || strings.Contains(a, "dangerously") {
					t.Fatalf("trust bypass: %s %v", name, args)
				}
			}
			return nil
		},
	}
	res, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	opt.Live = true
	res, err = Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("live calls %d\n%s", calls, res.Format())
	}
}

func TestLiveArgsHaveNoTrustBypass(t *testing.T) {
	banned := []string{"--trust", "--always-approve", "--dangerously-skip-permissions", "--trust-folder"}
	for _, name := range integrate.Names {
		_, args, err := LiveArgs(name, "iris")
		if err != nil {
			t.Fatal(name, err)
		}
		joined := strings.Join(args, " ")
		for _, bad := range banned {
			if strings.Contains(joined, bad) {
				t.Fatalf("%s live args contain %s: %s", name, bad, joined)
			}
		}
	}
}

func TestLiveClosesStdin(t *testing.T) {
	t.Setenv("IRIS_FAKE_REQUIRE_CLOSED_STDIN", "1")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	bin, err := integrate.PlaceFake(t.TempDir(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Layout: integrate.Layout{LookPath: func(name string) (string, error) {
		if name == "codex" {
			return bin, nil
		}
		return "", errors.New("not found")
	}}}
	// Generous: the first run of a fresh binary on Windows waits for the virus scanner, and
	// a killed process there looks exactly like the failure this test is looking for.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runLive(ctx, opt, integrate.Codex); err != nil {
		t.Fatal(err)
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
