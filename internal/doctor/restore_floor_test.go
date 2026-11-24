package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestDoctorExplainsWhyAHookDidNotRetryRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		oldHub        bool
		damaged       bool
	}{
		{"old hub", `{}`, true, false},
		{"restored hub with lower history", `{"highest_generation":1}`, false, false},
		{"damaged local proof", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registrations atomic.Int32
			uploaded := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/sessions":
					registrations.Add(1)
					var reg hubclient.SessionRequest
					if err := json.NewDecoder(r.Body).Decode(&reg); err != nil || reg.Generation != 3 {
						t.Errorf("registration = %+v, %v", reg, err)
					}
					w.WriteHeader(http.StatusConflict)
					_, _ = fmt.Fprintf(w, `{"error":{"code":"lost","details":%s}}`, tc.details)
				case "/v1/status", "/v1/agents", "/v1/hooks/seen":
					_, _ = fmt.Fprint(w, `{}`)
				case "/v1/ui/doctor":
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read doctor upload: %v", err)
					}
					uploaded <- string(body)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("the hook tried to use an Execution via %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			layout := doctorLayout(t)
			token := filepath.Join(layout.Home, "hub.token")
			if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sessions := filepath.Join(layout.Home, "sessions")
			if err := os.Mkdir(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte("claude\x00restore-test"))
			proof := filepath.Join(sessions, hex.EncodeToString(sum[:16]))
			proofBytes, wantRegistrations := "3", int32(1)
			if tc.damaged {
				proofBytes, wantRegistrations = "not a generation", 0
			}
			if err := os.WriteFile(proof, []byte(proofBytes), 0o600); err != nil {
				t.Fatal(err)
			}
			getenv := func(key string) string {
				return map[string]string{"IRIS_TOKEN_FILE": token, "IRIS_HUB_URL": srv.URL}[key]
			}
			payload := fmt.Sprintf(`{"session_id":"restore-test","cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"a.go"}}`, filepath.ToSlash(layout.Home))
			runner := hook.Runner{
				ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(payload), Getenv: getenv,
				Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: layout.Home}, nil },
				Host: func() string { return "local" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
				Hub: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}, HTTP: srv.Client()},
			}
			out := runner.Run()
			if out.Exit != 0 || !json.Valid(out.Stdout) || strings.Contains(string(out.Stdout), "deny") || registrations.Load() != wantRegistrations {
				t.Fatalf("output = %+v, registrations = %d", out, registrations.Load())
			}
			if raw, err := os.ReadFile(proof); err != nil || strings.TrimSpace(string(raw)) != proofBytes {
				t.Fatalf("proof = %q, %v", raw, err)
			}
			result, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "local", Dir: layout.Home})
			if err != nil {
				t.Fatal(err)
			}
			report := result.Format()
			if !strings.Contains(report, "restore_floor_refused") || strings.Contains(report, "hub predates restore floor") != tc.oldHub {
				t.Fatalf("doctor report:\n%s", report)
			}
			if !result.Stored {
				t.Fatalf("doctor report was not stored: %s", result.StoreError)
			}
			body := <-uploaded
			want := BundleCount{Client: "claude", Stage: "observed", Category: "restore_floor_refused", Count: 1}
			if tc.damaged {
				want.Category = "damaged generation proof"
				if !strings.Contains(report, "damaged generation proof at "+proof) {
					t.Fatalf("local diagnostic omitted the damaged file:\n%s", report)
				}
				if !strings.Contains(body, "damaged generation proof (1)") || strings.Contains(body, filepath.Base(proof)) {
					t.Fatalf("upload did not count or redact the observation: %s", body)
				}
			} else if !strings.Contains(report, "left the session unregistered") {
				t.Fatalf("doctor omitted the refusal explanation:\n%s", report)
			}
			bundle := NewBundle(result, "test", time.Now())
			if len(bundle.Observations) != 1 || bundle.Observations[0] != want {
				t.Fatalf("bundle observations = %+v", bundle.Observations)
			}
		})
	}
}

func TestDamagedProofDiagnosticEscapesControlCharacters(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	path := filepath.Join(dir, "sessions", "proof") + "\n\x1b[1A"
	log, err := json.Marshal(hook.Failure{Time: now, Client: "claude", Stage: "observed", Error: "damaged generation proof at " + path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hook-observed.log"), append(log, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Result{Report: Report{Clients: []ClientReport{{Client: "claude", Status: StatusOK}}}}
	if err := applyFailures(&res, token, now); err != nil {
		t.Fatal(err)
	}
	if report := res.Format(); strings.Contains(report, path) || !strings.Contains(report, fmt.Sprintf("damaged generation proof at %q", path)) {
		t.Fatalf("unsafe path diagnostic %q", report)
	}
}

func TestDoctorReportsDamagedEndMarkerLocallyWithoutLeakingPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		event      string
		unreadable bool
		resolution string
	}{
		{"SessionEnd with malformed marker", "SessionEnd", false, ""},
		{"SessionEnd with directory marker", "SessionEnd", true, ""},
		{"PreToolUse with malformed marker", "PreToolUse", false, ""},
		{"PreToolUse with directory marker", "PreToolUse", true, ""},
		{"PreToolUse with repaired marker", "PreToolUse", false, "repaired"},
		{"PreToolUse with archived marker", "PreToolUse", false, "archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := doctorLayout(t)
			token := filepath.Join(layout.Home, "hub.token")
			if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sessions := filepath.Join(layout.Home, "sessions")
			if err := os.Mkdir(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte("claude\x00end-marker-test"))
			proof := filepath.Join(sessions, hex.EncodeToString(sum[:16]))
			marker := proof + ".ended"
			if err := os.WriteFile(proof, []byte("3"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.unreadable {
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(marker, []byte("damaged\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			var registrations atomic.Int32
			uploaded := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/sessions":
					registrations.Add(1)
					_, _ = fmt.Fprint(w, `{"execution_id":"ex_end","generation":3,"agent":{"id":"ag_1","name":"claude-local-iris"}}`)
				case "/v1/executions/ex_end/end":
					_, _ = fmt.Fprint(w, `{}`)
				case "/v1/status", "/v1/agents", "/v1/hooks/seen":
					_, _ = fmt.Fprint(w, `{}`)
				case "/v1/ui/doctor":
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read doctor upload: %v", err)
					}
					uploaded <- string(body)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			getenv := func(key string) string {
				return map[string]string{"IRIS_TOKEN_FILE": token, "IRIS_HUB_URL": srv.URL}[key]
			}
			payload := fmt.Sprintf(`{"session_id":"end-marker-test","cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"a.go"}}`, filepath.ToSlash(layout.Home))
			runner := hook.Runner{
				ClientName: "claude", EventName: tc.event, Stdin: strings.NewReader(payload), Getenv: getenv,
				Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: layout.Home}, nil },
				Host: func() string { return "local" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
				Hub: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}, HTTP: srv.Client()},
			}
			out := runner.Run()
			if out.Exit != 0 || !json.Valid(out.Stdout) || strings.Contains(string(out.Stdout), "deny") {
				t.Fatalf("hook output = %+v", out)
			}
			if registrations.Load() != 0 {
				t.Fatalf("damaged marker allowed %d registrations", registrations.Load())
			}
			if raw, err := os.ReadFile(proof); err != nil || strings.TrimSpace(string(raw)) != "3" {
				t.Fatalf("damaged marker changed retained proof: %q, %v", raw, err)
			}
			switch tc.resolution {
			case "repaired":
				if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "archived":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			}

			result, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "local", Dir: layout.Home})
			if err != nil {
				t.Fatal(err)
			}
			wantExit, wantState := 1, "still damaged"
			if tc.resolution != "" {
				wantExit, wantState = 0, tc.resolution+" since"
			}
			if result.ExitCode() != wantExit {
				t.Fatalf("doctor exit code = %d, want %d for %s:\n%s", result.ExitCode(), wantExit, wantState, result.Format())
			}
			report := result.Format()
			if !strings.Contains(report, "damaged generation proof at "+marker+" ("+wantState+")") { // gitleaks:allow (a test observation token and path, not a credential)
				t.Fatalf("local diagnostic omitted damaged marker path and current state:\n%s", report)
			}
			if tc.resolution == "" {
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
						t.Errorf("current damaged-marker diagnostic omitted %q:\n%s", want, report)
					}
				}
				if strings.Contains(report, "start a new client session") {
					t.Errorf("damaged marker retained generic refusal advice:\n%s", report)
				}
			} else if !strings.Contains(report, "no longer blocks registration") {
				t.Errorf("resolved marker was not reported as history:\n%s", report)
			}
			if strings.Contains(report, "restore_floor_refused") {
				t.Fatalf("local refusal without a hub call reported restore_floor_refused:\n%s", report)
			}
			if !result.Stored {
				t.Fatalf("doctor report was not stored: %s", result.StoreError)
			}
			body := <-uploaded
			if !strings.Contains(body, "damaged generation proof (1)") || strings.Contains(body, filepath.Base(marker)) || strings.Contains(body, filepath.Base(proof)) {
				t.Fatalf("upload did not count or redact marker path: %s", body)
			}
			if strings.Contains(body, "restore_floor_refused") {
				t.Fatalf("upload counted a hub refusal that never happened: %s", body)
			}
			wantObs := []BundleCount{{Client: "claude", Stage: "observed", Category: "damaged generation proof", Count: 1}}
			bundle := NewBundle(result, "test", time.Now())
			if fmt.Sprintf("%+v", bundle.Observations) != fmt.Sprintf("%+v", wantObs) {
				t.Fatalf("bundle observations = %+v, want %+v", bundle.Observations, wantObs)
			}
		})
	}
}

func TestDoctorNamesAContributingEndMarkerWithoutUploadingThePath(t *testing.T) {
	layout := doctorLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(layout.Home, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("claude\x00repeat-marker"))
	proof := filepath.Join(sessions, hex.EncodeToString(sum[:16]))
	marker := proof + ".ended"
	if err := os.WriteFile(proof, []byte("3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions":
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"error":{"code":"lost","details":{"highest_generation":1}}}`)
		case "/v1/status", "/v1/agents", "/v1/hooks/seen":
			_, _ = fmt.Fprint(w, `{}`)
		case "/v1/ui/doctor":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read doctor upload: %v", err)
			}
			uploaded <- string(body)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	getenv := func(key string) string {
		return map[string]string{"IRIS_TOKEN_FILE": token, "IRIS_HUB_URL": srv.URL}[key]
	}
	payload := fmt.Sprintf(`{"session_id":"repeat-marker","cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"a.go"}}`, filepath.ToSlash(layout.Home))
	out := hook.Runner{
		ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: layout.Home}, nil },
		Host: func() string { return "local" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}, HTTP: srv.Client()},
	}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "deny") {
		t.Fatalf("hook output = %+v", out)
	}
	if raw, err := os.ReadFile(proof); err != nil || strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("repeated refusal changed proof to %q, %v", raw, err)
	}
	result, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "local", Dir: layout.Home})
	if err != nil {
		t.Fatal(err)
	}
	report := result.Format()
	if !strings.Contains(report, "restore_floor_refused at "+marker) || !strings.Contains(report, "left the session unregistered") {
		t.Fatalf("local report omitted the marker or the refusal:\n%s", report)
	}
	body := <-uploaded
	if strings.Contains(body, filepath.Base(marker)) || strings.Contains(body, filepath.Base(proof)) || !strings.Contains(body, "restore_floor_refused") {
		t.Fatalf("upload leaked a path or dropped the refusal: %s", body)
	}
	if strings.Contains(body, "damaged generation proof") {
		t.Fatalf("a readable marker was reported as damaged: %s", body)
	}
	want := []BundleCount{{Client: "claude", Stage: "observed", Category: "restore_floor_refused", Count: 1}}
	if got := NewBundle(result, "test", time.Now()).Observations; fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("bundle observations = %+v", got)
	}
}

func TestDoctorNamesADanglingEndMarkerLocally(t *testing.T) {
	layout := doctorLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(layout.Home, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("claude\x00dangling-ended"))
	proof := filepath.Join(sessions, hex.EncodeToString(sum[:16]))
	marker := proof + ".ended"
	if err := os.WriteFile(proof, []byte("3"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkToMissing(t, marker)
	uploaded := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions":
			_, _ = fmt.Fprint(w, `{"execution_id":"ex_end","generation":3,"agent":{"id":"ag_1","name":"claude-local-iris"}}`)
		case "/v1/executions/ex_end/end":
			_, _ = fmt.Fprint(w, `{}`)
		case "/v1/status", "/v1/agents", "/v1/hooks/seen":
			_, _ = fmt.Fprint(w, `{}`)
		case "/v1/ui/doctor":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read doctor upload: %v", err)
			}
			uploaded <- string(body)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	getenv := func(key string) string {
		return map[string]string{"IRIS_TOKEN_FILE": token, "IRIS_HUB_URL": srv.URL}[key]
	}
	payload := fmt.Sprintf(`{"session_id":"dangling-ended","cwd":%q}`, filepath.ToSlash(layout.Home))
	out := hook.Runner{
		ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: layout.Home}, nil },
		Host: func() string { return "local" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: getenv}, HTTP: srv.Client()},
	}.Run()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "deny") {
		t.Fatalf("hook output = %+v", out)
	}
	if raw, err := os.ReadFile(proof); err != nil || strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("SessionEnd discarded proof over a dangling marker: %q, %v", raw, err)
	}
	if info, err := os.Lstat(marker); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("SessionEnd replaced the dangling marker: %v, %v", info, err)
	}
	result, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "local", Dir: layout.Home})
	if err != nil {
		t.Fatal(err)
	}
	report := result.Format()
	if !strings.Contains(report, "damaged generation proof at "+marker) {
		t.Fatalf("local diagnostic omitted the dangling marker:\n%s", report)
	}
	body := <-uploaded
	if !strings.Contains(body, "damaged generation proof (1)") || strings.Contains(body, filepath.Base(marker)) || strings.Contains(body, filepath.Base(proof)) {
		t.Fatalf("upload did not count or redact the dangling marker: %s", body)
	}
}

func TestStoredDetailOmitsAnEndMarkerPath(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	marker := filepath.Join(dir, "sessions", "ended") + "\n\x1b[1A"
	plain := filepath.Join(dir, "sessions", "plain.ended")
	var log []byte
	for _, text := range []string{
		hook.RestoreFloorHubTooOld + " at " + marker,
		hook.RestoreFloorRefused + " at " + plain,
	} {
		line, err := json.Marshal(hook.Failure{Time: now, Client: "claude", Stage: "observed", Error: text})
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(dir, "hook-observed.log"), log, 0o600); err != nil {
		t.Fatal(err)
	}
	res := Result{Report: Report{Clients: []ClientReport{{Client: "claude", Status: StatusOK}}}}
	if err := applyFailures(&res, token, now); err != nil {
		t.Fatal(err)
	}
	detail := res.Report.Clients[0].Detail
	report := res.Format()
	if strings.Contains(detail, marker) || strings.Contains(detail, plain) || strings.Contains(detail, "sessions") {
		t.Fatalf("stored detail names a marker path: %s", detail)
	}
	if !strings.Contains(detail, hook.RestoreFloorHubTooOld) || !strings.Contains(detail, "left the session unregistered") {
		t.Fatalf("stored detail dropped the refusal: %s", detail)
	}
	if strings.Contains(report, marker) || !strings.Contains(report, fmt.Sprintf("%s at %q", hook.RestoreFloorHubTooOld, marker)) || !strings.Contains(report, hook.RestoreFloorRefused+" at "+plain) {
		t.Fatalf("local report = %q", report)
	}
}
