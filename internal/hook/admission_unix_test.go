//go:build !windows

package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
)

func TestUnixOptionalEvidenceWriteFailuresAndReducedRepairCarry(t *testing.T) {
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
		if err := os.MkdirAll(m.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(m.dir, 0o700) }()

		h := &m2cHub{
			filesFunc: func(context.Context, hubclient.TouchFilesRequest) (hubclient.TouchResponse, error) {
				if err := os.Chmod(m.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				return hubclient.TouchResponse{Action: "allow"}, nil
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "PreToolUse",
			Stdin: strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"move_file","tool_input":{"rename_from":%q,"rename_to":%q}}`, root, filepath.Join(root, "src", "a.go"), filepath.Join(sibling, "b.go"))),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
			Find: func(dir string) (gitinfo.Repo, error) {
				// Lock sessions dir read-only after RegisterSession, inside fileGroups before finalAdmitAndPublish writes.
				if dir == sibling || strings.HasPrefix(dir, sibling) {
					if err := os.Chmod(m.dir, 0o500); err != nil {
						t.Fatal(err)
					}
					gd := filepath.Join(sibling, ".git")
					return gitinfo.Repo{Worktree: sibling, GitDir: gd, CommonDir: gd, WorktreeID: "2:2"}, nil
				}
				gd := filepath.Join(root, ".git")
				return gitinfo.Repo{Worktree: root, GitDir: gd, CommonDir: gd, WorktreeID: "1:1"}, nil
			},
		}.Run()
		_ = os.Chmod(m.dir, 0o700)
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
		defer func() { _ = os.Chmod(m.dir, 0o700) }()

		h := &m2cHub{
			regFunc: func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
				return m2cClassifiedSession(m2cExecID(70), 2, m2cExecID(69), "hook", "stale_retry", true), nil
			},
			endFunc: func(context.Context, string, int) error {
				return os.Chmod(m.dir, 0o500)
			},
		}
		out := Runner{
			ClientName: "claude", EventName: "SessionEnd",
			Stdin: strings.NewReader(`{"session_id":"s1","cwd":` + quote(root) + `,"hook_event_name":"SessionEnd"}`),
			Hub:   h, Memory: m, Getenv: getenv, SuccessionEnabled: func(context.Context) bool { return true },
			after: never, logAfter: never,
		}.Run()
		_ = os.Chmod(m.dir, 0o700)
		assertAllow(t, ClientClaude, out)
		obs, err := ReadRecentObservations(getenv("IRIS_TOKEN_FILE"), 10)
		want := SessionEvidenceWriteFailed + " at " + endedMarkerPath(m.name("claude", "s1"))
		if err != nil || len(obs) != 1 || obs[0].Error != want {
			t.Fatalf("observations=%+v err=%v want=%q", obs, err, want)
		}
		fails, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
		if err != nil || len(fails) != 0 {
			t.Fatalf("failed .ended write must not log failure or grade silent: %+v err=%v", fails, err)
		}
	})

	t.Run("shellReduced repair carries same floor and consumed retry budget", func(t *testing.T) {
		f := newScopeFixture(t)
		f.reduced(time.Minute)
		f.m.Remember("claude", "s", 4)
		m2cWriteExact(t, f.m, "claude", "s", sessionproof.Record{
			Version: sessionproof.FormatVersion, Origin: sessionproof.OriginHook, Generation: 4, ExecutionID: m2cExecID(80),
		})

		h := &m2cScopedCombinedHub{&m2cHub{}}
		regCalls := 0
		h.regFunc = func(_ context.Context, _ hubclient.SessionRequest) (hubclient.SessionResponse, error) {
			regCalls++
			switch regCalls {
			case 1:
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":4}`)}
			case 2:
				resp := m2cClassifiedSession(m2cExecID(81), 5, m2cExecID(80), "hook", "stale_retry", true)
				resp.SessionID = "s"
				return resp, nil
			default:
				// Repair path after ReconcileScoped lost must NOT retry a 2nd time!
				return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":5}`)}
			}
		}
		h.scopedFn = func(context.Context, hubclient.ScopedRequest) (hubclient.CheckResponse, error) {
			return hubclient.CheckResponse{}, &hubclient.Error{Code: "lost", Details: json.RawMessage(`{"highest_generation":5}`)}
		}

		f.r.Hub = h
		f.r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
			return map[string]string{}, nil
		}
		f.r.SuccessionEnabled = func(context.Context) bool { return true }
		f.r.ShellDeadline = time.Second
		f.r.reconcile(context.Background(), f.ev, &runState{})

		want := []string{
			"register:gen=4:retry=0:end=0:opt=true",
			"register:gen=0:retry=4:end=0:opt=true",
			fmt.Sprintf("reconcile_scoped:id=%s:gen=5", m2cExecID(81)),
			// Repair receives genFloor=5 from attempt and MUST NOT retry a 2nd time!
			"register:gen=5:retry=0:end=0:opt=true",
		}
		if !slices.Equal(h.trace, want) {
			t.Fatalf("shellReduced repair trace=%v\nwant=%v", h.trace, want)
		}
	})
}

func TestUnixShellEndedChecksAndScopedVetos(t *testing.T) {
	t.Run("plain Reconcile and ReconcileScoped checkEndedRegistration and combined veto branches", func(t *testing.T) {
		for _, mode := range []string{"plain_ended", "scoped_ended", "combined_unusable_no_fallback", "combined_post_veto", "repair_final_veto"} {
			t.Run(mode, func(t *testing.T) {
				f := newScopeFixture(t)
				f.reduced(time.Minute)
				h := &m2cScopedCombinedHub{&m2cHub{}}
				switch mode {
				case "plain_ended":
					h.regFunc = func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						return m2cClassifiedSession(m2cExecID(90), 1, "", "hook", "initial", true, "file_claims", hubclient.SuccessionProofCapability), nil
					}
					h.reconcileFn = func(context.Context, hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
						f.m.Forget("claude", "s", 1)
						return hubclient.CheckResponse{}, nil
					}
				case "scoped_ended":
					h.regFunc = func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						return m2cClassifiedSession(m2cExecID(91), 1, "", "hook", "initial", true), nil
					}
					h.scopedFn = func(context.Context, hubclient.ScopedRequest) (hubclient.CheckResponse, error) {
						f.m.Forget("claude", "s", 1)
						return hubclient.CheckResponse{}, nil
					}
				case "combined_unusable_no_fallback":
					f.warm()
					h.scopedCombFn = func(context.Context, hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error) {
						return hubclient.ShellResponse{Session: hubclient.SessionResponse{ExecutionID: m2cExecID(92), Generation: 1, Capabilities: []string{"shell_scope", "file_claims", hubclient.SuccessionProofCapability}}}, nil
					}
				case "combined_post_veto":
					f.warm()
					h.scopedCombFn = func(context.Context, hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error) {
						f.m.Forget("claude", "s", 1)
						return hubclient.ShellResponse{Session: m2cClassifiedSession(m2cExecID(93), 1, "", "hook", "initial", true)}, nil
					}
				case "repair_final_veto":
					f.warm()
					h.scopedCombFn = func(context.Context, hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error) {
						return hubclient.ShellResponse{}, &hubclient.Error{Code: "lost"}
					}
					h.regFunc = func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
						f.m.Forget("claude", "s", 1)
						return m2cClassifiedSession(m2cExecID(94), 1, "", "hook", "initial", true), nil
					}
				}
				f.r.Hub = h
				f.r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
					return map[string]string{}, nil
				}
				f.r.SuccessionEnabled = func(context.Context) bool { return true }
				f.r.ShellDeadline = time.Second
				out := f.r.reconcile(context.Background(), f.ev, &runState{})
				if mode == "combined_unusable_no_fallback" {
					assertAllow(t, ClientClaude, out)
					if len(h.trace) != 1 || !strings.HasPrefix(h.trace[0], "reconcile_scoped_session") {
						t.Fatalf("%s fell back unexpectedly: %v", mode, h.trace)
					}
					return
				}
				if !strings.Contains(string(out.Stdout), string(ActionDeny)) {
					t.Fatalf("%s: expected PreToolUse Deny after ended veto, got %s trace=%v", mode, out.Stdout, h.trace)
				}
			})
		}
	})
}
