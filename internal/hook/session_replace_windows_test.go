//go:build windows

package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"golang.org/x/sys/windows"
)

type sessionEndReplaceHub struct {
	registration    hubclient.SessionRequest
	endedID         string
	endedGeneration int
}

func (h *sessionEndReplaceHub) RegisterSession(_ context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.registration = req
	return hubclient.SessionResponse{ExecutionID: "ex_replace", Generation: req.Generation}, nil
}

func (h *sessionEndReplaceHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{}, errors.New("unexpected touch during SessionEnd")
}

func (h *sessionEndReplaceHub) EndExecution(_ context.Context, id string, generation int) error {
	h.endedID, h.endedGeneration = id, generation
	return nil
}

func TestSessionEndReplacesMarkerWithReaderOpen(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 2)
	marker := m.name("claude", "s") + ".ended"
	if err := m.replace(marker, "1"); err != nil {
		t.Fatal(err)
	}
	held, err := OpenGenerationFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	root := filepath.Dir(m.dir)
	token := filepath.Join(root, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	hub := &sessionEndReplaceHub{}
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := (Runner{
		ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
		Host: func() string { return "host" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: hub, Memory: m, after: never, logAfter: never,
	}).Run()
	assertAllow(t, ClientClaude, out)
	if hub.registration.Generation != 2 || hub.endedID != "ex_replace" || hub.endedGeneration != 2 {
		t.Fatalf("SessionEnd registration = %+v, ended = %q G%d", hub.registration, hub.endedID, hub.endedGeneration)
	}
	if generation, err := readGeneration(marker); generation != 2 || err != nil {
		t.Fatalf("replacement with reader open = G%d, %v; want G2", generation, err)
	}
	old, err := io.ReadAll(held)
	if err != nil || string(old) != "1\n" {
		t.Fatalf("open reader lost the old marker: %q, %v", old, err)
	}
	observed, err := os.ReadFile(filepath.Join(root, "hook-observed.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(observed), DamagedGenerationProof) {
		t.Fatalf("SessionEnd recorded damaged proof after a successful replacement: %s", observed)
	}
}

func TestSessionEndWithOutsideReaderFailsAndRecordsMarker(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 2)
	proof := m.name("claude", "s")
	marker := proof + ".ended"
	if err := m.replace(marker, "1"); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(marker)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	root := filepath.Dir(m.dir)
	token := filepath.Join(root, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	hub := &sessionEndReplaceHub{}
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := (Runner{
		ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
		Host: func() string { return "host" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: hub, Memory: m, after: never, logAfter: never,
	}).Run()
	assertAllow(t, ClientClaude, out)
	if generation, err := readGeneration(marker); generation != 1 || err != nil {
		t.Fatalf("outside reader allowed marker replacement: G%d, %v", generation, err)
	}
	if generation, err := readGeneration(proof); generation != 2 || err != nil {
		t.Fatalf("failed marker replacement did not preserve proof: G%d, %v", generation, err)
	}
	observed, err := os.ReadFile(filepath.Join(root, "hook-observed.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(observed), DamagedGenerationProof) || !strings.Contains(string(observed), filepath.Base(marker)) {
		t.Fatalf("outside-reader failure was not recorded: %s", observed)
	}
}

func TestReplaceSessionFileDeadlineWindowsCheckpoints(t *testing.T) {
	t.Run("default deadline and nil now/pause", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		dst := filepath.Join(dir, "dst")
		if err := os.WriteFile(src, []byte("next\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceSessionFileDeadline(src, dst, time.Time{}, false, nil, nil); err != nil {
			t.Fatalf("replaceSessionFileDeadline default = %v", err)
		}
		if raw, err := os.ReadFile(dst); err != nil || string(raw) != "next\n" {
			t.Fatalf("dst = %q, %v", raw, err)
		}
	})

	t.Run("expired before CreateFile", func(t *testing.T) {
		origCreate := winCreateFile
		t.Cleanup(func() { winCreateFile = origCreate })
		winCreateFile = func(*uint16, uint32, uint32, *windows.SecurityAttributes, uint32, uint32, windows.Handle) (windows.Handle, error) {
			t.Fatal("CreateFile ran past deadline")
			return 0, nil
		}
		clock := time.Unix(1_000, 0)
		err := renameSessionFilePOSIXDeadline("old", "new", clock, func() time.Time { return clock })
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("pre-CreateFile expiry = %v, want ErrDeadlineExceeded", err)
		}

		// Path resolution consuming remaining budget stops cleanly before CreateFile.
		pathClock := time.Unix(1_000, 0)
		deadline := pathClock.Add(hookLockBudget)
		calls := 0
		err = renameSessionFilePOSIXDeadline("old", "new", deadline, func() time.Time {
			calls++
			if calls == 2 {
				pathClock = deadline
			}
			return pathClock
		})
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("post-path-resolution expiry = %v, want ErrDeadlineExceeded", err)
		}
	})

	t.Run("expired between CreateFile and SetFileInformationByHandle", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		if err := os.WriteFile(src, []byte("v\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		origCreate, origSet := winCreateFile, winSetFileInformationHandle
		t.Cleanup(func() {
			winCreateFile = origCreate
			winSetFileInformationHandle = origSet
		})
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		var opened windows.Handle
		winCreateFile = func(name *uint16, access, share uint32, sa *windows.SecurityAttributes, disp, flags uint32, tmpl windows.Handle) (windows.Handle, error) {
			clock = deadline
			h, err := origCreate(name, access, share, sa, disp, flags, tmpl)
			opened = h
			return h, err
		}
		winSetFileInformationHandle = func(windows.Handle, uint32, *byte, uint32) error {
			t.Fatal("SetFileInformationByHandle ran past deadline")
			return nil
		}
		err := replaceSessionFileDeadline(src, filepath.Join(dir, "dst"), deadline, false, func() time.Time { return clock }, func(time.Duration) {
			t.Fatal("unexpected retry sleep")
		})
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("between CreateFile and SetFileInfo = %v, want ErrDeadlineExceeded", err)
		}
		if opened == 0 {
			t.Fatal("CreateFile did not run")
		}
		// Closing an already-closed handle returns ERROR_INVALID_HANDLE.
		if closeErr := windows.CloseHandle(opened); closeErr == nil {
			t.Fatal("handle remained open after pre-SetFileInformationByHandle expiry")
		}
	})

	t.Run("optional write does zero sharing sleep while required caps sleep", func(t *testing.T) {
		clock := time.Unix(1_000, 0)
		deadline := clock.Add(10 * time.Millisecond)
		calls := 0
		err := replaceSessionFileWithOpsDeadline("old", "new", deadline, false, func() time.Time { return clock }, func(d time.Duration) {
			t.Fatalf("optional write slept %v on sharing violation", d)
		}, func(string, string, time.Time, func() time.Time) error {
			calls++
			return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
		}, os.Rename)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || calls != 1 {
			t.Fatalf("optional sharing violation = %v after %d calls", err, calls)
		}

		// Required write caps pause to min(sessionFileRenamePause, rem), then stops when rem <= 0.
		clock = time.Unix(1_000, 0)
		deadline = clock.Add(time.Millisecond + 400*time.Microsecond)
		var pauses []time.Duration
		calls = 0
		err = replaceSessionFileWithOpsDeadline("old", "new", deadline, true, func() time.Time { return clock }, func(d time.Duration) {
			pauses = append(pauses, d)
			clock = clock.Add(d)
		}, func(string, string, time.Time, func() time.Time) error {
			calls++
			return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
		}, os.Rename)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || calls != 2 {
			t.Fatalf("required sharing violation = %v after %d calls", err, calls)
		}
		if len(pauses) != 2 || pauses[0] != sessionFileRenamePause || pauses[1] != 400*time.Microsecond {
			t.Fatalf("required pauses = %v, want [%v, 400µs]", pauses, sessionFileRenamePause)
		}

		// Required write with pause == nil defaults to time.Sleep on retry.
		retryCalls := 0
		if err := replaceSessionFileWithOpsDeadline("old", "new", time.Now().Add(hookLockBudget), true, time.Now, nil, func(string, string, time.Time, func() time.Time) error {
			retryCalls++
			if retryCalls == 1 {
				return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
			}
			return nil
		}, os.Rename); err != nil || retryCalls != 2 {
			t.Fatalf("default pause retry = %v after %d calls", err, retryCalls)
		}

		// Required write hitting rem <= 0 immediately after rename returns without sleeping.
		clock = time.Unix(1_000, 0)
		deadline = clock.Add(5 * time.Millisecond)
		err = replaceSessionFileWithOpsDeadline("old", "new", deadline, true, func() time.Time { return clock }, func(d time.Duration) {
			t.Fatalf("unexpected sleep %v when rem <= 0", d)
		}, func(string, string, time.Time, func() time.Time) error {
			clock = deadline
			return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
		}, os.Rename)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("zero-rem required = %v, want ERROR_SHARING_VIOLATION", err)
		}
	})

	t.Run("two-session fake-clock contention and native held destination", func(t *testing.T) {
		clock := time.Unix(1_800_000_000, 0)
		dir := filepath.Join(t.TempDir(), "sessions")
		mA := fileMemory{
			dir:      dir,
			now:      func() time.Time { return clock },
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
			pause:    func(d time.Duration) { clock = clock.Add(d) },
		}
		mB := fileMemory{
			dir:      dir,
			now:      func() time.Time { return clock },
			mono:     func() time.Time { return clock },
			restrict: config.OwnerOnly,
			pause:    func(d time.Duration) { clock = clock.Add(d) },
		}
		if err := mA.directory(); err != nil {
			t.Fatal(err)
		}

		// Hold <hash>.execution open without FILE_SHARE_DELETE on native Windows NTFS.
		execDst := mA.name("claude", "sessA") + ".execution"
		if err := os.WriteFile(execDst, []byte("old-exec\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		execPtr, err := windows.UTF16PtrFromString(execDst)
		if err != nil {
			t.Fatal(err)
		}
		heldHandle, err := windows.CreateFile(execPtr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		held := os.NewFile(uintptr(heldHandle), execDst)
		defer func() { _ = held.Close() }()

		// Session A spends 45ms waiting/preparing before first replacement (succeeds) and
		// second optional replacement against held destination fails with no sharing sleep.
		deadlineA := clock.Add(hookLockBudget)
		tmp1, err := prepareSessionFile(dir, "1", mA.restrict)
		if err != nil {
			t.Fatal(err)
		}
		tmp2, err := prepareSessionFile(dir, "2", mA.restrict)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Remove(tmp1)
			_ = os.Remove(tmp2)
		}()
		unlockA, err := mA.lockDeadline(deadlineA)
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(45 * time.Millisecond)
		if err := replaceSessionFileDeadline(tmp1, mA.name("claude", "sessA"), deadlineA, false, mA.monoClock(), mA.pause); err != nil {
			unlockA()
			t.Fatalf("first optional replacement failed: %v", err)
		}
		// Optional replacement against held <hash>.execution fails immediately with no new 50ms budget.
		if err := replaceSessionFileDeadline(tmp2, execDst, deadlineA, false, mA.monoClock(), mA.pause); err == nil {
			unlockA()
			t.Fatal("held destination without FILE_SHARE_DELETE unexpectedly allowed replacement")
		}
		unlockA()
		if raw, err := os.ReadFile(execDst); err != nil || string(raw) != "old-exec\n" {
			t.Fatalf("held <hash>.execution changed to %q, %v", raw, err)
		}

		// Session B acquires the released lock and writes its own session cleanly.
		mB.Remember("claude", "sessB", 5)
		if got := mB.Recall("claude", "sessB"); got != 5 {
			t.Fatalf("session B recall = G%d, want G5", got)
		}
	})
}
