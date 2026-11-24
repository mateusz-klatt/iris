package doctor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

func runEndedMarkerDoctor(t *testing.T, setup func(*hook.Sessions, time.Time)) (Result, string) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	layout := emptyLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setup(hook.NewSessions(token), now)

	var uploaded []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status", "/v1/agents", "/v1/hooks/seen":
			_, _ = io.WriteString(w, `{}`)
		case "/v1/ui/doctor":
			var err error
			uploaded, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	res, err := Run(context.Background(), Options{
		Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Now: func() time.Time { return now }, Dir: layout.Home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stored {
		t.Fatalf("doctor report was not stored: %s", res.StoreError)
	}
	return res, string(uploaded)
}

func writeEndedMarkerObservation(t *testing.T, sessions *hook.Sessions, marker, client string, at time.Time) {
	t.Helper()
	line, err := json.Marshal(hook.Failure{
		Time: at, Client: client, Event: "pre_tool", Stage: "observed", Error: hook.DamagedGenerationProof + " at " + marker,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(sessions.Dir()), "hook-observed.log")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRedactedMarkerCount(t *testing.T, res Result, upload, marker string, count int) Bundle {
	t.Helper()
	var stored Report
	if err := json.Unmarshal([]byte(upload), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.CurrentDamagedEndMarkers != count {
		t.Fatalf("uploaded current damaged marker count = %d, want %d", stored.CurrentDamagedEndMarkers, count)
	}
	bundle := NewBundle(res, "test", res.Report.CheckedAt)
	if bundle.CurrentDamagedEndMarkers != count {
		t.Fatalf("bundle current damaged marker count = %d, want %d", bundle.CurrentDamagedEndMarkers, count)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if marker != "" {
		for _, private := range []string{marker, filepath.Base(marker)} {
			if strings.Contains(upload, private) || strings.Contains(string(raw), private) {
				t.Fatalf("marker path %q escaped into upload or bundle", private)
			}
		}
	}
	return bundle
}

func assertCurrentEndMarkerDiagnostic(t *testing.T, report string) {
	t.Helper()
	for _, want := range []string{
		"damaged end marker prevents this session from registering",
		"no hub binding, no mail delivery and no claims",
		"hooks still allow edits and commands",
		"Stop that session's client, hooks, MCP shim and watch",
		"Verify the configured hub and that session's history",
		"verified ended generation (a single decimal)",
		"archive it (move it aside)",
		"restart the client",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("diagnostic omitted %q", want)
		}
	}
}

func TestDoctorFindsCurrentDamageBehindAnOldObservation(t *testing.T) {
	var marker string
	res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, now time.Time) {
		if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		marker = filepath.Join(sessions.Dir(), "old.ended")
		if err := os.WriteFile(marker, []byte("broken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, now.Add(-25*time.Hour))
	})
	if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 1 {
		t.Fatalf("exit = %d, current count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
	}
	report := res.Format()
	var claudeLine string
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, integrate.Claude+"\t") {
			claudeLine = line
		}
	}
	if !strings.Contains(claudeLine, "damaged generation proof at "+marker+" (still damaged)") {
		t.Fatalf("old observation did not attribute the current marker to its client: %q", claudeLine)
	}
	if strings.Contains(claudeLine, "observed in 24 h") || strings.Contains(report, "local sessions without a client observation") {
		t.Fatalf("old history changed its window or became local-only:\n%s", report)
	}
	assertCurrentEndMarkerDiagnostic(t, claudeLine)
	bundle := assertRedactedMarkerCount(t, res, upload, marker, 1)
	if len(bundle.Observations) != 0 {
		t.Fatalf("old observation entered the 24-hour bundle: %+v", bundle.Observations)
	}
}

func TestDoctorRechecksAnObservedMarkerOutsideTheCurrentSessionsDirectory(t *testing.T) {
	var marker string
	res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, now time.Time) {
		marker = filepath.Join(t.TempDir(), "fallback.ended")
		if err := os.WriteFile(marker, []byte("broken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, now.Add(-time.Hour))
	})
	if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 0 {
		t.Fatalf("exit = %d, current directory count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
	}
	report := res.Format()
	if !strings.Contains(report, "damaged generation proof at "+marker+" (still damaged)") || strings.Contains(report, "local sessions without a client observation") {
		t.Fatalf("observation-only damage was not re-checked and attributed:\n%s", report)
	}
	assertCurrentEndMarkerDiagnostic(t, report)
	bundle := assertRedactedMarkerCount(t, res, upload, marker, 0)
	if len(bundle.Observations) != 1 || bundle.Observations[0].Category != hook.DamagedGenerationProof {
		t.Fatalf("recent observation was not retained: %+v", bundle.Observations)
	}
}

func TestDoctorFindsCurrentDamageWithoutAnObservation(t *testing.T) {
	var marker string
	res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, _ time.Time) {
		if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		marker = filepath.Join(sessions.Dir(), "not-a-session-hash.ended")
		if err := os.WriteFile(marker, []byte("malformed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 1 {
		t.Fatalf("exit = %d, current count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
	}
	report := res.Format()
	local := "local sessions without a client observation"
	if !strings.Contains(report, local) || !strings.Contains(report, "damaged generation proof at "+marker+" (still damaged)") {
		t.Fatalf("unattributed marker did not get a local-only diagnostic:\n%s", report)
	}
	assertCurrentEndMarkerDiagnostic(t, report)
	bundle := assertRedactedMarkerCount(t, res, upload, marker, 1)
	if len(bundle.Observations) != 0 {
		t.Fatalf("scan invented a rolling-log observation: %+v", bundle.Observations)
	}
}

func TestDoctorCurrentMarkerScanHealthyCases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*testing.T, *hook.Sessions, time.Time) string
		unexpected []string
	}{
		{
			name: "repaired before doctor with an old observation",
			setup: func(t *testing.T, sessions *hook.Sessions, now time.Time) string {
				t.Helper()
				if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(sessions.Dir(), "repaired.ended")
				if err := os.WriteFile(marker, []byte("7\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, now.Add(-25*time.Hour))
				return marker
			},
			unexpected: []string{"still damaged", "local sessions without a client observation"},
		},
		{
			name: "valid marker and ignored entries",
			setup: func(t *testing.T, sessions *hook.Sessions, _ time.Time) string {
				t.Helper()
				if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(sessions.Dir(), "valid.ended")
				if err := os.WriteFile(marker, []byte("3\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sessions.Dir(), "not-a-marker"), []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
				nested := filepath.Join(sessions.Dir(), "nested")
				if err := os.Mkdir(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nested, "hidden.ended"), []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
				return marker
			},
			unexpected: []string{"damaged generation proof", "local sessions"},
		},
		{
			name: "missing sessions directory",
			setup: func(_ *testing.T, _ *hook.Sessions, _ time.Time) string {
				return ""
			},
			unexpected: []string{"local sessions"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marker string
			res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, now time.Time) {
				marker = tc.setup(t, sessions, now)
			})
			if res.ExitCode() != 0 || res.Report.CurrentDamagedEndMarkers != 0 {
				t.Fatalf("exit = %d, current count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
			}
			report := res.Format()
			for _, unexpected := range tc.unexpected {
				if strings.Contains(report, unexpected) {
					t.Errorf("healthy scan reported %q:\n%s", unexpected, report)
				}
			}
			assertRedactedMarkerCount(t, res, upload, marker, 0)
		})
	}
}

func TestDoctorCurrentMarkerScanRejectsDirectoriesAndDanglingLinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "directory", setup: func(t *testing.T, marker string) {
			t.Helper()
			if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dangling link", setup: symlinkToMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marker string
			res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, _ time.Time) {
				if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
					t.Fatal(err)
				}
				marker = filepath.Join(sessions.Dir(), tc.name+".ended")
				tc.setup(t, marker)
			})
			if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 1 {
				t.Fatalf("exit = %d, current count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
			}
			report := res.Format()
			if !strings.Contains(report, "local sessions without a client observation") || !strings.Contains(report, marker+" (still damaged)") {
				t.Fatalf("current %s was not graded as damaged:\n%s", tc.name, report)
			}
			assertCurrentEndMarkerDiagnostic(t, report)
			assertRedactedMarkerCount(t, res, upload, marker, 1)
		})
	}
}

func TestDoctorNotesAnUnreadableSessionsDirectory(t *testing.T) {
	res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, _ time.Time) {
		if err := os.WriteFile(sessions.Dir(), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if res.ExitCode() != 0 || res.Report.CurrentDamagedEndMarkers != 0 {
		t.Fatalf("exit = %d, current count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
	}
	if report := res.Format(); !strings.Contains(report, "local sessions: could not scan for damaged end markers") || strings.Contains(report, "still damaged") {
		t.Fatalf("unreadable sessions directory note:\n%s", report)
	}
	assertRedactedMarkerCount(t, res, upload, "", 0)
}
