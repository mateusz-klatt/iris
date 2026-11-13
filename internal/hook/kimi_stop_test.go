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

func (h *kimiHarness) run(event, session string) Output {
	h.t.Helper()
	body := `{"hook_event_name":"` + event + `","session_id":"` + session + `","cwd":` + quote(h.repo) + `,"client_type":"kimi_code_cli"}`
	return Runner{
		ClientName: "kimi", EventName: event, Stdin: strings.NewReader(body), Hub: h.hub, Getenv: h.env,
		Ancestor:        func() (proc.Ancestor, error) { return h.anc, nil },
		Find:            func(string) (gitinfo.Repo, error) { return gitinfo.Find(h.repo) },
		PinnedConfigDir: func() (string, error) { return h.pinned, h.pinErr },
		Now:             func() time.Time { return h.now },
		after:           never, logAfter: never,
	}.Run()
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

func (h *kimiHarness) mapped() string {
	id, _, err := (&kimiwake.Store{Root: h.root()}).Session(kimiWakePID, kimiWakeStart, time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// asked asserts the exact continuation of ADR 030: stderr and exit 2, which Kimi treats as a block.
func asked(t *testing.T, out Output) {
	t.Helper()
	if out.Exit != 2 || string(out.Stderr) != kimiWatchReason+"\n" || len(out.Stdout) != 0 {
		t.Fatalf("Stop did not ask for the watcher: exit %d stdout %q stderr %q", out.Exit, out.Stdout, out.Stderr)
	}
}

// quiet asserts an ordinary end of turn: nothing blocks and nothing asks.
func quiet(t *testing.T, out Output) {
	t.Helper()
	if out.Exit != 0 || strings.Contains(string(out.Stderr), "iris watch") || len(out.Stdout) != 0 {
		t.Fatalf("Stop asked or blocked: exit %d stdout %q stderr %q", out.Exit, out.Stdout, out.Stderr)
	}
}

// The continuation is ADR 030's text, word for word: the exact command, description and options,
// and that Iris cannot check the command the model runs.
func TestKimiStopAsksInTheWordsOfADR030(t *testing.T) {
	adr, err := os.ReadFile(filepath.Join("..", "..", "docs", "adr", "030-kimi-one-shot-wake.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(adr), "```text\n"+kimiWatchReason+"\n```") {
		t.Fatal("the Stop continuation differs from the text ADR 030 fixes")
	}
}

// With wake on, the first Stop after the hub accepted the session asks for the watcher. Inside the
// grace a second Stop asks nothing more; after it, the unanswered request latches and no Stop asks,
// until a SessionStart of the session re-arms it.
func TestKimiStopAsksOnceUntilSessionStartRearms(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	if out := h.run("SessionStart", kimiSession); out.Exit != 0 || len(out.Stdout) == 0 {
		t.Fatalf("SessionStart: %+v", out)
	}
	asked(t, h.run("Stop", kimiSession))
	h.now = h.now.Add(kimiwake.Grace - time.Millisecond)
	quiet(t, h.run("Stop", kimiSession))
	h.now = h.now.Add(time.Millisecond)
	quiet(t, h.run("Stop", kimiSession))
	h.now = h.now.Add(time.Hour)
	quiet(t, h.run("Stop", kimiSession))
	h.run("SessionStart", kimiSession)
	asked(t, h.run("Stop", kimiSession))
}

// SessionStart publishes the session for its Kimi process only once the hub has accepted it, and
// /clear re-points the process to its new session, so a Stop of the old one no longer asks.
func TestKimiSessionStartPublishesTheSessionTheHubAccepted(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.hub = scriptHub{regErr: errors.New("down")}
	h.run("SessionStart", kimiSession)
	if got := h.mapped(); got != "" {
		t.Fatalf("a session the hub refused was published: %q", got)
	}
	quiet(t, h.run("Stop", kimiSession))
	h.hub = scriptHub{}
	h.run("SessionStart", kimiSession)
	if got := h.mapped(); got != kimiSession {
		t.Fatalf("published %q", got)
	}
	h.run("SessionStart", kimiCleared)
	if got := h.mapped(); got != kimiCleared {
		t.Fatalf("after /clear published %q", got)
	}
	quiet(t, h.run("Stop", kimiSession))
	asked(t, h.run("Stop", kimiCleared))
}

// Without wake explicitly on, Stop asks nothing; the session is still published, so turning wake
// on needs no restart.
func TestKimiStopAsksNothingUnlessWakeIsOn(t *testing.T) {
	h := newKimiHarness(t)
	h.run("SessionStart", kimiSession)
	if got := h.mapped(); got != kimiSession {
		t.Fatalf("published %q", got)
	}
	quiet(t, h.run("Stop", kimiSession))
	h.wake("off")
	quiet(t, h.run("Stop", kimiSession))
	h.wake("on")
	asked(t, h.run("Stop", kimiSession))
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
			h.run("SessionStart", kimiSession)
			quiet(t, h.run("Stop", kimiSession))
			if got := h.mapped(); got != "" {
				t.Fatalf("published %q", got)
			}
		})
	}
	h := newKimiHarness(t)
	h.wake("on")
	h.run("SessionStart", "session/../x")
	if got := h.mapped(); got != "" {
		t.Fatalf("published %q", got)
	}
	quiet(t, h.run("Stop", "session/../x"))
}

// A pinned directory that cannot be found, or state that cannot be written, never breaks a
// session: SessionStart still allows it with its context, and Stop lets the turn end (D3).
func TestKimiWakeFailuresLetTheSessionRun(t *testing.T) {
	h := newKimiHarness(t)
	h.wake("on")
	h.pinErr = errors.New("no pinned directory")
	if out := h.run("SessionStart", kimiSession); out.Exit != 0 || len(out.Stdout) == 0 {
		t.Fatalf("SessionStart: %+v", out)
	}
	quiet(t, h.run("Stop", kimiSession))
	h.pinErr = nil
	if got := h.mapped(); got != "" {
		t.Fatalf("published %q", got)
	}
	h.run("SessionStart", kimiSession)
	// The records directory cannot be made, so Stop cannot record a request and must not ask.
	watchers := filepath.Join(h.root(), "watchers")
	if err := os.Remove(watchers); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchers, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	quiet(t, h.run("Stop", kimiSession))
	if out := h.run("SessionStart", kimiCleared); out.Exit != 0 || len(out.Stdout) == 0 {
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
