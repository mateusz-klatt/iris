package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// A shell reconcile that only ran past its budget is degraded, not broken: one slow git status
// on a cold worktree must not make a healthy client silent for a day. Many of them do deserve
// attention, and any failure that shows the hook broken still makes the client silent.
func TestFailuresAreGradedByStage(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	entry := func(client, stage, err string) string {
		return fmt.Sprintf(`{"time":%q,"client":%q,"event":"pre_tool","stage":%q,"error":%q}`+"\n",
			now.Add(-time.Hour).Format(time.RFC3339), client, stage, err)
	}
	overBudget := func(client string, n int) string {
		return strings.Repeat(entry(client, "shell status", "shell reconciliation passed its deadline"), n)
	}
	cases := []struct {
		name, log, status, detail string
		exit                      int
	}{
		{"one slow git status", overBudget(integrate.Claude, 1), StatusOK, "degraded: 1 shell reconciliation(s) ran past their budget", 0},
		{"ten are still degraded", overBudget(integrate.Claude, slowLimit), StatusOK, "degraded: 10", 0},
		{"more than ten are slow", overBudget(integrate.Claude, slowLimit+1), StatusSlow, "11 shell reconciliations ran past their budget (1s, or 300ms for a hub on this machine) in 24 h", 1},
		{"a broken hook beside slow ones", overBudget(integrate.Claude, 3) + entry(integrate.Claude, "decode", "invalid JSON at byte 1"), StatusSilent, "recent hook failure: pre_tool decode: invalid JSON at byte 1", 1},
		{"a lost session without proof", entry(integrate.Claude, "shell session", "hub request failed: lost"), StatusSilent, "shell session: hub request failed: lost", 1},
		{"a deadline outside a shell stage", entry(integrate.Claude, "touch", "shell reconciliation passed its deadline"), StatusSilent, "recent hook failure", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "hub.token")
			if err := os.WriteFile(filepath.Join(filepath.Dir(token), "hook-failures.log"), []byte(tc.log), 0o600); err != nil {
				t.Fatal(err)
			}
			res := Result{HubUp: true, TokenOK: true, Report: Report{Clients: []ClientReport{
				{Client: integrate.Claude, Status: StatusOK},
				{Client: integrate.Codex, Status: StatusOK},
			}}}
			if err := applyFailures(&res, token, now); err != nil {
				t.Fatal(err)
			}
			row := res.Report.Clients[0]
			if row.Status != tc.status || !strings.Contains(row.Detail, tc.detail) || res.ExitCode() != tc.exit {
				t.Fatalf("status %s exit %d: %s", row.Status, res.ExitCode(), row.Detail)
			}
			if other := res.Report.Clients[1]; other.Status != StatusOK || other.Detail != "" {
				t.Fatalf("another client was graded: %+v", other)
			}
		})
	}
}

// A degraded note joins what doctor already said about the row rather than replacing it.
func TestADegradedNoteKeepsTheRowsDetail(t *testing.T) {
	if got := joinDetail("hooks reviewed", "degraded: 1"); got != "hooks reviewed; degraded: 1" {
		t.Fatal(got)
	}
}

// Today's hub refuses a report with a status it does not know, so slow goes as silent and
// keeps its grade in the detail. Other rows and the local report are unchanged.
func TestTheHubIsSentSlowAsSilent(t *testing.T) {
	local := Report{Clients: []ClientReport{{Client: "claude", Status: StatusSlow, Detail: "11 shell"}, {Client: "codex", Status: StatusOK}}}
	sent := published(local)
	if sent.Clients[0].Status != StatusSilent || sent.Clients[0].Detail != "slow: 11 shell" || sent.Clients[1] != local.Clients[1] {
		t.Fatalf("%+v", sent.Clients)
	}
	if local.Clients[0].Status != StatusSlow {
		t.Fatal("publishing changed the local report")
	}
}

// A hub that knows "slow" gets it as is. One from before it answers 400, and doctor then
// sends the report again with slow as silent, so the report is still stored.
func TestSlowIsSentAsIsAndAsSilentToAnOlderHub(t *testing.T) {
	report := Report{Clients: []ClientReport{{Client: "claude", Status: StatusSlow, Detail: "11 shell"}}}
	for _, old := range []bool{false, true} {
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in Report
			_ = json.NewDecoder(r.Body).Decode(&in)
			got = append(got, in.Clients[0].Status)
			if old && in.Clients[0].Status == StatusSlow {
				http.Error(w, `{"error":{"code":"bad_request"}}`, http.StatusBadRequest)
			}
		}))
		err := storeReport(context.Background(), srv.URL, "t", report, srv.Client())
		srv.Close()
		want := []string{StatusSlow}
		if old {
			want = []string{StatusSlow, StatusSilent}
		}
		if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("old %v: sent %v, err %v", old, got, err)
		}
	}
	if hasSlow(Report{Clients: []ClientReport{{Status: StatusOK}}}) {
		t.Fatal("a report without slow reads as slow")
	}
}
