//go:build !windows

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

// Each worktree's line says which scope its breaker suggests and why, and the last full status
// recorded as succeeding there. A reduced worktree whose cooldown is over still says the breaker
// tripped, with the full attempt to come, and the limits follow the lines (ADR 018 decision 6).
func TestDoctorNamesEachWorktreesShellScope(t *testing.T) {
	next := time.Date(2026, 11, 10, 12, 1, 0, 0, time.UTC)
	finished := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	const reason = "a full git status passed its 300ms budget"
	got := scopeLines([]hook.ScopeHint{
		{Worktree: "/a", LastFullOK: finished},
		{Worktree: "/b", Reduced: true, Due: true, Reason: reason, NextFull: next},
		{Worktree: "/c", Reduced: true, Reason: reason, NextFull: next, LastFullOK: finished},
	})
	want := []string{
		"/a: the breaker suggests full scope; last recorded full status that succeeded: 2026-11-10T11:00:00Z",
		"/b: the breaker suggests reduced scope until 2026-11-10T12:01:00Z, because " + reason + "; last recorded full status that succeeded: none",
		"/c: the breaker tripped because " + reason + ", and its next shell command tries full scope again; last recorded full status that succeeded: 2026-11-10T11:00:00Z",
		scopeLimits,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, limit := range []string{"does not see tracked edits inside submodules", "neither warned about nor claimed", "still runs full scope where its hub does not take", "a later change to the hint does not alter that choice"} {
		if !strings.Contains(scopeLimits, limit) {
			t.Fatalf("the limits do not say %q", limit)
		}
	}
	if lines := scopeLines(nil); lines != nil {
		t.Fatalf("no breakers: %v", lines)
	}
}

// Doctor reads the breakers hooks keep beside the token file, as a hook reads them.
func TestDoctorReadsTheBreakersBesideTheToken(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	if lines := scopeReport(token, time.Now()); lines != nil {
		t.Fatalf("no memory: %v", lines)
	}
	sessions := filepath.Join(filepath.Dir(token), "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	raw := fmt.Sprintf(`{"v":1,"worktree":"/w","scope":"reduced","reason":"a full git status passed its 300ms budget","tripped_ms":%d,"next_full_ms":%d,"cooldown_ms":60000,"last_full_ok_ms":0}`, now.UnixMilli(), now.Add(time.Minute).UnixMilli())
	if err := os.WriteFile(filepath.Join(sessions, "breaker-0123"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := scopeReport(token, now)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "/w: the breaker suggests reduced scope until 2026-11-10T12:01:00Z") || lines[1] != scopeLimits {
		t.Fatalf("lines %q", lines)
	}
	if out := (Result{Scope: lines}).Format(); !strings.Contains(out, "shell scope: /w: the breaker suggests reduced scope until") || !strings.Contains(out, "shell scope: limits:") {
		t.Fatalf("not printed:\n%s", out)
	}
}
