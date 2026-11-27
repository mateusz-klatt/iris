//go:build windows

package hook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"golang.org/x/sys/windows"
)

func TestWindowsOptionalEvidenceWriteFailuresON(t *testing.T) {
	t.Run("optional evidence write failure during PreTool edit emits SessionEvidenceWriteFailed without blocking Touch or grading silent", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "main")
		sibling := filepath.Join(parent, "sib")
		for _, dir := range []string{root, sibling} {
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		var denyTmp atomic.Bool
		origRestrict := m.restrict
		m.restrict = func(path string) error {
			if denyTmp.Load() && filepath.Dir(path) == m.dir {
				return errors.New("access denied")
			}
			if origRestrict != nil {
				return origRestrict(path)
			}
			return nil
		}

		h := &m2cHub{
			filesFunc: func(context.Context, hubclient.TouchFilesRequest) (hubclient.TouchResponse, error) {
				denyTmp.Store(false)
				return hubclient.TouchResponse{Action: "allow"}, nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin:             strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, root, filepath.Join(root, "src", "a.go"), filepath.Join(sibling, "b.go"))),
			Hub:               h,
			Memory:            m,
			Getenv:            getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			after:             never,
			logAfter:          never,
			Find: func(dir string) (gitinfo.Repo, error) {
				// Reject new temp-file creation in sessions dir after RegisterSession, inside fileGroups before finalAdmitAndPublish.
				if dir == sibling || strings.HasPrefix(dir, sibling) {
					denyTmp.Store(true)
					gd := filepath.Join(sibling, ".git")
					return gitinfo.Repo{Worktree: sibling, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
				}
				gd := filepath.Join(root, ".git")
				return gitinfo.Repo{Worktree: root, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
			},
		}.Run()
		denyTmp.Store(false)
		assertAllow(t, ClientClaude, out)
		if !slices.Contains(h.trace, fmt.Sprintf("touch_files:id=%s:gen=1", m2cExecID(1))) {
			t.Fatalf("optional write failure blocked TouchFiles: %v", h.trace)
		}
		obs, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(obs) != 2 {
			t.Fatalf("expected 2 SessionEvidenceWriteFailed observations (.execution and <hash>), got %+v err=%v", obs, err)
		}
		for _, o := range obs {
			if !strings.HasPrefix(o.Error, SessionEvidenceWriteFailed+" at ") {
				t.Fatalf("unexpected observation: %+v", o)
			}
		}
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) != 0 {
			t.Fatalf("optional write failure must not log silent failure: %+v err=%v", fails, err)
		}
	})

	t.Run("SessionEnd failed .ended write ON emits SessionEvidenceWriteFailed only with no repair", func(t *testing.T) {
		root := gitRepo(t)
		getenv := failureEnv(t)
		m := sessionsFor(getenv)
		if err := m.directory(); err != nil {
			t.Fatal(err)
		}
		marker := endedMarkerPath(m.name("claude", "s1"))
		if err := os.WriteFile(marker, []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ptr, err := windows.UTF16PtrFromString(extendedWindowsPath(marker))
		if err != nil {
			t.Fatal(err)
		}
		// Hold the old .ended marker open without FILE_SHARE_DELETE so Windows refuses replacement in forgetEvidence.
		handle, err := windows.CreateFile(ptr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		held := os.NewFile(uintptr(handle), marker)
		defer func() { _ = held.Close() }()

		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(70), 2, m2cExecID(69), "hook", "stale_retry", true), nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "SessionEnd",
			Stdin:             strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionEnd"}`),
			Hub:               h,
			Memory:            m,
			Getenv:            getenv,
			SuccessionEnabled: func(context.Context) bool { return true },
			Find:              func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
			after:             never,
			logAfter:          never,
		}.Run()
		_ = held.Close()
		assertAllow(t, ClientClaude, out)
		obs, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
		want := SessionEvidenceWriteFailed + " at " + marker
		if err != nil || len(obs) != 1 || obs[0].Error != want {
			t.Fatalf("observations=%+v err=%v want=%q", obs, err, want)
		}
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) != 0 {
			t.Fatalf("failed .ended write must not log failure or grade silent: %+v err=%v", fails, err)
		}
		wantTrace := []string{
			"register:gen=0:retry=1:end=1:opt=true",
			fmt.Sprintf("end:id=%s:gen=2", m2cExecID(70)),
		}
		if !slices.Equal(h.trace, wantTrace) {
			t.Fatalf("unexpected repair RPC after failed .ended write: %v", h.trace)
		}
	})
}
