package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

// The hooks across a restore (ADR 006): a copy made by `iris admin backup` runs on a new stream,
// and a copy without the restore marker keeps the stream.

// restoredHook is one hub address whose store a test replaces with a restored copy, as an
// operator restores a backup while the hooks keep their state, and the hooks of one Claude
// session on it.
type restoredHook struct {
	t            *testing.T
	livePath     string
	restoredPath string
	clock        store.Clock
	server       *httptest.Server
	http         *http.Client
	current      atomic.Pointer[hub.Hub]
	root         string
	getenv       func(string) string
	registered   hubclient.SessionResponse
}

func newRestoredHook(t *testing.T, clock store.Clock) (*restoredHook, *store.Store) {
	t.Helper()
	h := &restoredHook{t: t, livePath: filepath.Join(t.TempDir(), "live.db"), root: gitRepo(t), clock: clock}
	live, err := store.Open(h.livePath, clock)
	if err != nil {
		t.Fatal(err)
	}
	// The restore closes it; this closes it on an earlier failure too, so that Windows can remove
	// the directory.
	t.Cleanup(func() { _ = live.Close() })
	h.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.current.Load().ServeHTTP(w, r)
	}))
	h.serve(live)
	h.server.Start()
	t.Cleanup(h.server.Close)
	token := tokenFile(t)
	h.getenv = func(key string) string {
		switch key {
		case "IRIS_TOKEN_FILE":
			return token
		case "IRIS_HUB_URL":
			return h.server.URL
		}
		return ""
	}
	return h, live
}

func (h *restoredHook) serve(s *store.Store) {
	h.current.Store(hub.New(s, "secret", []string{h.server.Listener.Addr().String()}))
}

// run runs one hook of the session and returns what it printed.
func (h *restoredHook) run(event, payload string) string {
	h.t.Helper()
	client := lastSession{Client: &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}, HTTP: h.http}, last: &h.registered}
	out := h.runner(event, payload, client).Run()
	if out.Exit != 0 {
		h.t.Fatalf("exit %d: %s", out.Exit, out.Stderr)
	}
	return string(out.Stdout)
}

func (h *restoredHook) runner(event, payload string, client Hub) Runner {
	return Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Getenv: h.getenv, Hub: client, Host: func() string { return "testhost" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }, ShellDeadline: time.Hour, after: never, logAfter: never}
}

func (h *restoredHook) prompt() string {
	return h.run("UserPromptSubmit", `{"session_id":"s1","cwd":`+quote(h.root)+`,"hook_event_name":"UserPromptSubmit","prompt":"go"}`)
}

func (h *restoredHook) told() toldState {
	return toldOf(sessionsFor(h.getenv), ClientClaude, "s1")
}

// restore stops the live hub and serves a copy of the backup at saved in its place. Without
// marked, the copy loses its restore marker, as a copy made some other way than `iris admin
// backup` lacks it.
func (h *restoredHook) restore(live *store.Store, saved string, marked bool) *store.Store {
	h.t.Helper()
	_ = live.Close()
	path := filepath.Join(h.t.TempDir(), "restored.db")
	h.restoredPath = path
	raw, err := os.ReadFile(saved)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	if !marked {
		db, _ := sql.Open("sqlite", path)
		_, err := db.Exec("DELETE FROM RestoreMarker")
		_ = db.Close()
		if err != nil {
			h.t.Fatal(err)
		}
	}
	restored, err := store.Open(path, h.clock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = restored.Close() })
	if restored.Restored() != marked {
		h.t.Fatalf("restored %v, marked %v", restored.Restored(), marked)
	}
	h.serve(restored)
	return restored
}

// lastSession is a hook's hub client that keeps the hub's last answer to the hook's
// registration, so a test learns the session's agent name as the hook did.
type lastSession struct {
	*hubclient.Client
	last *hubclient.SessionResponse
}

func (h lastSession) RegisterSession(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	res, err := h.Client.RegisterSession(ctx, req)
	*h.last = res
	return res, err
}

func (h lastSession) RegisterOutsideSession(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	res, err := h.Client.RegisterOutsideSession(ctx, req)
	*h.last = res
	return res, err
}

// peer is a session of another client on the same hub, made through the store, which sends mail.
type peer struct {
	id   string
	gen  int64
	name string
}

func startPeer(t *testing.T, s *store.Store, session string) peer {
	t.Helper()
	out := storeCall(t, s, "sessions", store.Request{Client: "codex", Host: "testhost", SessionID: session, Cwd: "/" + session})
	p := peer{id: out["execution_id"].(string), gen: out["generation"].(int64)}
	raw, _ := json.Marshal(storeCall(t, s, "validate", store.Request{ExecutionID: p.id})["agent"])
	var agent struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &agent)
	p.name = agent.Name
	return p
}

// reattach presents the peer's generation to the restored store, as its own hooks would.
func (p peer) reattach(t *testing.T, s *store.Store, session string) {
	t.Helper()
	storeCall(t, s, "sessions", store.Request{Client: "codex", Host: "testhost", SessionID: session, Cwd: "/" + session, Generation: p.gen})
}

func (p peer) send(t *testing.T, s *store.Store, key, to string) {
	t.Helper()
	storeCall(t, s, "messages", store.Request{ExecutionID: p.id, Generation: p.gen, Key: key, To: []string{to}, Body: key})
}

func storeCall(t *testing.T, s *store.Store, op string, r store.Request) map[string]any {
	t.Helper()
	out, err := s.Call(context.Background(), op, r)
	if err != nil {
		t.Fatal(op, err)
	}
	return out
}

// A session was told mail at cursor R on the live hub. The hub is then restored from a backup
// taken before that, and the restored hub's high-water mark passes R before the session's hooks
// run again, so the told cursor is not above it and the hooks cannot see that the hub went back
// (ADR 017). Mail sent after the restore has attention at or below R. On a backup's marked copy
// the hub runs on a new stream, and the next prompt hook tells that mail. On an unmarked copy the
// stream is the same, and the hook stays silent: the gap the restore marker closes, which an
// unmarked copy still has. Newer mail is told either way.
func TestARestoredHubTellsAttentionBelowAToldCursor(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "unmarked"}[marked], func(t *testing.T) {
			h, live := newRestoredHook(t, nil)
			if said := h.prompt(); strings.Contains(said, "Iris:") {
				t.Fatalf("nothing is pending, yet the hook said %s", said)
			}
			name := h.registered.Agent.Name
			sender, other := startPeer(t, live, "sender"), startPeer(t, live, "other")
			saved := filepath.Join(t.TempDir(), "backup.db")
			if err := backup.Run(context.Background(), h.livePath, saved, io.Discard); err != nil {
				t.Fatal(err)
			}
			// After the backup: unrelated mail, so the cursor told next is far above the
			// backup's high-water mark, then mail for the session, which its prompt hook tells.
			for i := range 20 {
				sender.send(t, live, fmt.Sprint("u", i), other.name)
			}
			sender.send(t, live, "m1", name)
			if said := h.prompt(); !strings.Contains(said, "Iris: 1 unread message.") {
				t.Fatalf("the live hub's mail was not told: %s", said)
			}
			before := h.told()

			restored := h.restore(live, saved, marked)
			sender.reattach(t, restored, "sender")
			other.reattach(t, restored, "other")
			high := func() int64 {
				return storeCall(t, restored, "events", store.Request{ExecutionID: sender.id})["high_water"].(int64)
			}
			sender.send(t, restored, "m2", name)
			if at := high(); at > before.Cursor {
				t.Fatalf("m2's attention, at most %d, must be at or below the told cursor %d", at, before.Cursor)
			}
			for i := 0; high() < before.Cursor; i++ {
				sender.send(t, restored, fmt.Sprint("v", i), other.name)
			}
			said := h.prompt()
			if marked && !strings.Contains(said, "Iris: 1 unread message.") {
				t.Fatalf("the mail on the new stream was not told: %q", said)
			}
			if !marked {
				if strings.Contains(said, "Iris:") {
					t.Fatalf("an unmarked copy told mail below the told cursor: %q", said)
				}
				if after := h.told(); after.Stream != before.Stream || after.Cursor != before.Cursor {
					t.Fatalf("the told identity moved from %+v to %+v", before, after)
				}
			}
			sender.send(t, restored, "m3", name)
			if said := h.prompt(); !strings.Contains(said, "Iris: 2 unread messages.") {
				t.Fatalf("newer mail after the restore was not told: %q", said)
			}
		})
	}
}

// A touch answer cached before a backup is replayed verbatim after the restore, with the old
// stream's identity (claims.go keeps it for a retried tool call). On a marked copy, the session
// is told once on the new stream; the replay names the old stream, so it is told again; and the
// next fresh answer names the new stream, so it is told a third time, and then no more: a replay
// from before a restore can cause two extra announcements, and nothing is lost (ADR 006). On an
// unmarked copy the stream never changes, and the told mail is not told again.
func TestAReplayedTouchAfterARestoreTellsOnceMore(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "unmarked"}[marked], func(t *testing.T) {
			h, live := newRestoredHook(t, nil)
			h.prompt()
			sender := startPeer(t, live, "sender")
			sender.send(t, live, "m0", h.registered.Agent.Name)
			edit := editPayload(ClientClaude, h.root)
			if said := h.run("PreToolUse", edit); !strings.Contains(said, "Iris: 1 unread message.") {
				t.Fatalf("the edit hook did not tell the mail: %s", said)
			}
			saved := filepath.Join(t.TempDir(), "backup.db")
			if err := backup.Run(context.Background(), h.livePath, saved, io.Discard); err != nil {
				t.Fatal(err)
			}
			h.restore(live, saved, marked)
			replay := func() string { return h.run("PreToolUse", edit) }
			want := map[bool][]bool{true: {true, true, true, false}, false: {false, false, false, false}}[marked]
			for i, step := range []func() string{h.prompt, replay, h.prompt, h.prompt} {
				said := step()
				if told := strings.Contains(said, "Iris: 1 unread message."); told != want[i] || !told && strings.Contains(said, "Iris:") {
					t.Fatalf("step %d (fresh, replay, fresh, fresh) said %q", i+1, said)
				}
			}
		})
	}
}

type restoreTransport func(*http.Request) (*http.Response, error)

func (f restoreTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type floorRequest struct {
	route      string
	generation int
	floor      int
}

func TestHookKeepsItsProofAcrossARestore(t *testing.T) {
	for _, test := range []struct{ name, route string }{
		{"edit", "/v1/sessions"},
		{"outside-git", "/v1/sessions/outside"},
		{"full-shell", "/v1/shell/reconcile"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkRestoredHookStates(t, test.route, nil)
		})
	}
}

func checkRestoredHookStates(t *testing.T, route string, prepare func(*restoredHook)) {
	t.Helper()
	for _, state := range []string{"recovering", "terminal", "reattached", "empty"} {
		t.Run(state, func(t *testing.T) {
			checkRestoredHookFloor(t, state, route, prepare)
		})
	}
}

// checkRestoredHookFloor runs real hooks against a backed-up SQLite database. It advances the
// original hub through G3 so the hooks themselves write the proof that survives the restore.
func checkRestoredHookFloor(t *testing.T, state, route string, prepare func(*restoredHook)) {
	t.Helper()
	ctx := context.Background()
	clock := &hookClock{wall: time.Unix(1800000000, 0)}
	h, live := newRestoredHook(t, clock)
	outside := route == "/v1/sessions/outside"
	shell := strings.HasPrefix(route, "/v1/shell/")
	if outside || shell {
		h.root = t.TempDir()
	}
	if shell {
		if err := os.WriteFile(filepath.Join(h.root, "base"), []byte("base"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init"}, {"add", "base"}} {
			if out, err := exec.Command("git", append([]string{"-C", h.root}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %s, %v", args, out, err)
			}
		}
	}
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
	start := func(expected int) hubclient.SessionResponse {
		t.Helper()
		h.prompt()
		if got := h.registered; got.Generation != expected {
			t.Fatalf("wanted G%d, registered %+v", expected, got)
		}
		return h.registered
	}
	end := func(sess hubclient.SessionResponse) {
		t.Helper()
		if err := client.EndExecution(ctx, sess.ExecutionID, sess.Generation); err != nil {
			t.Fatal(err)
		}
	}
	var first hubclient.SessionResponse
	if state != "empty" {
		first = start(1)
		sender := startPeer(t, live, "sender")
		sender.send(t, live, "backup-mail", first.Agent.Name)
		if state == "terminal" {
			end(first)
		}
	}
	saved := filepath.Join(t.TempDir(), "backup.db")
	if err := backup.Run(ctx, h.livePath, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	switch state {
	case "empty":
		end(start(1))
	case "terminal":
	default:
		end(first)
	}
	end(start(2))
	start(3)
	h.restore(live, saved, true)
	if state == "reattached" {
		if err := client.Do(ctx, http.MethodPost, "/v1/executions/"+first.ExecutionID+"/heartbeat", map[string]any{"generation": 1, "source": "shim"}, nil); err != nil {
			t.Fatalf("exact G1 reattach on restored hub: %v", err)
		}
	}
	if prepare != nil {
		prepare(h)
	}
	var seen []floorRequest
	transport := h.server.Client().Transport
	h.http = &http.Client{Transport: restoreTransport(func(r *http.Request) (*http.Response, error) {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		defer func() { _ = body.Close() }()
		var req hubclient.SessionRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			return nil, err
		}
		seen = append(seen, floorRequest{r.URL.Path, req.Generation, req.RetryGeneration})
		return transport.RoundTrip(r)
	})}
	run := func() {
		t.Helper()
		event, payload := "PreToolUse", writePayload(h.root, "s1", "unguarded.go")
		if outside {
			event, payload = "UserPromptSubmit", `{"session_id":"s1","cwd":`+quote(h.root)+`,"prompt":"go"}`
		} else if shell {
			payload = `{"session_id":"s1","cwd":` + quote(h.root) + `,"tool_name":"Bash","tool_use_id":"shell","tool_input":{"command":"touch unguarded.go"}}`
		}
		if out := h.run(event, payload); out != string(FallbackAllow(ClientClaude).Stdout) {
			t.Fatalf("hook output %q", out)
		}
		if got := seen; !slices.Equal(got, []floorRequest{{route: route, generation: 3}}) {
			failures, _ := ReadRecentFailures(h.getenv("IRIS_TOKEN_FILE"), 10)
			t.Fatalf("the refused hook sent %+v, failures %+v", got, failures)
		}
		seen = nil
		memory := NewSessions(h.getenv("IRIS_TOKEN_FILE"))
		if got, present, err := memory.RecallGeneration("claude", "s1"); err != nil || !present || got != 3 {
			t.Fatalf("proof after refusal is %d, present %v, err %v", got, present, err)
		}
		notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 1)
		if err != nil || len(notes) != 1 || notes[0].Error != RestoreFloorRefused {
			t.Fatalf("restore refusal observation: %+v, %v", notes, err)
		}
	}
	if state == "recovering" {
		clock.advance(120*time.Second - time.Millisecond)
		run()
		clock.advance(time.Millisecond)
	}
	run()
	db, err := sql.Open("sqlite", h.restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var generations, highest, claims, snapshots, attachedDeliveries int
	err = db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM Execution WHERE client='claude' AND session_id='s1'),
		(SELECT COALESCE(MAX(generation),0) FROM Execution WHERE client='claude' AND session_id='s1'),
		(SELECT COUNT(*) FROM Claim WHERE path='unguarded.go'),
		(SELECT COUNT(*) FROM ShellSnapshot)+(SELECT COUNT(*) FROM ReducedShellSnapshot),
		(SELECT COUNT(*) FROM Delivery WHERE execution_id IS NOT NULL)`).Scan(&generations, &highest, &claims, &snapshots, &attachedDeliveries)
	want := 1
	if state == "empty" {
		want = 0
	}
	if err != nil || generations != want || highest != want || claims != 0 || snapshots != 0 || attachedDeliveries != 0 {
		t.Fatalf("restored authority changed: %d executions, highest G%d, %d claims, %d baselines, %d deliveries attached, err %v", generations, highest, claims, snapshots, attachedDeliveries, err)
	}
}

type interceptedRegistration struct {
	*hubclient.Client
	register func(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error)
}

func (h interceptedRegistration) RegisterSession(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	return h.register(ctx, req)
}

func TestRestoreFloorIsRecheckedWhenTheHubChangesBetweenRequests(t *testing.T) {
	ctx := context.Background()
	h, live := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
	end := func(expected int) {
		t.Helper()
		if got := h.registered.Generation; got != expected {
			t.Fatalf("G%d required, got G%d", expected, got)
		}
		if err := client.EndExecution(ctx, h.registered.ExecutionID, h.registered.Generation); err != nil {
			t.Fatal(err)
		}
	}
	h.prompt()
	saved := filepath.Join(t.TempDir(), "backup.db")
	if err := backup.Run(ctx, h.livePath, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	for generation := 1; generation < 4; generation++ {
		end(generation)
		if generation < 3 {
			h.prompt()
		}
	}
	ready, retry := make(chan hubclient.SessionRequest, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-retry:
		default:
			close(retry)
		}
	})
	wrapped := interceptedRegistration{Client: client, register: func(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
		if req.Generation == 0 {
			ready <- req
			<-retry
		}
		return client.RegisterSession(ctx, req)
	}}
	done := make(chan Output, 1)
	go func() { done <- h.runner("PreToolUse", writePayload(h.root, "s1", "unguarded.go"), wrapped).Run() }()
	select {
	case req := <-ready:
		if req.RetryGeneration != 3 {
			t.Fatalf("sent floor G%d, want G3", req.RetryGeneration)
		}
	case out := <-done:
		t.Fatalf("eligible retry was not sent: %s", out.Stdout)
	}
	restored := h.restore(live, saved, true)
	close(retry)
	assertAllow(t, ClientClaude, <-done)
	if got := sessionsFor(h.getenv).Recall("claude", "s1"); got != 3 {
		t.Fatalf("local G%d replaced G3", got)
	}
	notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 1)
	if err != nil || len(notes) != 1 || notes[0].Error != RestoreFloorRefused {
		t.Fatalf("notes %+v, %v", notes, err)
	}
	binding := storeCall(t, restored, "sessions", store.Request{Client: "claude", Host: "testhost", SessionID: "s1", Generation: 1, Worktree: h.root, Cwd: h.root})
	if binding["generation"] != int64(1) || binding["execution_id"] == "" {
		t.Fatalf("original G1 could not reattach exactly: %+v", binding)
	}
}

func TestADelayedRetryCannotReplaceNewerProof(t *testing.T) {
	ctx := context.Background()
	h, _ := newRestoredHook(t, &hookClock{wall: time.Unix(1800000000, 0)})
	client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: h.getenv}}
	h.prompt()
	if err := client.EndExecution(ctx, h.registered.ExecutionID, h.registered.Generation); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		req  hubclient.SessionRequest
		sess hubclient.SessionResponse
		err  error
	}
	ready, reply := make(chan answer, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-reply:
		default:
			close(reply)
		}
	})
	wrapped := interceptedRegistration{Client: client, register: func(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error) {
		sess, err := client.RegisterSession(ctx, req)
		if req.Generation == 0 {
			ready <- answer{req, sess, err}
			<-reply
		}
		return sess, err
	}}
	done := make(chan Output, 1)
	go func() { done <- h.runner("PreToolUse", writePayload(h.root, "s1", "late.go"), wrapped).Run() }()
	select {
	case first := <-ready:
		if first.err != nil || first.req.RetryGeneration != 1 || first.sess.Generation != 2 {
			t.Fatalf("first retry: %+v", first)
		}
		if err := client.EndExecution(ctx, first.sess.ExecutionID, first.sess.Generation); err != nil {
			t.Fatal(err)
		}
	case out := <-done:
		t.Fatalf("first hook skipped its eligible retry: %s", out.Stdout)
	}
	h.prompt()
	if got := h.registered.Generation; got != 3 {
		t.Fatalf("concurrent retry registered G%d, want G3", got)
	}
	close(reply)
	assertAllow(t, ClientClaude, <-done)
	memory := sessionsFor(h.getenv)
	if got := memory.Recall("claude", "s1"); got != 3 {
		t.Fatalf("the late G2 reply replaced G%d", got)
	}
	notes, err := ReadRecentObservations(h.getenv("IRIS_TOKEN_FILE"), 1)
	if err != nil || len(notes) != 1 || notes[0].Error != RestoreFloorRefused {
		t.Fatalf("late reply observation: %+v, %v", notes, err)
	}
}
