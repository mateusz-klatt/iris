package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

func TestDoctorWarnsWhenCursorCrossLoadsClaude(t *testing.T) {
	mismatch := binding.ClientMismatchObservation(integrate.Claude, integrate.Cursor)
	crossLoad := "cross-load: ran in a session of cursor"
	for _, tc := range []struct {
		name         string
		observations []string
		warn         bool
	}{
		{name: "MCP mismatch", observations: []string{mismatch}, warn: true},
		{name: "hook cross-load", observations: []string{crossLoad}, warn: true},
		{name: "both", observations: []string{mismatch, crossLoad}, warn: true},
		{name: "another mismatch", observations: []string{binding.ClientMismatchObservation(integrate.Claude, integrate.Grok)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
			token := filepath.Join(t.TempDir(), "hub.token")
			var log strings.Builder
			for _, observation := range tc.observations {
				fmt.Fprintf(&log, `{"time":%q,"client":"claude","event":"mcp","stage":"observed","error":%q}`+"\n", now.Add(-time.Hour).Format(time.RFC3339), observation)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(token), "hook-observed.log"), []byte(log.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			res := Result{HubUp: true, TokenOK: true, Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}}}
			if err := applyFailures(&res, token, now); err != nil {
				t.Fatal(err)
			}
			row := res.Report.Clients[0]
			if row.Status != StatusOK {
				t.Fatalf("an observation became a failing grade: %+v", row)
			}
			for _, observation := range tc.observations {
				if !strings.Contains(row.Detail, observation+" (1)") {
					t.Errorf("doctor omitted observation %q: %q", observation, row.Detail)
				}
			}
			warned := strings.Contains(row.Detail, cursorClaudeCrossLoadWarning)
			if warned != tc.warn {
				t.Fatalf("warning present %v, want %v: %q", warned, tc.warn, row.Detail)
			}
			if tc.warn {
				for _, text := range []string{"agent mcp disable plugin-iris-iris", "~/.cursor/projects/<project>/mcp-disabled.json", "only control", "Iris cannot stop Cursor from spawning the plugin's processes"} {
					if !strings.Contains(res.Format(), text) {
						t.Errorf("doctor output omits %q:\n%s", text, res.Format())
					}
				}
			}
		})
	}
}

func TestBundleClassifiesClientMismatchWithoutItsDetails(t *testing.T) {
	text := binding.ClientMismatchObservation("claude", "cursor")
	if got := observationKind(text); got != binding.ObservedClientMismatch {
		t.Fatalf("%q became %q", text, got)
	}
}
