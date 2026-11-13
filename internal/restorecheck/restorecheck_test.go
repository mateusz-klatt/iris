package restorecheck

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

// backup makes a hub database with one session still active, as a backup taken while the hub
// ran holds, and returns its path.
func backup(t *testing.T, active bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.db")
	s, err := store.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		if _, err := s.Call(context.Background(), "sessions", store.Request{Client: "codex", Host: "laptop", SessionID: "s1", Cwd: "/repo", Worktree: "/repo", Source: "hook"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// markBackup sets the restore marker in the backup at path, as `iris admin backup` does.
func markBackup(t *testing.T, path string) {
	t.Helper()
	db, _ := sql.Open("sqlite", path)
	defer func() { _ = db.Close() }()
	if err := store.MarkRestore(context.Background(), db, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A restored hub gives no session its backup-time authority, resets a monitor from after the
// backup, and says what the first sweep would remove. A marked backup's hub runs on a new
// stream; an unmarked one keeps its stream, which is a warning, not a failure (ADR 006). The
// backup itself is not changed.
func TestARestoredHubPassesEveryCheck(t *testing.T) {
	for _, tc := range []struct{ active, marked bool }{{true, true}, {false, true}, {true, false}, {false, false}} {
		name := map[bool]string{true: "with a live session", false: "with none"}[tc.active] + map[bool]string{true: ", marked", false: ", unmarked"}[tc.marked]
		active := tc.active
		t.Run(name, func(t *testing.T) {
			src := backup(t, active)
			if tc.marked {
				markBackup(t, src)
			}
			before, _ := os.ReadFile(src)
			var out bytes.Buffer
			if err := Run(context.Background(), src, filepath.Join(t.TempDir(), "scratch"), &out); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			text := out.String()
			for _, want := range []string{"recovering, none live", "answers status", "gets a reset", "first retention sweep"} {
				if !strings.Contains(text, want) {
					t.Fatalf("no %q in\n%s", want, text)
				}
			}
			if !regexp.MustCompile(`answers status \(database [0-9]+ bytes\)`).MatchString(text) {
				t.Fatalf("the size is not a whole number of bytes:\n%s", text)
			}
			if strings.Contains(text, "FAIL") {
				t.Fatal(text)
			}
			if active != strings.Contains(text, "gets no restored authority") {
				t.Fatal(text)
			}
			if tc.marked != strings.Contains(text, "runs on a new stream") || tc.marked == strings.Contains(text, "warn the backup has no restore marker") || tc.marked == strings.Contains(text, "Run `iris admin mark-restored` on the copy you restore") {
				t.Fatal(text)
			}
			if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
				t.Fatal("the backup was changed")
			}
		})
	}
}

// restore-check can be run on the scratch copy that an earlier restore-check left. That copy holds
// the earlier check's own session, recovering after the next start, so the monitor check needs a
// session new to the copy: with a fixed one, the hub refused the registration, and the check
// failed as though the monitor had not been reset.
func TestARestoreCheckChecksTheCopyAnEarlierCheckLeft(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	var out bytes.Buffer
	if err := Run(context.Background(), backup(t, false), first, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for range 2 {
		out.Reset()
		if err := Run(context.Background(), filepath.Join(first, "hub.db"), filepath.Join(t.TempDir(), "again"), &out); err != nil || !strings.Contains(out.String(), "ok   a monitor whose cursor is ahead of the restored history gets a reset") {
			t.Fatalf("%v\n%s", err, out.String())
		}
	}
}

// It refuses to work anywhere it could overwrite something, and on anything not a database.
func TestTheRestoreCheckRefusesWhatIsNotACleanCopy(t *testing.T) {
	src := backup(t, false)
	used := t.TempDir()
	if err := os.WriteFile(filepath.Join(used, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), src, used, &out); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a used scratch directory: %v", err)
	}
	if err := Run(context.Background(), filepath.Dir(src), t.TempDir(), &out); err == nil {
		t.Fatal("a directory was taken for a backup")
	}
	if err := Run(context.Background(), filepath.Join(t.TempDir(), "missing.db"), t.TempDir(), &out); err == nil {
		t.Fatal("a missing backup was accepted")
	}
	junk := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), junk, filepath.Join(t.TempDir(), "s"), &out); err == nil {
		t.Fatal("a file that is not a database passed")
	}
}

// A check that fails says FAIL and fails the run, so a bad restore cannot read as a good one.
func TestAFailedCheckFailsTheRun(t *testing.T) {
	real := dialWatch
	dialWatch = func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		return nil, nil, errors.New("no socket")
	}
	t.Cleanup(func() { dialWatch = real })
	var out bytes.Buffer
	err := Run(context.Background(), backup(t, true), filepath.Join(t.TempDir(), "s"), &out)
	if err == nil || !strings.Contains(out.String(), "FAIL a monitor") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

// A copy that opens as SQLite but not as a hub, and a hub that cannot listen, are errors.
func TestACopyThatIsNotAHubOrNoPortIsAnError(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), backup(t, false), filepath.Join(t.TempDir(), "s"), "256.0.0.1:0", &out); err == nil {
		t.Fatal("a hub with no port to listen on passed")
	}
	odd := filepath.Join(t.TempDir(), "odd.db")
	db, err := sql.Open("sqlite", odd)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE Execution(id TEXT, host_id TEXT, client TEXT, session_id TEXT, worktree TEXT, generation INTEGER, state TEXT, recovery_deadline INTEGER)", "INSERT INTO Execution VALUES('ex','h','codex','s','/w',1,'active',NULL)", "CREATE TABLE Repository(wrong TEXT)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	if err := Run(context.Background(), odd, filepath.Join(t.TempDir(), "s"), &out); err == nil || !strings.Contains(err.Error(), "open the copy") {
		t.Fatalf("a database that is not a hub: %v", err)
	}
}

type brokenTransport struct{}

func (brokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no route")
}

// A hub the check cannot reach fails every call it makes, and the run with it.
func TestAnUnreachableHubFailsItsChecks(t *testing.T) {
	real := httpClient
	httpClient = &http.Client{Transport: brokenTransport{}}
	t.Cleanup(func() { httpClient = real })
	var out bytes.Buffer
	err := Run(context.Background(), backup(t, true), filepath.Join(t.TempDir(), "s"), &out)
	if err == nil || !strings.Contains(out.String(), "FAIL the restored hub answers status") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// A session that cannot register is reported as that, not as a monitor that was not reset.
	if !strings.Contains(out.String(), "FAIL a new session registers on the restored hub") || strings.Contains(out.String(), "gets a reset") {
		t.Fatalf("a refused registration was blamed on the monitor:\n%s", out.String())
	}
}

// A scratch directory that cannot be made, and a backup that cannot be read, are errors.
func TestTheCopyNeedsAScratchDirectoryAndAReadableBackup(t *testing.T) {
	src := backup(t, false)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), src, filepath.Join(file, "scratch"), &out); err == nil {
		t.Fatal("a scratch directory under a file was accepted")
	}
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("root reads any file, and Windows file modes do not stop reading")
	}
	if err := os.Chmod(src, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(src, 0o600) })
	if err := Run(context.Background(), src, filepath.Join(t.TempDir(), "s"), &out); err == nil {
		t.Fatal("an unreadable backup was copied")
	}
}
