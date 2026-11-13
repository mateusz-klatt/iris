package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// unusableLine is a Codex shim's observation of a call that named a session without a usable
// Execution, as the hook package's writer adds it.
func unusableLine(at time.Time) string {
	return fmt.Sprintf(`{"time":%q,"client":"codex","event":"mcp","stage":"observed","error":%q}`+"\n", at.Format(time.RFC3339), binding.ObservedUnusable)
}

// The hint for such calls follows doctor's own evidence, the hub's word on when a Codex hook from
// this host last reached it (ADR 017). Hooks that never ran call for the review in /hooks. Hooks
// that did run do not, so doctor says what else it can be. Without the hub's word doctor says it
// cannot tell.
func TestTheUnusableSessionHintFollowsTheHubsEvidence(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(filepath.Join(filepath.Dir(token), "hook-observed.log"), []byte(unusableLine(now.Add(-time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	review := "no Codex hook from this host reached the hub in those 24 h, so the hooks may not run: review them once in /hooks"
	ran := "a Codex hook from this host reached the hub at 2026-11-10T10:00:00Z, so hooks run here. Either the hub restarted and that session had not reattached, or this Codex gives its tool calls a different session id from the one it gives its hooks; in that case this version of Iris cannot bind the session the calls name. A session whose own hooks did not run while other sessions' hooks did looks the same to doctor"
	cannot := "doctor could not learn when a Codex hook from this host last reached the hub. It cannot tell"
	for _, tc := range []struct {
		name string
		seen map[string]string
		want string
	}{
		{"no Codex hook reached the hub", map[string]string{"claude": "2026-11-10T11:00:00Z"}, review},
		{"the last Codex hook is older than the 24 hours", map[string]string{"codex": "2026-11-09T11:00:00Z"}, review},
		{"a Codex hook reached the hub", map[string]string{"codex": "2026-11-10T10:00:00Z"}, ran},
		{"doctor could not ask the hub", nil, cannot},
		{"an answer doctor cannot read", map[string]string{"codex": "yesterday"}, cannot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Result{}
			res.Report.Clients = []ClientReport{{Client: integrate.Codex, Status: StatusOK}, {Client: integrate.Claude, Status: StatusOK}}
			if err := applyFailures(&res, token, now); err != nil {
				t.Fatal(err)
			}
			for i := range res.Report.Clients {
				unusableHint(&res.Report.Clients[i], tc.seen, now)
			}
			codex, claude := res.Report.Clients[0], res.Report.Clients[1]
			if codex.Status != StatusOK || !strings.HasPrefix(codex.Detail, "observed in 24 h: "+binding.ObservedUnusable+" (1); "+tc.want) {
				t.Fatalf("%+v", codex)
			}
			if tc.want == ran && strings.Contains(codex.Detail, "/hooks") {
				t.Fatalf("hooks that ran sent the person to /hooks: %s", codex.Detail)
			}
			if claude.Detail != "" {
				t.Fatalf("another client got the hint: %+v", claude)
			}
		})
	}
}

// Doctor gives the hint with what it learned from the hub, or without it when the hub could not
// be asked, and the report it stores carries it.
func TestDoctorExplainsAnUnusableSessionFromTheHub(t *testing.T) {
	hookAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	var stored string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hooks/seen":
			_, _ = fmt.Fprintf(w, `{"clients":{"codex":%q}}`, hookAt)
		case "/v1/ui/doctor":
			raw, _ := io.ReadAll(r.Body)
			stored = string(raw)
			w.WriteHeader(http.StatusOK)
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
	if err := os.WriteFile(filepath.Join(layout.Home, "hook-observed.log"), []byte(unusableLine(time.Now().Add(-time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	codexDetail := func(res Result) string {
		for _, row := range res.Report.Clients {
			if row.Client == integrate.Codex {
				return row.Detail
			}
		}
		t.Fatal("no Codex row")
		return ""
	}
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if detail := codexDetail(res); !strings.Contains(detail, "a Codex hook from this host reached the hub at "+hookAt+", so hooks run here") || !strings.Contains(res.Format(), "so hooks run here") {
		t.Fatalf("%s\n%s", detail, res.Format())
	}
	if !strings.Contains(stored, "so hooks run here") {
		t.Fatalf("the stored report lacks the hint: %s", stored)
	}
	srv.Close()
	res, err = Run(context.Background(), Options{Layout: layout, HubURL: srv.URL, TokenFile: token, HTTP: srv.Client(), Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if detail := codexDetail(res); !strings.Contains(detail, "doctor could not learn when a Codex hook") {
		t.Fatal(detail)
	}
}

// The bundle counts both observations a tool call's ids can lead to as kinds of their own.
func TestTheBundleKnowsTheCallObservations(t *testing.T) {
	for _, text := range []string{binding.ObservedTwoSessions, binding.ObservedUnusable} {
		if got := observationKind(text); got != text {
			t.Fatalf("%q is %q", text, got)
		}
	}
}

// A shim's rebind after a loss (ADR 027) is a kind of its own in the bundle, so a host whose
// sessions keep losing their generation shows it, rather than an anonymous "other".
func TestTheBundleKnowsTheRebindObservation(t *testing.T) {
	if got := observationKind(binding.ObservedRebound); got != binding.ObservedRebound {
		t.Fatalf("%q is %q", binding.ObservedRebound, got)
	}
}
