package hook

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

func newFileMemory(t *testing.T) fileMemory {
	t.Helper()
	return fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: time.Now, restrict: config.OwnerOnly}
}

func TestFileMemoryRemembersEachSessionApart(t *testing.T) {
	m := newFileMemory(t)
	if m.Recall("claude", "s") != 0 {
		t.Fatal("an unknown session has a generation")
	}
	m.Remember("claude", "s", 3)
	m.Remember("codex", "s", 7)
	if m.Recall("claude", "s") != 3 || m.Recall("codex", "s") != 7 {
		t.Fatal(m.Recall("claude", "s"), m.Recall("codex", "s"))
	}
	m.Remember("claude", "s", 4)
	if m.Recall("claude", "s") != 4 {
		t.Fatal("a later generation replaces the earlier one")
	}
	m.Forget("claude", "s")
	if m.Recall("claude", "s") != 0 || m.Recall("codex", "s") != 7 {
		t.Fatal("forget removed the wrong session")
	}
	// Windows has no mode bits: the files take the ACL of the user's configuration directory.
	if runtime.GOOS == "windows" {
		return
	}
	for _, info := range mustList(t, m.dir) {
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", info.Name(), info.Mode())
		}
	}
	if dir, err := os.Stat(m.dir); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatal(dir, err)
	}
}

func mustList(t *testing.T, dir string) []os.FileInfo {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []os.FileInfo
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, info)
	}
	return out
}

func TestFileMemoryTreatsDamageAsNoProof(t *testing.T) {
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	for _, body := range []string{"", "x", "-2"} {
		if err := os.WriteFile(m.name("claude", "s"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if m.Recall("claude", "s") != 0 {
			t.Fatalf("%q read as a generation", body)
		}
	}
}

func TestFileMemoryFailsQuietly(t *testing.T) {
	blocked := t.TempDir()
	file := filepath.Join(blocked, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := fileMemory{dir: filepath.Join(file, "sessions"), now: time.Now, restrict: config.OwnerOnly}
	m.Remember("claude", "s", 1)
	m.Forget("claude", "s")
	m.prune()
	if m.Recall("claude", "s") != 0 {
		t.Fatal("nothing could have been stored")
	}

	// The target is a directory, so the rename fails and the temporary file must go.
	m = newFileMemory(t)
	if err := os.MkdirAll(filepath.Join(m.name("claude", "s"), "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.Remember("claude", "s", 1)
	for _, info := range mustList(t, m.dir) {
		if strings.HasPrefix(info.Name(), ".tmp-") {
			t.Fatal("a failed write left", info.Name())
		}
	}

	// A mode of 0500 refuses a write only on POSIX, and never for root.
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		readOnly := newFileMemory(t)
		if err := os.MkdirAll(readOnly.dir, 0o500); err != nil {
			t.Fatal(err)
		}
		readOnly.Remember("claude", "s", 1)
		if readOnly.Recall("claude", "s") != 0 {
			t.Fatal("a read-only directory took a write")
		}
	}
}

func TestFileMemoryPrunesOnlyStaleEntries(t *testing.T) {
	now := time.Now()
	m := fileMemory{dir: filepath.Join(t.TempDir(), "sessions"), now: func() time.Time { return now }, restrict: config.OwnerOnly}
	m.Remember("claude", "old", 1)
	m.Remember("claude", "new", 2)
	old := now.Add(-memoryAge - time.Hour)
	if err := os.Chtimes(m.name("claude", "old"), old, old); err != nil {
		t.Fatal(err)
	}
	m.prune()
	if m.Recall("claude", "old") != 0 || m.Recall("claude", "new") != 2 {
		t.Fatal("prune removed the wrong entry")
	}
}

func TestMemoryLivesBesideTheToken(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	r := Runner{Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}}
	m, ok := r.memory().(fileMemory)
	if !ok || m.dir != filepath.Join(filepath.Dir(token), "sessions") || m.restrict == nil {
		t.Fatalf("%#v", r.memory())
	}
	if (Runner{}).memory() != nil {
		t.Fatal("a Runner without an environment has nowhere private to write")
	}
	given := &recoveryHub{}
	if got, ok := (Runner{Memory: fakeMemory{given}}).memory().(fakeMemory); !ok || got.h != given {
		t.Fatal("an injected Memory was ignored")
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, "")
	}
	if (Runner{Getenv: func(string) string { return "" }}).memory() != nil {
		t.Fatal("no token path, so no memory")
	}
}

type fakeMemory struct{ h *recoveryHub }

func (fakeMemory) Recall(string, string) int    { return 0 }
func (fakeMemory) Remember(string, string, int) {}
func (fakeMemory) Forget(string, string)        {}

// recoveryHub plays the hub's side of D9: an Execution in recovery refuses a request
// without its generation, and a stale generation is refused outright.
type recoveryHub struct {
	mu         sync.Mutex
	generation int
	recovering bool
	sent       []int
	touched    int
	ended      int
}

func (h *recoveryHub) RegisterSession(_ context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, r.Generation)
	switch {
	case r.Generation != 0 && r.Generation != h.generation:
		return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Message: "session generation no longer exists"}
	case h.recovering && r.Generation == 0:
		return hubclient.SessionResponse{}, &hubclient.Error{Code: "lost", Message: "recovery requires the prior session generation"}
	}
	h.recovering = false
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: h.generation}, nil
}

func (h *recoveryHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	h.mu.Lock()
	h.touched++
	h.mu.Unlock()
	return hubclient.TouchResponse{Action: "allow"}, nil
}

func (h *recoveryHub) EndExecution(context.Context, string, int) error {
	h.ended++
	return nil
}

func writePayload(root, session, file string) string {
	return `{"session_id":` + quote(session) + `,"cwd":` + quote(root) + `,"hook_event_name":"PreToolUse","tool_name":"Write","tool_use_id":"t-` + file + `","tool_input":{"file_path":` + quote(filepath.Join(root, file)) + `}}`
}

func runWith(h Hub, m Memory, event, payload string) Output {
	return Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: h, Getenv: func(string) string { return "" }, Memory: m}.Run()
}

func TestARememberedGenerationReattachesARecoveringSession(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	h := &recoveryHub{generation: 2}
	runWith(h, m, "SessionStart", `{"session_id":"s","cwd":`+quote(root)+`}`)
	if m.Recall("claude", "s") != 2 {
		t.Fatal("session start did not remember its generation")
	}
	h.recovering = true
	out := runWith(h, m, "PreToolUse", writePayload(root, "s", "a.go"))
	if strings.Contains(string(out.Stdout), "deny") || h.touched != 1 {
		t.Fatalf("a session with proof was refused: %s", out.Stdout)
	}
	if h.sent[len(h.sent)-1] != 2 {
		t.Fatal("the hook did not send its proof", h.sent)
	}
}

func TestAnEditWithoutProofIsRefusedNotAllowed(t *testing.T) {
	root := gitRepo(t)
	h := &recoveryHub{generation: 1, recovering: true}
	out := runWith(h, newFileMemory(t), "PreToolUse", writePayload(root, "s", "a.go"))
	text := string(out.Stdout)
	if !strings.Contains(text, `"permissionDecision":"deny"`) || !strings.Contains(text, "hub restarted") {
		t.Fatalf("a recovering session's edit must be refused: %s", text)
	}
	if h.touched != 0 {
		t.Fatal("the refused edit reached touch")
	}
}

func TestAStaleGenerationIsDroppedAndTheCallRepeated(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	h := &recoveryHub{generation: 5}
	out := runWith(h, m, "PreToolUse", writePayload(root, "s", "a.go"))
	if strings.Contains(string(out.Stdout), "deny") || h.touched != 1 {
		t.Fatalf("%s", out.Stdout)
	}
	if len(h.sent) != 2 || h.sent[0] != 1 || h.sent[1] != 0 {
		t.Fatal("the hook must retry once without the stale generation", h.sent)
	}
	if m.Recall("claude", "s") != 5 {
		t.Fatal("the current generation was not remembered")
	}
}

func TestAStaleGenerationDuringRecoveryStillRefuses(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	m.Remember("claude", "s", 1)
	h := &recoveryHub{generation: 5, recovering: true}
	out := runWith(h, m, "PreToolUse", writePayload(root, "s", "a.go"))
	if !strings.Contains(string(out.Stdout), `"permissionDecision":"deny"`) || h.touched != 0 {
		t.Fatalf("%s", out.Stdout)
	}
}

func TestSessionStartAndEndStillFailOpenAndEndForgets(t *testing.T) {
	root := gitRepo(t)
	m := newFileMemory(t)
	h := &recoveryHub{generation: 1, recovering: true}
	start := `{"session_id":"s","cwd":` + quote(root) + `}`
	if out := runWith(h, m, "SessionStart", start); strings.Contains(string(out.Stdout), "deny") {
		t.Fatal("session start is not a tool and must not refuse")
	}
	h.recovering = false
	runWith(h, m, "SessionStart", start)
	if m.Recall("claude", "s") != 1 {
		t.Fatal(m.Recall("claude", "s"))
	}
	runWith(h, m, "SessionEnd", start)
	if h.ended != 1 || m.Recall("claude", "s") != 0 {
		t.Fatal("session end must forget the generation", h.ended)
	}
}

func TestAnotherHubFailureStillFailsOpen(t *testing.T) {
	root := gitRepo(t)
	out := runWith(scriptHub{regErr: errors.New("down")}, newFileMemory(t), "PreToolUse", writePayload(root, "s", "a.go"))
	if strings.Contains(string(out.Stdout), "deny") {
		t.Fatalf("only a recovering session refuses; an outage keeps D3: %s", out.Stdout)
	}
}

// hookClock lets the test move the hub past its recovery deadline.
type hookClock struct {
	mu   sync.Mutex
	wall time.Time
	tick int64
}

func (c *hookClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}
func (c *hookClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick
}
func (c *hookClock) advance(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.tick += d.Milliseconds()
	c.mu.Unlock()
}

// The window from D9, against the real store and hub: a restart, then edits from a session
// that kept its proof, one that lost it, and one that never had a claim.
func TestTheRecoveryWindowNeverAllowsAnUnclaimedEdit(t *testing.T) {
	root := gitRepo(t)
	file := filepath.Join(t.TempDir(), "hub.db")
	clock := &hookClock{wall: time.Unix(1800000000, 0)}
	token := tokenFile(t)
	var url string
	start := func() func() {
		database, err := store.Open(file, clock)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(nil)
		server.Config.Handler = hub.New(database, "secret", []string{server.Listener.Addr().String()})
		server.Start()
		url = server.URL
		return func() { server.Close(); database.Close() }
	}
	env := func(k string) string {
		switch k {
		case "IRIS_TOKEN_FILE":
			return token
		case "IRIS_HUB_URL":
			return url
		}
		return ""
	}
	hook := func(event, payload string) string {
		client := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: env}}
		out := Runner{ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload), Hub: client, Getenv: env, Deadline: 10 * time.Second}.Run()
		return string(out.Stdout)
	}
	allowed := func(out string) bool { return !strings.Contains(out, `"permissionDecision"`) }
	stop := start()
	for _, s := range []string{"kept", "forgot"} {
		hook("SessionStart", `{"session_id":"`+s+`","cwd":`+quote(root)+`}`)
		if out := hook("PreToolUse", writePayload(root, s, s+".go")); !allowed(out) {
			t.Fatal(out)
		}
	}
	stop()
	stop = start()
	defer func() { stop() }()
	(fileMemory{dir: filepath.Join(filepath.Dir(token), "sessions")}).Forget("claude", "forgot")

	if out := hook("PreToolUse", writePayload(root, "forgot", "new.go")); !strings.Contains(out, "hub restarted") {
		t.Fatalf("an edit without proof was not refused: %s", out)
	}
	if out := hook("PreToolUse", writePayload(root, "kept", "next.go")); !allowed(out) {
		t.Fatalf("a session with proof was refused: %s", out)
	}
	hook("SessionStart", `{"session_id":"fresh","cwd":`+quote(root)+`}`)
	if out := hook("PreToolUse", writePayload(root, "fresh", "forgot.go")); !strings.Contains(out, "is held by") {
		t.Fatalf("a recovering session's claim must still block others: %s", out)
	}
	clock.advance(121 * time.Second)
	if out := hook("PreToolUse", writePayload(root, "forgot", "new.go")); !allowed(out) {
		t.Fatalf("after the deadline the session starts afresh: %s", out)
	}
	if out := hook("PreToolUse", writePayload(root, "fresh", "new.go")); !strings.Contains(out, "is held by") {
		t.Fatalf("the fresh generation's edit was not claimed: %s", out)
	}
}

// A file that cannot be protected is not kept, and neither is one in an unprotected directory.
func TestFileMemoryKeepsNothingItCannotProtect(t *testing.T) {
	refused := errors.New("acl")
	for name, failOn := range map[string]func(string, string) bool{
		"directory": func(path, dir string) bool { return path == dir },
		"file":      func(path, dir string) bool { return path != dir },
	} {
		m := newFileMemory(t)
		m.restrict = func(path string) error {
			if failOn(path, m.dir) {
				return refused
			}
			return nil
		}
		m.Remember("claude", "s", 3)
		if m.Recall("claude", "s") != 0 {
			t.Fatalf("%s: an unprotected generation was kept", name)
		}
		if entries, err := os.ReadDir(m.dir); err == nil && len(entries) != 0 {
			t.Fatalf("%s: left %d entries", name, len(entries))
		}
	}
}
