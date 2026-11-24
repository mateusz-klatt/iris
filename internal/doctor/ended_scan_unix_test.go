//go:build !windows

package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
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

func TestDoctorFIFOEndMarkerIsDamagedWithoutBlocking(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	dir := hook.NewSessions(token).Dir()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "x.ended")
	if err := syscall.Mkfifo(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result  Result
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		res := Result{
			HubUp: true, TokenOK: true,
			Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
		}
		start := time.Now()
		err := applyFailures(&res, token, time.Now())
		done <- outcome{result: res, err: err, elapsed: time.Since(start)}
	}()
	select {
	case <-time.After(500 * time.Millisecond):
		t.Fatal("doctor scan blocked on a FIFO end marker")
	case got := <-done:
		if got.elapsed >= 500*time.Millisecond {
			t.Fatalf("FIFO scan took %s", got.elapsed)
		}
		if got.err != nil || got.result.ExitCode() != 1 || got.result.Report.CurrentDamagedEndMarkers != 1 || got.result.Report.EndMarkerScanIncomplete {
			t.Fatalf("FIFO doctor result = %+v, err %v", got.result, got.err)
		}
		if report := got.result.Format(); !strings.Contains(report, "local sessions without a client observation") || !strings.Contains(report, marker+" (still damaged)") {
			t.Fatalf("FIFO was not reported as damaged:\n%s", report)
		}
	}
}
