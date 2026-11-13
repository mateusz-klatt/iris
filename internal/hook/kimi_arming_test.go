package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// kimiSession, the captured session, is golden_test.go's. kimiCleared is the session /clear
// started in ADR 030's measurement.
const (
	kimiWakePID   = 4242
	kimiWakeStart = int64(1_700_000_000_000)
	kimiCleared   = "session_638c5a91-9064-450e-8a7a-6f48c85d9186"
)

// kimiHarness runs Kimi hooks in a repository, under a Kimi process, with a pinned directory of
// their own.
type kimiHarness struct {
	t      *testing.T
	pinned string
	repo   string
	env    func(string) string
	now    time.Time
	anc    proc.Ancestor
	hub    Hub
	pinErr error
}

func newKimiHarness(t *testing.T) *kimiHarness {
	t.Helper()
	return &kimiHarness{
		t: t, pinned: t.TempDir(), repo: gitRepo(t), env: failureEnv(t), now: time.UnixMilli(1_763_000_000_000),
		anc: proc.Ancestor{PID: kimiWakePID, StartMS: kimiWakeStart, Name: "/home/user/.local/bin/kimi"}, hub: scriptHub{},
	}
}

func (h *kimiHarness) runner(event, session, extra string) Runner {
	body := `{"hook_event_name":"` + event + `","session_id":"` + session + `","cwd":` + quote(h.repo) + `,"client_type":"kimi_code_cli"` + extra + `}`
	return Runner{
		ClientName: "kimi", EventName: event, Stdin: strings.NewReader(body), Hub: h.hub, Getenv: h.env,
		Ancestor:        func() (proc.Ancestor, error) { return h.anc, nil },
		Find:            func(string) (gitinfo.Repo, error) { return gitinfo.Find(h.repo) },
		PinnedConfigDir: func() (string, error) { return h.pinned, h.pinErr },
		Now:             func() time.Time { return h.now },
		after:           never, logAfter: never,
	}
}

func (h *kimiHarness) run(event, session, extra string) Output {
	h.t.Helper()
	return h.runner(event, session, extra).Run()
}

func (h *kimiHarness) start(session string) Output {
	h.t.Helper()
	return h.run("SessionStart", session, "")
}

// turnPrompt is a prompt's payload fields when it starts a turn.
const turnPrompt = `,"prompt":[{"type":"text","text":"hello"}],"is_steer":false`

// prompt submits a prompt that starts a turn; steer submits one typed into a running turn.
func (h *kimiHarness) prompt(session string) Output {
	h.t.Helper()
	return h.run("UserPromptSubmit", session, turnPrompt)
}

func (h *kimiHarness) steer(session string) Output {
	h.t.Helper()
	return h.run("UserPromptSubmit", session, `,"prompt":[{"type":"text","text":"also this"}],"is_steer":true`)
}

func (h *kimiHarness) root() string { return kimiwake.Root(h.pinned) }

func (h *kimiHarness) wake(text string) {
	h.t.Helper()
	if err := os.MkdirAll(h.root(), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root(), "preference"), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *kimiHarness) store() *kimiwake.Store { return &kimiwake.Store{Root: h.root()} }

func (h *kimiHarness) mapped() string {
	h.t.Helper()
	id, _, err := h.store().Session(kimiWakePID, kimiWakeStart, time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *kimiHarness) record(session string) (kimiwake.Record, bool) {
	h.t.Helper()
	r, exists, err := h.store().Read(kimiwake.Key{PID: kimiWakePID, StartMS: kimiWakeStart, Session: session}, time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	return r, exists
}

// asked asserts the exact request of ADR 030 as the prompt's whole added context.
func asked(t *testing.T, out Output) {
	t.Helper()
	if out.Exit != 0 || string(out.Stdout) != kimiArmRequest+"\n" {
		t.Fatalf("the prompt did not ask for the watcher: exit %d stdout %q stderr %q", out.Exit, out.Stdout, out.Stderr)
	}
}

// quiet asserts a prompt that does not ask, and is not blocked.
func quiet(t *testing.T, out Output) {
	t.Helper()
	if out.Exit != 0 || strings.Contains(string(out.Stdout), "iris watch --client kimi --once") {
		t.Fatalf("the prompt asked or was blocked: exit %d stdout %q stderr %q", out.Exit, out.Stdout, out.Stderr)
	}
}

// The request is ADR 030's text, word for word: the exact command, description and options, that it
// comes from Iris and not the person, and that Iris cannot check the command the model runs.
func TestKimiArmRequestIsTheTextOfADR030(t *testing.T) {
	adr, err := os.ReadFile(filepath.Join("..", "..", "docs", "adr", "030-kimi-one-shot-wake.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(adr), "```text\n"+kimiArmRequest+"\n```") {
		t.Fatal("the arming request differs from the text ADR 030 fixes")
	}
}

// Kimi marks a prompt typed into a running turn with is_steer; the captured ordinary prompt is not
// one.
func TestKimiPromptSaysWhetherItSteers(t *testing.T) {
	captured, err := os.ReadFile("testdata/kimi/userpromptsubmit.json")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Decode(ClientKimi, "UserPromptSubmit", captured)
	if err != nil || ev.Steer {
		t.Fatalf("captured prompt: %+v %v", ev, err)
	}
	steer := strings.Replace(string(captured), `"is_steer": false`, `"is_steer": true`, 1)
	if ev, err := Decode(ClientKimi, "UserPromptSubmit", []byte(steer)); err != nil || !ev.Steer {
		t.Fatalf("steer: %+v %v", ev, err)
	}
}

// With wake on, the first prompt after the hub accepted the session asks for the watcher. A request
// still pending at the next prompt was not answered by the turn that got it, so it latches and no
// prompt asks, until a SessionStart of the session re-arms it.
func TestKimiPromptAsksOnceThenLatchesUntilSessionStart(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	if out := h.start(kimiSession); out.Exit != 0 || len(out.Stdout) == 0 {
		t.Fatalf("SessionStart: %+v", out)
	}
	asked(t, h.prompt(kimiSession))
	h.now = h.now.Add(time.Minute)
	quiet(t, h.prompt(kimiSession))
	if r, _ := h.record(kimiSession); r.Latch == nil || r.Latch.State != kimiwake.LatchFailed {
		t.Fatalf("an unanswered request did not latch: %+v", r)
	}
	quiet(t, h.prompt(kimiSession))
	h.start(kimiSession)
	asked(t, h.prompt(kimiSession))
}

// A steer starts no turn: it never asks, and it never judges a pending request, which only the next
// prompt that starts a turn latches.
func TestKimiSteerNeitherAsksNorJudges(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.start(kimiSession)
	quiet(t, h.steer(kimiSession))
	if _, exists := h.record(kimiSession); exists {
		t.Fatal("a steer created a record")
	}
	asked(t, h.prompt(kimiSession))
	requested, _ := h.record(kimiSession)
	quiet(t, h.steer(kimiSession))
	if r, _ := h.record(kimiSession); r.Latch != nil || r.ArmRequestedMS != requested.ArmRequestedMS {
		t.Fatalf("a steer judged the pending request: %+v", r)
	}
	quiet(t, h.prompt(kimiSession))
	if r, _ := h.record(kimiSession); r.Latch == nil {
		t.Fatalf("the next prompt did not latch: %+v", r)
	}
}

// The request follows the prompt's attention note on a line of its own.
func TestKimiArmRequestFollowsTheAttentionNote(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.start(kimiSession)
	calls := 0
	h.hub = attentionHub{calls: &calls, now: &hubclient.Attention{
		Unread: 1, HighWater: 5, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "s1", Cursor: 5},
	}}
	out := h.prompt(kimiSession)
	lines := strings.Split(strings.TrimSuffix(string(out.Stdout), "\n"), "\n")
	if out.Exit != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "Iris: ") || !strings.Contains(lines[0], "message") || lines[1] != kimiArmRequest {
		t.Fatalf("stdout %q", out.Stdout)
	}
}

// The request is decided before the attention note records what the session was told. A deadline
// that passes while arming loses the note, so its attention must stay untold for a later prompt to
// say it.
func TestADeadlineWhileArmingLeavesTheAttentionUntold(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.start(kimiSession)
	calls := 0
	h.hub = attentionHub{calls: &calls, now: &hubclient.Attention{
		Unread: 1, HighWater: 5, Identity: hubclient.Identity{Known: true, Pending: true, Stream: "s1", Cursor: 5},
	}}
	r := h.runner("UserPromptSubmit", kimiSession, turnPrompt)
	deadline, entered := make(chan time.Time), make(chan struct{})
	r.after = func(time.Duration) <-chan time.Time { return deadline }
	// The decision never goes on: a real hook process exits once the deadline has answered.
	r.arming = func() { close(entered); select {} }
	out := make(chan Output, 1)
	go func() { out <- r.Run() }()
	<-entered
	deadline <- time.Now()
	if got := <-out; got.Exit != 0 || len(got.Stdout) != 0 {
		t.Fatalf("past the deadline: %+v", got)
	}
	told, err := NewSessions(h.env("IRIS_TOKEN_FILE")).Told("kimi", kimiSession)
	if err != nil || told.Identity.Pending {
		t.Fatalf("the lost note's attention was recorded as told: %+v %v", told, err)
	}
}

// SessionStart publishes the session for its Kimi process only once the hub has accepted it, and
// /clear re-points the process to its new session, so a prompt of the old one no longer asks.
func TestKimiSessionStartPublishesTheSessionTheHubAccepted(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.hub = scriptHub{regErr: errors.New("down")}
	h.start(kimiSession)
	if got := h.mapped(); got != "" {
		t.Fatalf("a session the hub refused was published: %q", got)
	}
	h.hub = scriptHub{}
	quiet(t, h.prompt(kimiSession))
	h.start(kimiSession)
	if got := h.mapped(); got != kimiSession {
		t.Fatalf("published %q", got)
	}
	h.start(kimiCleared)
	if got := h.mapped(); got != kimiCleared {
		t.Fatalf("after /clear published %q", got)
	}
	quiet(t, h.prompt(kimiSession))
	asked(t, h.prompt(kimiCleared))
}

// Without wake explicitly on, a prompt asks nothing; the session is still published, so turning
// wake on needs no restart.
func TestKimiPromptAsksNothingUnlessWakeIsOn(t *testing.T) {
	h := newKimiHarness(t)
	h.start(kimiSession)
	if got := h.mapped(); got != kimiSession {
		t.Fatalf("published %q", got)
	}
	quiet(t, h.prompt(kimiSession))
	h.wake("off")
	quiet(t, h.prompt(kimiSession))
	h.wake("on")
	asked(t, h.prompt(kimiSession))
}

// Only a single-session Kimi launcher with a known start time names a process; anything else
// publishes nothing and asks nothing. An id that cannot name a record does neither either.
func TestKimiWakeNeedsTheKimiProcessAndAUsableSessionID(t *testing.T) {
	for name, change := range map[string]func(h *kimiHarness){
		"another process": func(h *kimiHarness) { h.anc.Name = "/usr/bin/node" },
		"no start time":   func(h *kimiHarness) { h.anc.StartMS = 0 },
		"no pid":          func(h *kimiHarness) { h.anc.PID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			h := newKimiHarness(t)
			h.wake("on")
			change(h)
			h.start(kimiSession)
			quiet(t, h.prompt(kimiSession))
			if got := h.mapped(); got != "" {
				t.Fatalf("published %q", got)
			}
		})
	}
	h := newKimiHarness(t)
	h.wake("on")
	h.start("session/../x")
	if got := h.mapped(); got != "" {
		t.Fatalf("published %q", got)
	}
	quiet(t, h.prompt("session/../x"))
}

// A pinned directory that cannot be found, or state that cannot be written, never breaks a
// session: SessionStart still allows it with its context, and a prompt asks nothing (D3).
func TestKimiWakeFailuresLetTheSessionRun(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.pinErr = errors.New("no pinned directory")
	if out := h.start(kimiSession); out.Exit != 0 || len(out.Stdout) == 0 {
		t.Fatalf("SessionStart: %+v", out)
	}
	quiet(t, h.prompt(kimiSession))
	h.pinErr = nil
	if got := h.mapped(); got != "" {
		t.Fatalf("published %q", got)
	}
	h.start(kimiSession)
	// The records directory cannot be made, so a prompt cannot record a request and must not ask.
	watchers := filepath.Join(h.root(), "watchers")
	if err := os.Remove(watchers); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchers, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	quiet(t, h.prompt(kimiSession))
	if out := h.start(kimiCleared); out.Exit != 0 || len(out.Stdout) == 0 {
		t.Fatalf("SessionStart with unwritable state: %+v", out)
	}
	if got := h.mapped(); got != kimiSession {
		t.Fatalf("published %q", got)
	}
}

// A Runner without a pinned directory of its own uses the OS account's, which TestMain points at a
// temporary directory.
func TestKimiWakeDefaultsToThePinnedDirectory(t *testing.T) {
	pinned, err := config.PinnedDir()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := Runner{}.kimiWake()
	if !ok || s.Root != kimiwake.Root(pinned) {
		t.Fatalf("%+v %v, want %s", s, ok, kimiwake.Root(pinned))
	}
}
