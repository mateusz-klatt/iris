//go:build linux

package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func copyWakeTestBinary(t *testing.T, dst string) {
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

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestAntigravityWakeSurvivesInPlaceUpdateViaDeletedDescriptor(t *testing.T) {
	const convID = "e28a6164-6409-4810-b8ee-df8202a16a0a"

	binDir := t.TempDir()
	fakeAgy := filepath.Join(binDir, "agy")
	copyWakeTestBinary(t, fakeAgy)

	child := exec.Command(fakeAgy, "fake-agy-sleep")
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

	agySnap, ok := proc.Lookup(child.Process.Pid)
	if !ok || agySnap.StartMS <= 0 {
		t.Fatalf("fake agy lookup failed: %+v %v", agySnap, ok)
	}

	// Simulate an in-place agy update: rename the running binary, unlink it, and place a decoy
	// executable at the original path.
	oldPath := fakeAgy + ".1795283135508816615.old"
	if err := os.Rename(fakeAgy, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	decoyMarker := filepath.Join(binDir, "decoy-ran")
	decoy := "#!/bin/sh\necho DECOY > " + strconv.Quote(decoyMarker) + "\nexit 42\n"
	if err := os.WriteFile(fakeAgy, []byte(decoy), 0o755); err != nil {
		t.Fatal(err)
	}

	// Check exact descriptor cleanup across repeated check() calls before starting any
	// httptest servers that open background sockets.
	checkOnly := &antigravityCommands{
		owner: hook.AntigravityHelperOwner{
			AgyPID:     agySnap.PID,
			AgyStartMS: agySnap.StartMS,
			SessionID:  convID,
		},
		executable: proc.OpenExecutable,
		now:        func() time.Time { return time.UnixMilli(2000) },
	}
	hubclient.HTTPClient().CloseIdleConnections()
	beforeFDs := openFDCount(t)
	for range 15 {
		info, det, differs := checkOnly.check(context.Background(), "")
		if det != "" || differs || !strings.HasSuffix(info.Path, " (deleted)") {
			t.Fatalf("check(deleted) = info=%+v det=%q differs=%v", info, det, differs)
		}
	}
	if afterFDs := openFDCount(t); afterFDs != beforeFDs {
		t.Fatalf("check() leaked file descriptors: before=%d after=%d", beforeFDs, afterFDs)
	}

	h := newAgyTestHarness(t, convID)

	// Mark the conversation idle so antigravityCommands.queue admits the wake.
	owner := hook.AntigravityHelperOwner{
		AgyPID:        h.agyPID,
		AgyStartMS:    h.agyStartMS,
		HelperPID:     h.agyPID,
		HelperStartMS: h.agyStartMS,
		SessionID:     convID,
		AttemptID:     "00112233445566778899aabbccddeeff",
	}
	if _, err := hook.PublishAntigravityHelper(h.configDir, owner, nil); err != nil {
		t.Fatal(err)
	}
	h.emitHook(convID, "Stop", map[string]any{
		"conversationId":     convID,
		"executionNum":       0,
		"continuationActive": false,
		"fullyIdle":          true,
		"terminationReason":  "NO_TOOL_CALL",
	})

	cmds := &antigravityCommands{
		configDir: h.configDir,
		owner: hook.AntigravityHelperOwner{
			AgyPID:     agySnap.PID,
			AgyStartMS: agySnap.StartMS,
			SessionID:  convID,
		},
		agentEnv: []string{
			"ANTIGRAVITY_CONVERSATION_ID=" + convID,
			"ANTIGRAVITY_LS_ADDRESS=" + h.lsAddr,
			"ANTIGRAVITY_CSRF_TOKEN=deleted-fd-secret",
			"GOCOVERDIR=" + t.TempDir(),
		},
		executable:   proc.OpenExecutable,
		now:          func() time.Time { return time.UnixMilli(2000) },
		queueTimeout: 5 * time.Second,
		waitDelay:    time.Second,
	}

	// queue() must execute the old deleted image via /proc/self/fd/3 and never invoke the decoy.
	const wakeMsg = "Iris wake after in-place update"
	ans := cmds.queue(context.Background(), convID, wakeMsg, func(start func() error) (bool, error) {
		err := start()
		return err == nil, err
	})
	if ans.answer != "submitted" || ans.detail != "" || !ans.started {
		t.Fatalf("queue(deleted) = %+v, want submitted", ans)
	}
	h.mu.Lock()
	calls := append([]fakeLSRequest(nil), h.lsCalls...)
	h.mu.Unlock()
	wantArgs := []string{"agentapi", "send-message", convID, wakeMsg}
	if len(calls) != 1 || !slices.Equal(calls[0].Args, wantArgs) {
		t.Fatalf("LS calls = %+v, want single call with args %v", calls, wantArgs)
	}
	if _, err := os.Stat(decoyMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("decoy at original path ran instead of old image: stat err = %v", err)
	}

	// Once fake agy exits, check() reports unavailable (process gone, not deleted-image error).
	stopChild()
	if _, det, _ := cmds.check(context.Background(), ""); det != antigravityExeUnavailable {
		t.Fatalf("check(exited) = %q, want %q", det, antigravityExeUnavailable)
	}
}

// Real descriptor-backed Start failure in its own server-free fixture so process-wide exact
// file descriptor counts cannot be perturbed by asynchronous HTTP test-server socket closes.
func TestAntigravityWakeDeletedDescriptorStartFailureClosesFD(t *testing.T) {
	const convID = "e28a6164-6409-4810-b8ee-df8202a16a0a"

	dir := t.TempDir()
	configDir := filepath.Join(dir, "iris-cfg")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook.AntigravityWakePath(configDir), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	noExecAgy := filepath.Join(dir, "agy-noexec")
	copyWakeTestBinary(t, noExecAgy)
	noExecChild := exec.Command(noExecAgy, "fake-agy-sleep")
	if err := noExecChild.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = noExecChild.Process.Kill()
		_ = noExecChild.Wait()
	})
	noExecSnap, ok := proc.Lookup(noExecChild.Process.Pid)
	if !ok || noExecSnap.StartMS <= 0 {
		t.Fatalf("noexec child lookup failed: %+v %v", noExecSnap, ok)
	}
	// Strip execute bits from the running binary before unlinking it so proc.OpenExecutable
	// succeeds with a real open descriptor while execve(/proc/self/fd/3) fails with EACCES.
	if err := os.Chmod(noExecAgy, 0o600); err != nil {
		t.Fatal(err)
	}
	noExecOld := noExecAgy + ".old"
	if err := os.Rename(noExecAgy, noExecOld); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(noExecOld); err != nil {
		t.Fatal(err)
	}

	// Record convID as idle via an Antigravity Stop hook without starting any HTTP server.
	stopPayload, _ := json.Marshal(map[string]any{
		"conversationId":     convID,
		"executionNum":       1,
		"continuationActive": false,
		"fullyIdle":          true,
		"terminationReason":  "NO_TOOL_CALL",
	})
	tokenPath := filepath.Join(configDir, "hub.token")
	(hook.Runner{
		ClientName:      "antigravity",
		EventName:       "Stop",
		Stdin:           bytes.NewReader(stopPayload),
		PinnedConfigDir: func() (string, error) { return configDir, nil },
		Find: func(string) (gitinfo.Repo, error) {
			return gitinfo.Repo{}, gitinfo.ErrNotRepo
		},
		Getenv: func(k string) string {
			switch k {
			case "IRIS_TOKEN_FILE":
				return tokenPath
			case "ANTIGRAVITY_CONVERSATION_ID":
				return convID
			default:
				return ""
			}
		},
		Ancestor: func() (proc.Ancestor, error) {
			return proc.Ancestor{PID: noExecSnap.PID, StartMS: noExecSnap.StartMS, Name: "agy"}, nil
		},
	}).Run()
	if conv, ok := hook.ReadAntigravityConvState(configDir, convID); !ok || !conv.Idle {
		t.Fatalf("antigravity Stop hook did not mark conv idle: conv=%+v ok=%v", conv, ok)
	}

	cmds := &antigravityCommands{
		configDir: configDir,
		owner: hook.AntigravityHelperOwner{
			AgyPID:     noExecSnap.PID,
			AgyStartMS: noExecSnap.StartMS,
			SessionID:  convID,
		},
		agentEnv:     []string{"ANTIGRAVITY_CONVERSATION_ID=" + convID, "GOCOVERDIR=" + t.TempDir()},
		executable:   proc.OpenExecutable,
		now:          func() time.Time { return time.UnixMilli(2000) },
		queueTimeout: 5 * time.Second,
		waitDelay:    time.Second,
	}

	var startErr error
	hubclient.HTTPClient().CloseIdleConnections()
	beforeFDs := openFDCount(t)
	failAns := cmds.queue(context.Background(), convID, "Iris wake after in-place update", func(start func() error) (bool, error) {
		startErr = start()
		return startErr == nil, startErr
	})
	var pathErr *os.PathError
	if !errors.Is(startErr, syscall.EACCES) || !errors.As(startErr, &pathErr) || pathErr.Path != "/proc/self/fd/3" {
		t.Fatalf("real deleted-fd Start error = %v, want PathError(/proc/self/fd/3) with EACCES", startErr)
	}
	if failAns.answer != "failed" || failAns.detail != antigravityExeDeleted || failAns.started {
		t.Fatalf("real deleted-fd Start failure = %+v, want failed with %q", failAns, antigravityExeDeleted)
	}
	if afterFDs := openFDCount(t); afterFDs != beforeFDs {
		t.Fatalf("failed Start on deleted descriptor leaked fds: before=%d after=%d", beforeFDs, afterFDs)
	}
}
