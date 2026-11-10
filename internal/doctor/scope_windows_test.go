package doctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Windows hooks keep no breaker and always run full scope (ADR 018), so doctor reports no shell
// scope there, whatever the session memory holds.
func TestWindowsReportsNoShellScope(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	sessions := filepath.Join(filepath.Dir(token), "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "breaker-0123"), []byte(`{"v":1,"worktree":"C:\\w","scope":"reduced","next_full_ms":9999999999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if lines := scopeReport(token, time.Now()); lines != nil {
		t.Fatalf("lines %q", lines)
	}
}
