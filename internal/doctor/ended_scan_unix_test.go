//go:build !windows

package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

func TestDoctorCurrentMarkerScanRejectsAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a marker without permission bits")
	}
	var marker string
	res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, _ time.Time) {
		if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		marker = filepath.Join(sessions.Dir(), "unreadable.ended")
		if err := os.WriteFile(marker, []byte("4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(marker, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(marker, 0o600) })
	})
	if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 1 {
		t.Fatalf("exit = %d, current count = %d", res.ExitCode(), res.Report.CurrentDamagedEndMarkers)
	}
	if report := res.Format(); !strings.Contains(report, "local sessions without a client observation") || !strings.Contains(report, marker+" (still damaged)") {
		t.Fatalf("unreadable marker was not graded as damaged:\n%s", report)
	}
	assertRedactedMarkerCount(t, res, upload, marker, 1)
}
