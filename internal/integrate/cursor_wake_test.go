package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorWakeMarkerIsExplicitAndSurvivesReinstall(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	enabled, err := CursorWake(l)
	if err != nil || !enabled {
		t.Fatalf("missing marker: %v %v", enabled, err)
	}
	if err := InstallCursorWake(l, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err = CursorWake(l); err != nil || enabled {
		t.Fatalf("off: %v %v", enabled, err)
	}
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if enabled, err = CursorWake(l); err != nil || enabled {
		t.Fatalf("reinstall cleared off: %v %v", enabled, err)
	}
	if err := InstallCursorWake(l, true); err != nil {
		t.Fatal(err)
	}
	dir, err := l.underHome(".cursor")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(dir, cursorWakeFile))); got != "on\n" {
		t.Fatalf("marker %q", got)
	}
	files, _ := c.Files(l)
	if !strings.Contains(string(mustRead(t, files[0])), "--event stop") {
		t.Fatal("stop hook was not installed")
	}
}

func TestCursorWakeReportsAnUnreadableHomeAndMarker(t *testing.T) {
	if err := InstallCursorWake(homeless(t), false); err == nil {
		t.Fatal("wake install without a home succeeded")
	}
	if _, err := CursorWake(homeless(t)); err == nil {
		t.Fatal("no home was a successful read")
	}
	l := testLayout(t)
	dir, err := l.underHome(".cursor")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "hooks.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallCursorWake(l, true); err == nil {
		t.Fatal("a directory in place of hooks.json was installed")
	}
	if _, err := os.Stat(filepath.Join(dir, cursorWakeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a broken hooks file still wrote the marker: %v", err)
	}
	marker := testLayout(t)
	dir, err = marker.underHome(".cursor")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, cursorWakeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CursorWake(marker); err == nil {
		t.Fatal("a directory was read as the marker")
	}
}
