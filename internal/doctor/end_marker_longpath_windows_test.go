package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

func TestDoctorGradesLongEndMarkerPathHealthy(t *testing.T) {
	layout := emptyLayout(t)
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 235))
	token := filepath.Join(deep, "hub.token")
	sessions := hook.NewSessions(token)
	if err := os.MkdirAll(`\\?\`+sessions.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(`\\?\`+token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(sessions.Dir(), "0123456789abcdef0123456789abcdef.ended")
	if len(marker) <= 260 {
		t.Fatalf("test marker path has only %d characters", len(marker))
	}
	if err := os.WriteFile(`\\?\`+marker, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	res, upload := runEndedMarkerDoctorWithOptions(t, layout, Options{TokenFile: token}, now)
	if res.ExitCode() != 0 || res.Report.CurrentDamagedEndMarkers != 0 || res.Report.EndMarkerScanIncomplete {
		t.Fatalf("long-path doctor result = %+v", res)
	}
	if report := res.Format(); strings.Contains(report, "damaged generation proof") {
		t.Fatalf("healthy long marker was classified as damaged:\n%s", report)
	}
	assertRedactedMarkerCount(t, res, upload, marker, 0)
}

func TestDoctorPreservesWin32Aliases(t *testing.T) {
	for _, long := range []bool{false, true} {
		name := "short trailing dot"
		if long {
			name = "long trailing dot"
		}
		t.Run(name, func(t *testing.T) {
			layout := emptyLayout(t)
			root := t.TempDir()
			component := "iris"
			if long {
				component = strings.Repeat("d", 235)
			}
			actualBase := filepath.Join(root, component)
			actualSessions := filepath.Join(actualBase, "sessions")
			if err := os.MkdirAll(`\\?\`+actualSessions, 0o700); err != nil {
				t.Fatal(err)
			}
			actualToken := filepath.Join(actualBase, "hub.token")
			if err := os.WriteFile(`\\?\`+actualToken, []byte("test-only-credential\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(actualSessions, "0123456789abcdef0123456789abcdef.ended")
			if err := os.WriteFile(`\\?\`+marker, []byte("2\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			aliasToken := root + `\` + component + `.\hub.token`
			aliasMarker := filepath.Join(hook.NewSessions(aliasToken).Dir(), filepath.Base(marker))
			if long && len(aliasMarker) <= 260 {
				t.Fatalf("alias marker has only %d characters", len(aliasMarker))
			}
			if _, err := os.Stat(aliasMarker); err != nil {
				t.Fatalf("os.Stat rejected the alias: %v", err)
			}
			now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
			res, upload := runEndedMarkerDoctorWithOptions(t, layout, Options{TokenFile: aliasToken}, now)
			if res.ExitCode() != 0 || res.Report.CurrentDamagedEndMarkers != 0 || res.Report.EndMarkerScanIncomplete {
				t.Fatalf("alias doctor result = %+v", res)
			}
			if report := res.Format(); strings.Contains(report, "damaged generation proof") {
				t.Fatalf("healthy alias was classified as damaged:\n%s", report)
			}
			assertRedactedMarkerCount(t, res, upload, marker, 0)
		})
	}
}
