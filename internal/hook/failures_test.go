package hook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func failureEnv(t *testing.T) func(string) string {
	t.Helper()
	token := filepath.Join(t.TempDir(), "hub.token")
	return func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
}

type failureHub struct {
	stage string
	// entered is closed when RegisterSession starts, for a deadline that must fire there.
	entered chan struct{}
}

func (h failureHub) RegisterSession(ctx context.Context, _ hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	if h.stage == "session" {
		return hubclient.SessionResponse{}, &hubclient.Error{Code: "unavailable", Message: "SECRET PAYLOAD"}
	}
	if h.stage == "timeout" {
		close(h.entered)
		<-ctx.Done()
		return hubclient.SessionResponse{}, ctx.Err()
	}
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: 1}, nil
}
func (h failureHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	if h.stage == "touch" {
		return hubclient.TouchResponse{}, &hubclient.Error{Code: "bad_request", Message: "SECRET PAYLOAD"}
	}
	return hubclient.TouchResponse{Action: "unexpected SECRET PAYLOAD"}, nil
}
func (h failureHub) EndExecution(context.Context, string, int) error {
	return errors.New("SECRET PAYLOAD")
}

type failureReader struct{}

func (failureReader) Read([]byte) (int, error) { return 0, errors.New("SECRET PAYLOAD") }

func TestFailureLogStagesAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		change      func(*Runner)
	}{
		{"read", "read", func(r *Runner) { r.Stdin = failureReader{} }},
		{"json", "decode", func(r *Runner) { r.Stdin = strings.NewReader(`{"SECRET PAYLOAD":`) }},
		{"missing-session", "decode", func(r *Runner) { r.Stdin = strings.NewReader(`{"cwd":"SECRET PAYLOAD"}`) }},
		{"client", "client", func(r *Runner) { r.ClientName = "unsupported" }},
		{"repository", "repository", func(r *Runner) {
			r.Find = func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, os.ErrPermission }
		}},
		{"hub", "hub", func(r *Runner) { r.Hub = nil }},
		{"session", "session", func(r *Runner) { r.Hub = failureHub{stage: "session"} }},
		{"touch", "touch", func(r *Runner) { r.Hub = failureHub{stage: "touch"} }},
		{"end", "end", func(r *Runner) { r.EventName = "SessionEnd" }},
		{"action", "touch", func(r *Runner) {}},
		{"paths", "paths", func(r *Runner) {
			r.Stdin = strings.NewReader(`{"session_id":"s","cwd":"/repo","tool_name":"Write","tool_input":{}}`)
		}},
		{"panic", "repository", func(r *Runner) { r.Find = func(string) (gitinfo.Repo, error) { panic("SECRET PAYLOAD") } }},
		{"timeout", "session", func(r *Runner) {
			// The deadline fires only once the hook is waiting on the hub, so the stage it
			// records cannot depend on how fast the machine reached it.
			entered := make(chan struct{})
			r.Hub = failureHub{stage: "timeout", entered: entered}
			r.after = func(time.Duration) <-chan time.Time {
				fired := make(chan time.Time)
				go func() { <-entered; close(fired) }()
				return fired
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := failureEnv(t)
			token := getenv("IRIS_TOKEN_FILE")
			r := Runner{ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(editPayload(ClientClaude, "/repo")), Getenv: getenv, Hub: failureHub{}, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, after: never, logAfter: never}
			tc.change(&r)
			out := r.Run()
			assertAllow(t, ClientClaude, out)
			entries, err := ReadRecentFailures(token, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("entries: %+v", entries)
			}
			entry := entries[0]
			if entry.Time.IsZero() || entry.Stage != tc.stage || entry.Event != r.EventName || entry.Error == "" {
				t.Fatal(entry)
			}
			raw, err := os.ReadFile(failurePath(token))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "SECRET PAYLOAD") {
				t.Fatalf("payload leaked: %s", raw)
			}
			time.Sleep(15 * time.Millisecond)
			entries, err = ReadRecentFailures(token, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("late worker logged twice: %v %v", entries, err)
			}
		})
	}
}
func TestFailureLogRotation(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	for i := 0; i < 500; i++ {
		if err := appendFailure(token, Failure{Time: time.Now().UTC(), Client: "claude", Event: "PreToolUse", Stage: "touch", Error: strings.Repeat("x", 200)}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(failurePath(token))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > failureLogLimit {
		t.Fatal(info.Size())
	}
	if info.Mode().Perm()&0077 != 0 && os.PathSeparator != '\\' {
		t.Fatal(info.Mode())
	}
	entries, err := ReadRecentFailures(token, 3)
	if err != nil || len(entries) != 3 {
		t.Fatal(entries, err)
	}
	if entries[2].Time.Before(entries[0].Time) {
		t.Fatal("entries out of order")
	}
}
func TestUnwritableFailureLogStillAllows(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.Mkdir(failurePath(token), 0700); err != nil {
		t.Fatal(err)
	}
	// That logging cannot delay the answer is TestSlowFailureLoggerDoesNotHoldHook's job, by
	// order. Here the failed write is awaited, so the test checks only that the hook allows.
	out := (Runner{ClientName: "kimi", Stdin: strings.NewReader("{"), Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}, after: never, logAfter: never}).Run()
	assertAllow(t, ClientKimi, out)
}

func TestNormalAllowsDoNotLogFailures(t *testing.T) {
	getenv := failureEnv(t)
	for _, payload := range []string{`{"session_id":"s","cwd":"/outside","tool_name":"Read"}`, `{"session_id":"s","cwd":"/outside","tool_name":"Write","tool_input":{"path":"file"}}`} {
		out := (Runner{ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(payload), Getenv: getenv, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo }, after: never}).Run()
		assertAllow(t, ClientClaude, out)
	}
	entries, err := ReadRecentFailures(getenv("IRIS_TOKEN_FILE"), 10)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestSlowFailureLoggerDoesNotHoldHook(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	unblock := make(chan struct{})
	written := make(chan struct{})
	// The budget expires as soon as it starts, so Run must answer while the writer is still
	// blocked in its configuration. Order, not speed, is what is checked.
	expired := func(time.Duration) <-chan time.Time {
		c := make(chan time.Time)
		close(c)
		return c
	}
	out := (Runner{ClientName: "copilot", EventName: "preToolUse", Stdin: strings.NewReader("{"), Getenv: func(key string) string {
		<-unblock
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}, logAfter: expired, logged: func() { close(written) }}).Run()
	assertAllow(t, ClientCopilot, out)
	close(unblock)
	<-written
	entries, err := ReadRecentFailures(token, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failure was not recorded after logging resumed: %v %v", entries, err)
	}
}

// A reader that arrives during a write waits. Windows file locks are mandatory, so
// without a shared lock the read fails. A writer that finds the file busy skips its
// entry, because logging must never delay a hook, so only reads must succeed here.
func TestFailureLogReadDuringWrites(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			entry := Failure{Time: time.Now().UTC(), Client: "claude", Event: "pre_tool", Stage: "session", Error: "x"}
			_ = appendFailure(token, entry)
		}
	}()
	for {
		if _, err := ReadRecentFailures(token, 10); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestFailureLogReadWaitsWhileWriteHoldsTheLock(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	entry := Failure{Time: time.Now().UTC(), Client: "claude", Event: "pre_tool", Stage: "session", Error: "held"}
	if err := appendFailure(token, entry); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(failurePath(token), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err = tryLockFile(f); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		entries, err := ReadRecentFailures(token, 10)
		if err != nil {
			read <- err
			return
		}
		if len(entries) != 1 || entries[0].Error != "held" {
			read <- errors.New("missing entry")
			return
		}
		read <- nil
	}()
	select {
	case err := <-read:
		t.Fatalf("read finished while the write lock was held: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	unlockFile(f)
	if err := <-read; err != nil {
		t.Fatalf("read did not resume after the lock was released: %v", err)
	}
}

func TestMalformedClientConfigStillLogsTheFailure(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	out := (Runner{ClientName: "kimi", Stdin: strings.NewReader("{"), Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}, after: never, logAfter: never}).Run()
	assertAllow(t, ClientKimi, out)
	entries, err := ReadRecentFailures(token, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("malformed client.json left no failure trace")
	}
	if entries[0].Stage == "" || entries[0].Client != "kimi" {
		t.Fatal(entries[0])
	}
}

func TestRecordFailureUsesRelocatedTokenFileFromClientConfig(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, home)
	}
	fallback, _, err := (hubclient.EnvConfig{Getenv: func(string) string { return "" }}).BaseTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	relocated := filepath.Join(t.TempDir(), "hub.token")
	if err := hubclient.WriteConfig(filepath.Join(filepath.Dir(fallback), "client.json"), hubclient.FileConfig{
		Hub:       "http://127.0.0.1:7878",
		TokenFile: relocated,
	}); err != nil {
		t.Fatal(err)
	}
	out := (Runner{
		ClientName: "claude",
		EventName:  "PreToolUse",
		Stdin:      strings.NewReader("{"),
		Getenv:     func(string) string { return "" },
		after:      never,
		logAfter:   never,
	}).Run()
	assertAllow(t, ClientClaude, out)
	entries, err := ReadRecentFailures(relocated, 10)
	if err != nil || len(entries) != 1 || entries[0].Stage != "decode" || entries[0].Client != "claude" {
		t.Fatalf("relocated log = %+v, err = %v", entries, err)
	}
	defaultEntries, err := ReadRecentFailures(fallback, 10)
	if err != nil || len(defaultEntries) != 0 {
		t.Fatalf("default log unexpectedly written: %+v, err = %v", defaultEntries, err)
	}
}

func TestRecordFailureFallsBackWhenClientConfigStalls(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, home)
	}
	fallback, _, err := (hubclient.EnvConfig{Getenv: func(string) string { return "" }}).BaseTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(fallback), 0o700); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	readDone := make(chan struct{})
	defer func() {
		close(unblock)
		<-readDone
	}()
	stalled := func(time.Duration) <-chan time.Time {
		c := make(chan time.Time)
		go func() {
			<-entered
			close(c)
		}()
		return c
	}
	out := (Runner{
		ClientName: "claude",
		EventName:  "SessionStart",
		Stdin:      strings.NewReader("{"),
		Getenv:     func(string) string { return "" },
		readConfig: func(string) ([]byte, error) {
			defer close(readDone)
			close(entered)
			<-unblock
			return nil, os.ErrNotExist
		},
		after:       never,
		logAfter:    never,
		configAfter: stalled,
	}).Run()
	assertAllow(t, ClientClaude, out)
	entries, err := ReadRecentFailures(fallback, 10)
	if err != nil || len(entries) != 1 || entries[0].Stage != "decode" || entries[0].Client != "claude" {
		t.Fatalf("fallback log = %+v, err = %v", entries, err)
	}
}

func TestAppendFailureDoesNotRecreateRemovedDirectory(t *testing.T) {
	removedRoot := filepath.Join(t.TempDir(), "iris-test-home-removed")
	removedDir := filepath.Join(removedRoot, "iris")
	token := filepath.Join(removedDir, "hub.token")
	err := appendFailure(token, Failure{Time: time.Now().UTC(), Client: "claude", Event: "SessionStart", Stage: "session", Error: "hook deadline exceeded"})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("appendFailure error = %v, want os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(removedRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing directory was created: stat err = %v", statErr)
	}

	// An abandoned recorder goroutine that finishes after its home directory is removed
	// must not recreate that directory.
	ephemeral := filepath.Join(t.TempDir(), "iris-test-home-ephemeral")
	if err := os.MkdirAll(ephemeral, 0o700); err != nil {
		t.Fatal(err)
	}
	ephemeralToken := filepath.Join(ephemeral, "hub.token")
	unblock := make(chan struct{})
	written := make(chan struct{})
	expired := func(time.Duration) <-chan time.Time {
		c := make(chan time.Time)
		close(c)
		return c
	}
	out := (Runner{
		ClientName: "claude",
		EventName:  "SessionStart",
		Stdin:      strings.NewReader("{"),
		Getenv: func(key string) string {
			<-unblock
			if key == "IRIS_TOKEN_FILE" {
				return ephemeralToken
			}
			return ""
		},
		logAfter: expired,
		logged:   func() { close(written) },
	}).Run()
	assertAllow(t, ClientClaude, out)
	if err := os.RemoveAll(ephemeral); err != nil {
		t.Fatal(err)
	}
	close(unblock)
	<-written
	if _, statErr := os.Stat(ephemeral); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("abandoned recorder recreated removed directory: stat err = %v", statErr)
	}
}

func TestPinnedObserverPerformsZeroConfigReads(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	var reads atomic.Int32
	r := Runner{
		ClientName: "codex",
		EventName:  "mcp",
		Getenv: func(key string) string {
			if key == "IRIS_TOKEN_FILE" {
				return token
			}
			return ""
		},
		readConfig: func(string) ([]byte, error) {
			reads.Add(1)
			return nil, os.ErrNotExist
		},
		logAfter: never,
	}
	for range 5 {
		r.record(stageObserved, "wake_stream_following", nil)
	}
	if got := reads.Load(); got != 0 {
		t.Fatalf("pinned observer read client.json %d times, want 0", got)
	}
	entries, err := ReadRecentObservations(token, 10)
	if err != nil || len(entries) != 5 {
		t.Fatalf("observations = %+v, err = %v", entries, err)
	}
}

func TestHookLogsTooLargeLimitsAndAllows(t *testing.T) {
	root := gitRepo(t)
	cases := []struct {
		name, event, stage, payload, caps, route, hubResp, wantErr string
		status                                                     gitStandIn
	}{
		{
			name:    "touch claim_limit",
			event:   "PreToolUse",
			stage:   "touch",
			payload: editPayload(ClientClaude, root),
			route:   "/v1/claims/touch",
			hubResp: `{"error":{"code":"too_large","message":"SECRET maximum 500 live claims","details":{"limit":"claim_limit"}}}`,
			wantErr: "hub request failed: too_large (claim_limit)",
		},
		{
			name:    "touch/files request_body_limit",
			event:   "PreToolUse",
			stage:   "touch",
			payload: editPayload(ClientClaude, root),
			caps:    `"capabilities":["file_claims"],`,
			route:   "/v1/claims/touch/files",
			hubResp: `{"error":{"code":"too_large","message":"SECRET body exceeds limit","details":{"limit":"request_body_limit"}}}`,
			wantErr: "hub request failed: too_large (request_body_limit)",
		},
		{
			name:    "reconcile older hub without details",
			event:   "PostToolUse",
			stage:   "shell session",
			payload: fmt.Sprintf(`{"session_id":"s1","cwd":%q,"tool_name":"Bash","tool_input":{"command":"ls"}}`, root),
			route:   "/v1/shell/reconcile",
			hubResp: `{"error":{"code":"too_large","message":"SECRET legacy body error"}}`,
			wantErr: "hub request failed: too_large (unknown)",
			status: func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				return map[string]string{"a.txt": "??"}, nil
			},
		},
		{
			name:    "touch unrecognized limit detail",
			event:   "PreToolUse",
			stage:   "touch",
			payload: editPayload(ClientClaude, root),
			route:   "/v1/claims/touch",
			hubResp: `{"error":{"code":"too_large","message":"SECRET","details":{"limit":"SECRET_DETAIL"}}}`,
			wantErr: "hub request failed: too_large (unknown)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sessions" {
					_, _ = fmt.Fprintf(w, `{"execution_id":"ex_1","generation":1,"handle":"ex_1",%s"agent":{"name":"claude-host"}}`, tc.caps)
					return
				}
				if r.URL.Path == tc.route {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = w.Write([]byte(tc.hubResp))
					return
				}
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer srv.Close()
			getenv := failureEnv(t)
			token := getenv("IRIS_TOKEN_FILE")
			r := Runner{
				ClientName:    "claude",
				EventName:     tc.event,
				Stdin:         strings.NewReader(tc.payload),
				Getenv:        getenv,
				Hub:           &hubclient.Client{Config: hubURL(srv.URL)},
				Status:        tc.status,
				ShellDeadline: time.Minute,
				after:         never,
				logAfter:      never,
			}
			out := r.Run()
			assertAllow(t, ClientClaude, out)
			entries, err := ReadRecentFailures(token, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries = %+v, err = %v", entries, err)
			}
			if entries[0].Stage != tc.stage || entries[0].Error != tc.wantErr {
				t.Fatalf("got %+v, want stage %q error %q", entries[0], tc.stage, tc.wantErr)
			}
			raw, err := os.ReadFile(failurePath(token))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "SECRET") {
				t.Fatalf("hub message or detail leaked: %s", raw)
			}
		})
	}
}

func TestHookLogsOverCapGitStatusAndAllows(t *testing.T) {
	root := t.TempDir()
	initCmd := exec.Command("git", "init", "-q", root)
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for i := range 800 {
		name := strings.Repeat("n", 100) + strconv.Itoa(1000+i)
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	getenv := failureEnv(t)
	token := getenv("IRIS_TOKEN_FILE")
	payload := fmt.Sprintf(`{"conversationId":"conv-1","workspacePaths":[%q],"toolCall":{"name":"run_command","args":{"CommandLine":"pip install pkg"}}}`, root)
	out := Runner{
		ClientName:    "antigravity",
		EventName:     "post_tool_use",
		Stdin:         strings.NewReader(payload),
		Getenv:        getenv,
		Hub:           reconcileHub{},
		ShellDeadline: time.Minute,
		after:         never,
		logAfter:      never,
	}.Run()
	wantAllow := Encode(Event{Client: ClientAntigravity, Kind: KindPostTool}, Decision{Action: ActionAllow})
	if out.Exit != wantAllow.Exit || string(out.Stdout) != string(wantAllow.Stdout) {
		t.Fatalf("got stdout %q stderr %q exit %d; want stdout %q exit %d", out.Stdout, out.Stderr, out.Exit, wantAllow.Stdout, wantAllow.Exit)
	}
	entries, err := ReadRecentFailures(token, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
	got := entries[0]
	if got.Stage != "shell status" || got.Error != failStatusTooLarge || got.Degraded() {
		t.Fatalf("got %+v, want stage shell status and error %q (non-degraded)", got, failStatusTooLarge)
	}
}
