package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	}{
		{"old hub", `{}`, true},
		{"restored hub with lower history", `{"highest_generation":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registrations atomic.Int32
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
				case "/v1/status", "/v1/agents", "/v1/hooks/seen", "/v1/ui/doctor":
					_, _ = fmt.Fprint(w, `{}`)
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
			if err := os.WriteFile(proof, []byte("3"), 0o600); err != nil {
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
			if out.Exit != 0 || !json.Valid(out.Stdout) || strings.Contains(string(out.Stdout), "deny") || registrations.Load() != 1 {
				t.Fatalf("output = %+v, registrations = %d", out, registrations.Load())
			}
			if raw, err := os.ReadFile(proof); err != nil || strings.TrimSpace(string(raw)) != "3" {
				t.Fatalf("proof = %q, %v", raw, err)
			}
			result, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "local", Dir: layout.Home})
			if err != nil {
				t.Fatal(err)
			}
			report := result.Format()
			if !strings.Contains(report, "restore_floor_refused") || !strings.Contains(report, "left the session unregistered") || strings.Contains(report, "hub predates restore floor") != tc.oldHub {
				t.Fatalf("doctor report:\n%s", report)
			}
			bundle := NewBundle(result, "test", time.Now())
			want := BundleCount{Client: "claude", Stage: "observed", Category: "restore_floor_refused", Count: 1}
			if len(bundle.Observations) != 1 || bundle.Observations[0] != want {
				t.Fatalf("bundle observations = %+v", bundle.Observations)
			}
		})
	}
}
