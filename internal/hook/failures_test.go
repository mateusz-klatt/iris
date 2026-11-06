package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type failureHub struct{ stage string }

func (h failureHub) RegisterSession(ctx context.Context, _ hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	if h.stage == "session" {
		return hubclient.SessionResponse{}, &hubclient.Error{Code: "unauthorized", Message: "SECRET PAYLOAD"}
	}
	if h.stage == "timeout" {
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
		{"timeout", "session", func(r *Runner) { r.Hub = failureHub{stage: "timeout"}; r.Deadline = 10 * time.Millisecond }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := failureEnv(t)
			token := getenv("IRIS_TOKEN_FILE")
			r := Runner{ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(editPayload(ClientClaude, "/repo")), Getenv: getenv, Hub: failureHub{}, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: "/repo"}, nil }, Deadline: time.Second}
			tc.change(&r)
			out := r.Run()
			assertAllow(t, ClientClaude, out)
			deadline := time.Now().Add(time.Second)
			var entries []Failure
			for time.Now().Before(deadline) {
				var err error
				entries, err = ReadRecentFailures(token, 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) > 0 {
					break
				}
				time.Sleep(time.Millisecond)
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
	start := time.Now()
	out := (Runner{ClientName: "kimi", Stdin: strings.NewReader("{"), Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}}).Run()
	assertAllow(t, ClientKimi, out)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("logging delayed hook")
	}
}

func TestNormalAllowsDoNotLogFailures(t *testing.T) {
	getenv := failureEnv(t)
	for _, payload := range []string{`{"session_id":"s","cwd":"/outside","tool_name":"Read"}`, `{"session_id":"s","cwd":"/outside","tool_name":"Write","tool_input":{"path":"file"}}`} {
		out := (Runner{ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(payload), Getenv: getenv, Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo }}).Run()
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
	done := make(chan Output, 1)
	go func() {
		done <- (Runner{ClientName: "copilot", EventName: "preToolUse", Stdin: strings.NewReader("{"), Getenv: func(key string) string {
			<-unblock
			if key == "IRIS_TOKEN_FILE" {
				return token
			}
			return ""
		}}).Run()
	}()
	select {
	case out := <-done:
		assertAllow(t, ClientCopilot, out)
	case <-time.After(100 * time.Millisecond):
		close(unblock)
		t.Fatal("slow log configuration blocked the hook")
	}
	close(unblock)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entries, err := ReadRecentFailures(token, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("failure was not recorded after logging resumed")
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
	defer f.Close()
	if err = lockFailureLog(f); err != nil {
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
	unlockFailureLog(f)
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not resume after the lock was released")
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
	}}).Run()
	assertAllow(t, ClientKimi, out)
	deadline := time.Now().Add(time.Second)
	for {
		entries, err := ReadRecentFailures(token, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 {
			if entries[0].Stage == "" || entries[0].Client != "kimi" {
				t.Fatal(entries[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("malformed client.json left no failure trace")
		}
		time.Sleep(time.Millisecond)
	}
}
