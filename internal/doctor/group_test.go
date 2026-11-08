package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// A crash loop writes the same failure many times. Doctor names each kind once, with its count
// and when it last happened, newest first, and stops after a few kinds, so the line says what
// broke instead of repeating it.
func TestRepeatedFailuresAreGroupedNewestFirst(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	line := func(event, stage, text string, ago time.Duration) string {
		return fmt.Sprintf(`{"time":%q,"client":%q,"event":%q,"stage":%q,"error":%q}`+"\n", now.Add(-ago).Format(time.RFC3339), integrate.Codex, event, stage, text)
	}
	var log strings.Builder
	for i := 0; i < 150; i++ {
		log.WriteString(line("PreToolUse", "shell session", "dependency failure (*errors.errorString)", 8*time.Hour-time.Duration(i)*time.Second))
	}
	log.WriteString(line("SessionStart", "session", "connection refused", 2*time.Hour))
	log.WriteString(line("SessionStart", "session", "connection refused", 30*time.Minute))
	for i, stage := range []string{"client", "decode", "hub", "paths", "touch"} {
		log.WriteString(line("PreToolUse", stage, "hook deadline exceeded", 20*time.Hour+time.Duration(i)*time.Minute))
	}
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(filepath.Join(filepath.Dir(token), "hook-failures.log"), []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Result{Report: Report{Clients: []ClientReport{{Client: integrate.Codex, Status: StatusOK}}}}
	if err := applyFailures(&res, token, now); err != nil {
		t.Fatal(err)
	}
	want := "recent hook failure: SessionStart session: connection refused ×2, last at 11:30 UTC; " +
		"PreToolUse shell session: dependency failure (*errors.errorString) ×150, last at 04:02 UTC; " +
		"PreToolUse client: hook deadline exceeded, at 16:00 UTC; " +
		"PreToolUse decode: hook deadline exceeded, at 15:59 UTC; " +
		"PreToolUse hub: hook deadline exceeded, at 15:58 UTC; and 2 more"
	if got := res.Report.Clients[0]; got.Status != StatusSilent || got.Detail != want {
		t.Fatalf("%s: %s\nwant %s", got.Status, got.Detail, want)
	}
}
