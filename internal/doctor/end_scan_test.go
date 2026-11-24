package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

type fakeMarkerInfo struct{ mode fs.FileMode }

func (f fakeMarkerInfo) Name() string       { return "marker" }
func (f fakeMarkerInfo) Size() int64        { return 0 }
func (f fakeMarkerInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeMarkerInfo) ModTime() time.Time { return time.Time{} }
func (f fakeMarkerInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeMarkerInfo) Sys() any           { return nil }

func TestEndMarkerRegularFileClassifiesMetadataPortably(t *testing.T) {
	regular := fakeMarkerInfo{}
	directory := fakeMarkerInfo{mode: fs.ModeDir}
	fifo := fakeMarkerInfo{mode: fs.ModeNamedPipe}
	symlink := fakeMarkerInfo{mode: fs.ModeSymlink}
	for _, tc := range []struct {
		name          string
		entry, target fs.FileInfo
		want          bool
	}{
		{name: "regular", entry: regular, want: true},
		{name: "directory", entry: directory},
		{name: "FIFO", entry: fifo},
		{name: "symlink to regular", entry: symlink, target: regular, want: true},
		{name: "symlink to directory", entry: symlink, target: directory},
		{name: "dangling symlink", entry: symlink},
		{name: "missing metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := endMarkerRegularFile(tc.entry, tc.target); got != tc.want {
				t.Fatalf("endMarkerRegularFile() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEndMarkerPathClassHandlesMetadataAndRacesPortably(t *testing.T) {
	regular := fakeMarkerInfo{}
	directory := fakeMarkerInfo{mode: fs.ModeDir}
	fifo := fakeMarkerInfo{mode: fs.ModeNamedPipe}
	symlink := fakeMarkerInfo{mode: fs.ModeSymlink}
	ioErr := errors.New("metadata failure")
	for _, tc := range []struct {
		name                string
		entry, target       fs.FileInfo
		entryErr, targetErr error
		secondEntryErr      error
		want                endMarkerFileClass
	}{
		{name: "regular", entry: regular, want: endMarkerFileRegular},
		{name: "directory", entry: directory, want: endMarkerFileDamaged},
		{name: "FIFO", entry: fifo, want: endMarkerFileDamaged},
		{name: "symlink to regular", entry: symlink, target: regular, want: endMarkerFileRegular},
		{name: "symlink to directory", entry: symlink, target: directory, want: endMarkerFileDamaged},
		{name: "dangling symlink", entry: symlink, targetErr: fs.ErrNotExist, want: endMarkerFileDamaged},
		{name: "symlink pruned during target stat", entry: symlink, targetErr: fs.ErrNotExist, secondEntryErr: fs.ErrNotExist, want: endMarkerFileAbsent},
		{name: "entry pruned", entryErr: fs.ErrNotExist, want: endMarkerFileAbsent},
		{name: "metadata error", entryErr: ioErr, want: endMarkerFileDamaged},
		{name: "target error", entry: symlink, targetErr: ioErr, want: endMarkerFileDamaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalLstat, originalStat := endMarkerLstat, endMarkerStat
			t.Cleanup(func() { endMarkerLstat, endMarkerStat = originalLstat, originalStat })
			calls := 0
			endMarkerLstat = func(string) (fs.FileInfo, error) {
				calls++
				if calls > 1 && tc.secondEntryErr != nil {
					return nil, tc.secondEntryErr
				}
				return tc.entry, tc.entryErr
			}
			endMarkerStat = func(string) (fs.FileInfo, error) { return tc.target, tc.targetErr }
			if got := endMarkerPathClass("marker.ended"); got != tc.want {
				t.Fatalf("endMarkerPathClass() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDoctorTreatsAnEntryPrunedBeforeMetadataAsAbsent(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	dir := hook.NewSessions(token).Dir()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "pruned.ended")
	if err := os.WriteFile(marker, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalLstat, originalRead := endMarkerLstat, endMarkerReadGeneration
	t.Cleanup(func() { endMarkerLstat, endMarkerReadGeneration = originalLstat, originalRead })
	endMarkerLstat = func(path string) (fs.FileInfo, error) {
		if path == marker {
			return nil, fs.ErrNotExist
		}
		return os.Lstat(path)
	}
	read := false
	endMarkerReadGeneration = func(string) (int, error) {
		read = true
		return 0, errors.New("must not be read")
	}
	res := Result{
		HubUp: true, TokenOK: true,
		Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
	}
	if err := applyFailures(&res, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	if read || res.ExitCode() != 0 || res.Report.CurrentDamagedEndMarkers != 0 || res.Report.EndMarkerScanIncomplete {
		t.Fatalf("pruned entry: read=%v result=%+v", read, res)
	}
	if report := res.Format(); strings.Contains(report, marker) {
		t.Fatalf("pruned entry was reported as damaged:\n%s", report)
	}
}

func TestEndMarkerSessionDirsDeduplicatesFilesystemIdentity(t *testing.T) {
	t.Run("injected identity without privileges", func(t *testing.T) {
		root := t.TempDir()
		primaryToken := filepath.Join(root, "primary", "hub.token")
		fallbackToken := filepath.Join(root, "fallback", "hub.token")
		primaryDir := hook.NewSessions(primaryToken).Dir()
		fallbackDir := hook.NewSessions(fallbackToken).Dir()
		for _, dir := range []string{primaryDir, fallbackDir} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if got := endMarkerSessionDirs(primaryToken, fallbackToken); len(got) != 2 {
			t.Fatalf("distinct directories = %v", got)
		}
		info, err := os.Stat(primaryDir)
		if err != nil {
			t.Fatal(err)
		}
		original := endMarkerStat
		t.Cleanup(func() { endMarkerStat = original })
		endMarkerStat = func(path string) (fs.FileInfo, error) {
			if path == fallbackDir {
				return info, nil
			}
			return os.Stat(path)
		}
		if got := endMarkerSessionDirs(primaryToken, fallbackToken); len(got) != 1 || got[0] != primaryDir {
			t.Fatalf("identity-deduplicated directories = %v", got)
		}
	})
	t.Run("symlink alias integration", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.MkdirAll(filepath.Join(realDir, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias")
		symlinkTo(t, realDir, alias)
		primaryToken := filepath.Join(realDir, "hub.token")
		fallbackToken := filepath.Join(alias, "hub.token")
		if got := endMarkerSessionDirs(primaryToken, fallbackToken); len(got) != 1 {
			t.Fatalf("symlink-aliased directories = %v", got)
		}
	})
}

func TestEndMarkerScanWaitUsesTheEarlierParentDeadline(t *testing.T) {
	if got := endMarkerScanWait(context.Background(), endMarkerScanTimeBudget); got != endMarkerScanTimeBudget {
		t.Fatalf("background wait = %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := endMarkerScanWait(ctx, endMarkerScanTimeBudget); got <= 0 || got >= endMarkerScanTimeBudget {
		t.Fatalf("parent-deadline wait = %s", got)
	}
	if got := endMarkerScanTimeoutReason(context.Background()); got != "time budget exceeded" {
		t.Fatalf("background timeout reason = %q", got)
	}
	cancel()
	if got := endMarkerScanTimeoutReason(ctx); got != "context cancelled" {
		t.Fatalf("cancelled timeout reason = %q", got)
	}
}

func TestDoctorMarkerScanDeadlineAndSingleFlight(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	layout := emptyLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := hook.NewSessions(token)
	if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(sessions.Dir(), "blocked.ended")
	if err := os.WriteFile(marker, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, now.Add(-time.Hour))

	original := endMarkerReadGeneration
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	endMarkerReadGeneration = func(string) (int, error) {
		startOnce.Do(func() { close(started) })
		<-release
		return 4, nil
	}
	defer func() {
		releaseRead()
		endMarkerReadGeneration = original
		endMarkerScanFlight <- struct{}{}
		<-endMarkerScanFlight
	}()

	res, upload := runEndedMarkerDoctorWithOptions(t, layout, Options{TokenFile: token, markerScanTimeout: 20 * time.Millisecond}, now)
	<-started
	assertIncompleteMarkerScan(t, res, "time budget exceeded", 1, 0)
	if report := res.Format(); !strings.Contains(report, "not rechecked") || !strings.Contains(report, "current state of an observed damaged end marker is unknown") {
		t.Fatalf("unreached observation was given a verdict:\n%s", report)
	}
	assertRedactedMarkerCount(t, res, upload, marker, 0)

	second := Result{
		HubUp: true, TokenOK: true,
		Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
	}
	secondStart := time.Now()
	if err := applyFailures(&second, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(secondStart); elapsed >= 100*time.Millisecond {
		t.Fatalf("second scan did not return immediately: %s", elapsed)
	}
	assertIncompleteMarkerScan(t, second, "previous end-marker scan still running", 0, 0)

	releaseRead()
	endMarkerScanFlight <- struct{}{}
	<-endMarkerScanFlight
	endMarkerReadGeneration = original
	later := Result{
		HubUp: true, TokenOK: true,
		Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
	}
	if err := applyFailures(&later, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	if later.ExitCode() != 0 || later.Report.EndMarkerScanIncomplete || later.Report.CurrentDamagedEndMarkers != 0 {
		t.Fatalf("later scan did not recover: %+v", later)
	}
}

func TestDoctorObservationRecheckHandlesTypeAndRemovalRaces(t *testing.T) {
	t.Run("non-regular observed marker", func(t *testing.T) {
		var marker string
		res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, now time.Time) {
			marker = filepath.Join(t.TempDir(), "directory.ended")
			if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
			writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, now.Add(-time.Hour))
		})
		if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 0 || res.Report.EndMarkerScanIncomplete {
			t.Fatalf("observed directory result = %+v", res)
		}
		if report := res.Format(); !strings.Contains(report, marker+" (still damaged)") {
			t.Fatalf("observed directory was not damaged:\n%s", report)
		}
		assertRedactedMarkerCount(t, res, upload, marker, 0)
	})
	t.Run("regular marker removed before generation read", func(t *testing.T) {
		original := endMarkerReadGeneration
		t.Cleanup(func() { endMarkerReadGeneration = original })
		endMarkerReadGeneration = func(string) (int, error) { return 0, nil }
		var marker string
		res, upload := runEndedMarkerDoctor(t, func(sessions *hook.Sessions, now time.Time) {
			marker = filepath.Join(t.TempDir(), "removed.ended")
			if err := os.WriteFile(marker, []byte("4\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, now.Add(-time.Hour))
		})
		if res.ExitCode() != 0 || res.Report.EndMarkerScanIncomplete {
			t.Fatalf("removed observation result = %+v", res)
		}
		if report := res.Format(); !strings.Contains(report, marker+" (archived since)") {
			t.Fatalf("removed observation was not archived:\n%s", report)
		}
		assertRedactedMarkerCount(t, res, upload, marker, 0)
	})
}

func TestDoctorMarkerScanCancellationAfterStart(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	dir := hook.NewSessions(token).Dir()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "cancelled.ended")
	if err := os.WriteFile(marker, []byte("4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := endMarkerReadGeneration
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	endMarkerReadGeneration = func(string) (int, error) {
		startOnce.Do(func() { close(started) })
		<-release
		return 4, nil
	}
	defer func() {
		releaseRead()
		endMarkerReadGeneration = original
		endMarkerScanFlight <- struct{}{}
		<-endMarkerScanFlight
	}()
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res := Result{
			HubUp: true, TokenOK: true,
			Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
		}
		err := applyFailuresFrom(ctx, &res, token, "", time.Now(), defaultEndMarkerScanConfig())
		done <- outcome{result: res, err: err}
	}()
	<-started
	cancel()
	select {
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled doctor scan did not return")
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		assertIncompleteMarkerScan(t, got.result, "context cancelled", 1, 0)
	}
}

func TestEndMarkerDirectoryOpenErrorIsOnlyANote(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	original := endMarkerOpen
	t.Cleanup(func() { endMarkerOpen = original })
	endMarkerOpen = func(string) (*os.File, error) { return nil, errors.New("injected open failure") }
	res := Result{
		HubUp: true, TokenOK: true,
		Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
	}
	if err := applyFailures(&res, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	if res.ExitCode() != 0 || res.Report.EndMarkerScanIncomplete {
		t.Fatalf("open-error result = %+v", res)
	}
	if report := res.Format(); !strings.Contains(report, "could not scan for damaged end markers: injected open failure") {
		t.Fatalf("open-error note missing:\n%s", report)
	}
}

func TestEndMarkerObservedCurrentDamageUsesScanVerdict(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	sessions := hook.NewSessions(token)
	if err := os.Mkdir(sessions.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(sessions.Dir(), "observed.ended")
	if err := os.WriteFile(marker, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEndedMarkerObservation(t, sessions, marker, integrate.Claude, time.Now().Add(-time.Hour))
	res := Result{
		HubUp: true, TokenOK: true,
		Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}},
	}
	if err := applyFailures(&res, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	if res.ExitCode() != 1 || res.Report.CurrentDamagedEndMarkers != 1 || res.Report.EndMarkerScanIncomplete {
		t.Fatalf("observed current-damage result = %+v", res)
	}
	if report := res.Format(); !strings.Contains(report, marker+" (still damaged)") || strings.Contains(report, "not rechecked") {
		t.Fatalf("observed current damage did not reuse scan verdict:\n%s", report)
	}
}

func TestEndMarkerDamageStatusRejectsNonRegularMarker(t *testing.T) {
	originalLstat, originalRead := endMarkerLstat, endMarkerReadGeneration
	t.Cleanup(func() { endMarkerLstat, endMarkerReadGeneration = originalLstat, originalRead })
	endMarkerLstat = func(string) (fs.FileInfo, error) {
		return fakeMarkerInfo{mode: fs.ModeNamedPipe}, nil
	}
	read := false
	endMarkerReadGeneration = func(string) (int, error) {
		read = true
		return 0, errors.New("must not be read")
	}
	if got := markerDamageStatus("x.ended"); got != "still damaged" {
		t.Fatalf("markerDamageStatus() = %q", got)
	}
	if read {
		t.Fatal("non-regular observation was opened")
	}
}

func TestEndMarkerDamageStatusReportsAbsentMarker(t *testing.T) {
	originalLstat, originalRead := endMarkerLstat, endMarkerReadGeneration
	t.Cleanup(func() { endMarkerLstat, endMarkerReadGeneration = originalLstat, originalRead })
	endMarkerLstat = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	read := false
	endMarkerReadGeneration = func(string) (int, error) {
		read = true
		return 0, errors.New("must not be read")
	}
	if got := markerDamageStatus("x.ended"); got != "archived since" {
		t.Fatalf("markerDamageStatus() = %q", got)
	}
	if read {
		t.Fatal("absent observation was opened")
	}
}
