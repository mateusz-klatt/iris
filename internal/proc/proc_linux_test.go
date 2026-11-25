//go:build linux

package proc

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	s := "1055815 (python3) R 1055804 1055804 1055804 0 -1 4194304 974 0 0 0 1 0 0 0 20 0 1 0 2375890 24555520 2498 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	state, ppid, ticks, err := parseStat(s)
	if err != nil || state != "R" || ppid != 1055804 || ticks != 2375890 {
		t.Fatalf("state=%s ppid=%d ticks=%d err=%v", state, ppid, ticks, err)
	}
	// comm itself contains a parenthesis and spaces.
	s = "12 (a) b) S 99 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 50 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	state, ppid, ticks, err = parseStat(s)
	if err != nil || state != "S" || ppid != 99 || ticks != 50 {
		t.Fatalf("comm with paren: state=%s ppid=%d ticks=%d err=%v", state, ppid, ticks, err)
	}
}

func TestParseStatRefusesDamage(t *testing.T) {
	for _, s := range []string{
		"no parenthesis",
		"1 (name)",
		"1 (name) S 2 3",
		"1 (name) S x 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 50 0",
		"1 (name) S 2 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 x 0",
	} {
		if _, _, _, err := parseStat(s); err == nil {
			t.Errorf("%q", s)
		}
	}
}

func TestStartFrom(t *testing.T) {
	stat := "12 (a) S 99 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 250 0"
	ok := func() (int64, int64, error) { return 1000, 100, nil }
	if got, err := startFrom(stat, ok); got != 1002500 || err != nil {
		t.Fatalf("%d %v", got, err)
	}
	if _, err := startFrom("damaged", ok); err == nil {
		t.Fatal("damaged stat")
	}
	broken := errors.New("no clock")
	if _, err := startFrom(stat, func() (int64, int64, error) { return 0, 0, broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

func TestBootTime(t *testing.T) {
	read := func(text string, err error) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path != "/proc/stat" {
				t.Fatal(path)
			}
			return []byte(text), err
		}
	}
	if got, err := bootTime(read("cpu 1 2\nbtime 1700000000\nprocesses 3\n", nil)); got != 1700000000 || err != nil {
		t.Fatalf("%d %v", got, err)
	}
	if _, err := bootTime(read("cpu 1 2\n", nil)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := bootTime(read("btime soon\n", nil)); err == nil {
		t.Fatal("damaged line")
	}
	broken := errors.New("unreadable")
	if _, err := bootTime(read("", broken)); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

func auxv(words ...uint64) []byte {
	var out []byte
	for _, w := range words {
		if strconv.IntSize == 64 {
			out = binary.NativeEndian.AppendUint64(out, w)
		} else {
			out = binary.NativeEndian.AppendUint32(out, uint32(w))
		}
	}
	return out
}

func TestClkTck(t *testing.T) {
	read := func(raw []byte, err error) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return raw, err }
	}
	for name, c := range map[string]struct {
		raw  []byte
		err  error
		want int64
	}{
		"present":    {auxv(6, 4096, 17, 250, 0, 0), nil, 250},
		"zero value": {auxv(17, 0, 0, 0), nil, 100},
		"terminated": {auxv(6, 4096, 0, 0, 17, 250), nil, 100},
		"truncated":  {auxv(6, 4096), nil, 100},
		"unreadable": {nil, errors.New("denied"), 100},
	} {
		if got := clkTck(read(c.raw, c.err)); got != c.want {
			t.Errorf("%s: %d", name, got)
		}
	}
}

func TestAuxWordReadsBothWidths(t *testing.T) {
	if got := auxWord(binary.NativeEndian.AppendUint64(nil, 1<<40)); got != 1<<40 {
		t.Fatal(got)
	}
	if got := auxWord(binary.NativeEndian.AppendUint32(nil, 7)); got != 7 {
		t.Fatal(got)
	}
}

func TestStatusValue(t *testing.T) {
	status := []byte("Name:\tiris\nPPid:\t12\n")
	if got := statusValue(status, "PPid:"); got != "12" {
		t.Fatal(got)
	}
	if got := statusValue(status, "Missing:"); got != "" {
		t.Fatal(got)
	}
}

func TestLookupReadsThisProcess(t *testing.T) {
	got, ok := lookupOS(os.Getpid())
	link, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.PPID != os.Getppid() || got.StartMS <= 0 || !strings.Contains(got.Name, "proc.test") {
		t.Fatalf("%+v %v", got, ok)
	}
	if got.Exe != strings.TrimSuffix(link, " (deleted)") || got.Launcher != "" {
		t.Fatalf("exe %q launcher %q link %q", got.Exe, got.Launcher, link)
	}
}

func TestLookupReadsCursorLauncherEvidenceAndLeavesAnUnknownAgent(t *testing.T) {
	nodeBin := filepath.Join(t.TempDir(), "node")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeBin, self, 0o700); err != nil {
		t.Fatal(err)
	}
	index := "/home/user/.local/share/cursor-agent/versions/2026.11.22-e9042b2/index.js"
	cursor := exec.Command(nodeBin, "-test.run=^TestProcDeletedExeChild$", "--", "--use-system-ca", index)
	cursor.Args[0] = "/Users/user/.local/bin/agent"
	cursor.Env = append(os.Environ(), "IRIS_PROC_DELETED_MODE=sleep")
	if err := cursor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cursor.Process.Kill()
		_ = cursor.Wait()
	})
	otherWithIndex := exec.Command(os.Args[0], "-test.run=^TestProcDeletedExeChild$", "--", "--use-system-ca", index)
	otherWithIndex.Args[0] = "/Users/user/.local/bin/agent"
	otherWithIndex.Env = append(os.Environ(), "IRIS_PROC_DELETED_MODE=sleep")
	if err := otherWithIndex.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = otherWithIndex.Process.Kill()
		_ = otherWithIndex.Wait()
	})
	unknown := exec.Command(os.Args[0], "-test.run=^TestProcDeletedExeChild$")
	unknown.Args[0] = "agent"
	unknown.Env = append(os.Environ(), "IRIS_PROC_DELETED_MODE=sleep")
	if err := unknown.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unknown.Process.Kill()
		_ = unknown.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	var foundCursor, foundOther, foundUnknown Snapshot
	for {
		if s, ok := lookupOS(cursor.Process.Pid); ok && s.Launcher == index {
			foundCursor = s
		}
		if s, ok := lookupOS(otherWithIndex.Process.Pid); ok && s.Launcher == index {
			foundOther = s
		}
		if s, ok := lookupOS(unknown.Process.Pid); ok && s.Name == "agent" {
			foundUnknown = s
		}
		if foundCursor.PID != 0 && foundOther.PID != 0 && foundUnknown.PID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor %+v other %+v unknown %+v", foundCursor, foundOther, foundUnknown)
		}
		time.Sleep(time.Millisecond)
	}
	wantExe, err := filepath.EvalSymlinks(nodeBin)
	if err != nil {
		t.Fatal(err)
	}
	gotExe, err := filepath.EvalSymlinks(foundCursor.Exe)
	if err != nil {
		t.Fatal(err)
	}
	if foundCursor.Name != "cursor-agent" || gotExe != wantExe {
		t.Fatalf("cursor launcher %+v (want exe %q)", foundCursor, wantExe)
	}
	if client, ok := ClientNameOf(foundCursor, nil); !ok || client != "cursor" {
		t.Fatalf("cursor client %q %v", client, ok)
	}
	if foundOther.Name != "/Users/user/.local/bin/agent" || foundOther.Exe == "" {
		t.Fatalf("non-node binary with index.js changed name: %+v", foundOther)
	}
	if client, ok := ClientNameOf(foundOther, nil); ok {
		t.Fatalf("non-node binary with index.js became %s", client)
	}
	if foundUnknown.Name != "agent" || foundUnknown.Launcher != "" {
		t.Fatalf("unknown agent %+v", foundUnknown)
	}
	if client, ok := ClientNameOf(foundUnknown, nil); ok {
		t.Fatalf("unknown agent became %s", client)
	}
}

func TestLookupOfAMissingProcessHasNoStartTime(t *testing.T) {
	if _, _, err := statEvidence(0x7FFFFFF0); err == nil {
		t.Fatal("no such process")
	}
}

func TestLookupRejectsAZombie(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, _, err := statEvidence(cmd.Process.Pid)
		if err == nil && state == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not become a zombie: state %q, error %v", state, err)
		}
		time.Sleep(time.Millisecond)
	}
	if got, ok := Lookup(cmd.Process.Pid); ok {
		t.Fatalf("zombie was accepted as live: %+v", got)
	}
}

func TestProcDeletedExeChild(t *testing.T) {
	switch os.Getenv("IRIS_PROC_DELETED_MODE") {
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "record":
		out := os.Getenv("IRIS_PROC_RECORD_OUT")
		tag := os.Getenv("IRIS_PROC_RECORD_TAG")
		body := tag + "\n" + strings.Join(os.Args[1:], "\n") + "\n"
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func copyTestExecutable(t *testing.T, dst string) {
	t.Helper()
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenExecutableSurvivesDeletedBinaryAndNeverRunsDecoy(t *testing.T) {
	dir := t.TempDir()
	fakeAgy := filepath.Join(dir, "agy")
	copyTestExecutable(t, fakeAgy)

	child := exec.Command(fakeAgy, "-test.run=^TestProcDeletedExeChild$")
	child.Env = append(os.Environ(), "IRIS_PROC_DELETED_MODE=sleep")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childWaited := false
	stopChild := func() {
		if childWaited {
			return
		}
		childWaited = true
		_ = child.Process.Kill()
		_ = child.Wait()
	}
	t.Cleanup(stopChild)

	snap, ok := Lookup(child.Process.Pid)
	if !ok || snap.StartMS <= 0 {
		t.Fatalf("child lookup failed: %+v %v", snap, ok)
	}

	// Simulate an in-place update: rename the running binary, unlink it, and place a decoy
	// executable at the original path.
	oldPath := fakeAgy + ".1795283135508816615.old"
	if err := os.Rename(fakeAgy, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	decoyMarker := filepath.Join(dir, "decoy-ran")
	decoy := "#!/bin/sh\necho DECOY > " + strconv.Quote(decoyMarker) + "\nexit 42\n"
	if err := os.WriteFile(fakeAgy, []byte(decoy), 0o755); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink("/proc/" + strconv.Itoa(snap.PID) + "/exe")
	if err != nil || !strings.HasSuffix(target, " (deleted)") {
		t.Fatalf("expected deleted /proc/<pid>/exe, got %q (%v)", target, err)
	}

	// Path-only Executable still rejects a deleted image.
	if _, err := Executable(snap.PID, snap.StartMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Executable(deleted) = %v, want ErrNotFound", err)
	}

	img, err := OpenExecutable(snap.PID, snap.StartMS)
	if err != nil || !img.Deleted || img.Path != target || img.file == nil {
		t.Fatalf("OpenExecutable(deleted) = %+v, %v", img, err)
	}
	rawFD := img.file

	recordOut := filepath.Join(dir, "recorded.txt")
	cmd := img.Command(context.Background(), "-test.run=^TestProcDeletedExeChild$")
	if cmd.Path != "/proc/self/fd/3" || len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != rawFD {
		_ = img.Close()
		t.Fatalf("deleted Command = path %q extraFiles %v", cmd.Path, cmd.ExtraFiles)
	}
	cmd.Env = append(os.Environ(),
		"IRIS_PROC_DELETED_MODE=record",
		"IRIS_PROC_RECORD_TAG=old-image-v1",
		"IRIS_PROC_RECORD_OUT="+recordOut,
	)
	if err := cmd.Start(); err != nil {
		_ = img.Close()
		t.Fatalf("Start(/proc/self/fd/3) failed: %v", err)
	}
	if err := img.Close(); err != nil {
		t.Fatalf("Close after Start = %v", err)
	}
	if _, err := rawFD.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("parent descriptor was not closed: %v", err)
	}
	if err := img.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait(/proc/self/fd/3) failed: %v", err)
	}

	recorded, err := os.ReadFile(recordOut)
	if err != nil {
		t.Fatalf("old image did not write recorded args: %v", err)
	}
	if !strings.Contains(string(recorded), "old-image-v1\n-test.run=^TestProcDeletedExeChild$\n") {
		t.Fatalf("recorded output = %q", string(recorded))
	}
	if _, err := os.Stat(decoyMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("decoy binary ran instead of old image: stat err = %v", err)
	}

	// Once the child exits, OpenExecutable returns ErrNotFound.
	stopChild()
	if _, err := OpenExecutable(snap.PID, snap.StartMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenExecutable(exited) = %v, want ErrNotFound", err)
	}
}

func TestOpenDeletedExecutableHandlesIdentityChangeExitOpenAndFstatErrors(t *testing.T) {
	live := func(int) (Snapshot, bool) { return Snapshot{PID: 42, StartMS: 1000}, true }
	exited := func(int) (Snapshot, bool) { return Snapshot{}, false }
	reused := func(int) (Snapshot, bool) { return Snapshot{PID: 42, StartMS: 2000}, true }

	// 1. Open failure while live -> ErrExecutableDeleted; when exited or reused -> ErrNotFound.
	openDenied := func(string) (*os.File, error) { return nil, os.ErrPermission }
	if _, err := openDeletedExeFrom(42, 1000, "/bin/agy (deleted)", live, openDenied); !errors.Is(err, ErrExecutableDeleted) {
		t.Fatalf("open denied while live: %v", err)
	}
	if _, err := openDeletedExeFrom(42, 1000, "/bin/agy (deleted)", exited, openDenied); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open denied after exit: %v", err)
	}
	if _, err := openDeletedExeFrom(42, 1000, "/bin/agy (deleted)", reused, openDenied); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open denied after PID reuse: %v", err)
	}

	// 2. Non-regular file (directory) -> closes fd and returns ErrExecutableDeleted while live.
	dir := t.TempDir()
	dirFile, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openDeletedExeFrom(42, 1000, "/bin/agy (deleted)", live, func(string) (*os.File, error) {
		return dirFile, nil
	}); !errors.Is(err, ErrExecutableDeleted) {
		t.Fatalf("non-regular deleted exe: %v", err)
	}
	if _, err := dirFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("non-regular fd was not closed: %v", err)
	}

	// 3. fstat error (already-closed file) after process exit -> ErrNotFound.
	regPath := filepath.Join(dir, "reg")
	if err := os.WriteFile(regPath, []byte("bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	closedFile, err := os.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = closedFile.Close()
	if _, err := openDeletedExeFrom(42, 1000, "/bin/agy (deleted)", exited, func(string) (*os.File, error) {
		return closedFile, nil
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fstat error after exit: %v", err)
	}

	// 4. Identity change or process exit after open+fstat closes the fd and returns ErrNotFound.
	for name, getAfter := range map[string]func(int) (Snapshot, bool){
		"reused": reused,
		"exited": exited,
	} {
		f, err := os.Open(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := openDeletedExeFrom(42, 1000, "/bin/agy (deleted)", getAfter, func(string) (*os.File, error) {
			return f, nil
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s after fstat: %v", name, err)
		}
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("%s fd was not closed: %v", name, err)
		}
	}
}
