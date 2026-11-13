package wake

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

// The wake runner across a restore (ADR 006): a copy made by `iris admin backup` runs on a new
// stream, and a copy without the restore marker keeps the stream.

// restoredWake is one hub address whose store a test replaces with a restored copy, as an
// operator restores a backup while a session's wake state outlives it.
type restoredWake struct {
	t        *testing.T
	livePath string
	cwd      string
	server   *httptest.Server
	current  atomic.Pointer[hub.Hub]
	client   *hubclient.Client
}

func newRestoredWake(t *testing.T) (*restoredWake, *store.Store) {
	t.Helper()
	w := &restoredWake{t: t, livePath: filepath.Join(t.TempDir(), "live.db"), cwd: t.TempDir()}
	live, err := store.Open(w.livePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The restore closes it; this closes it on an earlier failure too, so that Windows can remove
	// the directory.
	t.Cleanup(func() { _ = live.Close() })
	w.server = httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.current.Load().ServeHTTP(rw, r)
	}))
	w.serve(live)
	w.server.Start()
	t.Cleanup(w.server.Close)
	w.client = &hubclient.Client{Config: integrationHubConfig{url: w.server.URL}, HTTP: w.server.Client()}
	return w, live
}

func (w *restoredWake) serve(s *store.Store) {
	h := hub.New(s, integrationHubToken, []string{w.server.Listener.Addr().String()})
	h.Log = log.New(io.Discard, "", 0)
	w.current.Store(h)
}

// register attaches a Codex session as its hooks do, presenting generation; 0 starts one.
func (w *restoredWake) register(session string, generation int) binding.WakeTarget {
	w.t.Helper()
	registered, err := w.client.RegisterSession(w.t.Context(), hubclient.SessionRequest{
		Client: "codex", SessionID: session, Host: "wake-integration-host", CWD: w.cwd, Generation: generation,
		RemoteURL: "https://example.com/wake-integration.git", RootCommit: "root", Source: "hook",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return binding.WakeTarget{SessionID: session, Host: "wake-integration-host", Bound: binding.Bound{
		ExecutionID: registered.ExecutionID, Generation: registered.Generation, RepositoryID: registered.RepositoryID, Agent: registered.Agent, Rung: 2,
	}}
}

// reattach presents the session's generation to the restored hub, as its hooks would.
func (w *restoredWake) reattach(target binding.WakeTarget) {
	w.t.Helper()
	if again := w.register(target.SessionID, target.Bound.Generation); again.Bound.ExecutionID != target.Bound.ExecutionID || again.Bound.Generation != target.Bound.Generation {
		w.t.Fatalf("the restored hub did not give %s its Execution back: %+v", target.SessionID, again.Bound)
	}
}

func (w *restoredWake) send(s *store.Store, from, to binding.WakeTarget, key string) {
	w.t.Helper()
	if _, err := s.Call(w.t.Context(), "messages", store.Request{ExecutionID: from.Bound.ExecutionID, Generation: int64(from.Bound.Generation), Key: key, To: []string{to.Bound.Agent.Name}, Body: "synthetic body"}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *restoredWake) high(s *store.Store, via binding.WakeTarget) int64 {
	w.t.Helper()
	out, err := s.Call(w.t.Context(), "events", store.Request{ExecutionID: via.Bound.ExecutionID})
	if err != nil {
		w.t.Fatal(err)
	}
	return out["high_water"].(int64)
}

// restore stops the live hub and serves a copy of the backup at saved in its place. Without
// marked, the copy loses its restore marker, as a copy made some other way than `iris admin
// backup` lacks it.
func (w *restoredWake) restore(live *store.Store, saved string, marked bool) *store.Store {
	w.t.Helper()
	_ = live.Close()
	path := filepath.Join(w.t.TempDir(), "restored.db")
	raw, err := os.ReadFile(saved)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		w.t.Fatal(err)
	}
	if !marked {
		db, _ := sql.Open("sqlite", path)
		_, err := db.Exec("DELETE FROM RestoreMarker")
		_ = db.Close()
		if err != nil {
			w.t.Fatal(err)
		}
	}
	restored, err := store.Open(path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = restored.Close() })
	if restored.Restored() != marked {
		w.t.Fatalf("restored %v, marked %v", restored.Restored(), marked)
	}
	w.serve(restored)
	return restored
}

// countedSource is the real hub source, counting the attention reads an engine makes.
type countedSource struct {
	hubSource
	reads *int
}

func (s countedSource) Attention(ctx context.Context, target binding.WakeTarget, after int64, stream string) (attentionRead, error) {
	*s.reads++
	return s.hubSource.Attention(ctx, target, after, stream)
}

// engineOn is a wake engine for target that reads the hub through the real source and queues
// through the recording Codex executable, which accepts every prompt.
func (w *restoredWake) engineOn(binary string, target binding.WakeTarget) (*engine, *engineResolver, *int) {
	w.t.Helper()
	c, _, _ := commandSetup(w.t, binary, commandScenario{VersionOut: "codex-cli " + commandVersion + "\n", Out: commandAccept + "\n"})
	now := time.Now()
	c.now = func() time.Time { return now }
	reads := new(int)
	r := &engineResolver{target: target, eligible: true}
	e := &engine{target: target, resolver: r, source: countedSource{hubSource{client: w.client}, reads},
		store:    newStateStore(hook.NewSessions(filepath.Join(w.t.TempDir(), "token")), target.SessionID),
		commands: engineIdle(c), clientVersion: func() string { return commandVersion }, now: c.now}
	return e, r, reads
}

// wakes steps the engine and requires the wake it queues to be the starts-th, and returns it.
func wakes(t *testing.T, e *engine, r *engineResolver, starts int) attempt {
	t.Helper()
	if got := e.step(t.Context()); got != e.waitFile() || r.starts != starts {
		t.Fatalf("no wake %d: %+v, starts %d", starts, got, r.starts)
	}
	d := mustSnapshot(t, e.store).Document
	a := d.Attempts[len(d.Attempts)-1]
	if a.Answer != "submitted" || a.ConsumedMS != 0 {
		t.Fatalf("wake %d is not an outstanding submitted prompt: %+v", starts, a)
	}
	return a
}

// consume marks the outstanding wake consumed, as the woken turn's prompt hook does first,
// before it asks the hub anything.
func consume(t *testing.T, e *engine) {
	t.Helper()
	d := mustSnapshot(t, e.store).Document
	for i := range d.Attempts {
		if d.Attempts[i].outstanding() {
			d.Attempts[i].ConsumedMS = e.now().UnixMilli()
		}
	}
	writeState(t, e.store, d)
}

// restoreBelow restores the hub under a session woken for attention at woken, sends the session
// mail whose attention is at or below that cursor, and moves the restored hub's high-water mark
// to it, so that neither the wake mark nor a told cursor is above the hub's history. It returns
// the restored store and the sender.
func (w *restoredWake) restoreBelow(live *store.Store, saved string, marked bool, woken attempt, target, sender, other binding.WakeTarget) *store.Store {
	w.t.Helper()
	restored := w.restore(live, saved, marked)
	for _, s := range []binding.WakeTarget{target, sender, other} {
		w.reattach(s)
	}
	w.send(restored, sender, target, "m2")
	if at := w.high(restored, sender); at > woken.Cursor {
		w.t.Fatalf("m2's attention, at most %d, must be at or below the woken cursor %d", at, woken.Cursor)
	}
	for i := 0; w.high(restored, sender) < woken.Cursor; i++ {
		w.send(restored, sender, other, fmt.Sprint("v", i))
	}
	return restored
}

// newerMailWakes is the positive control after a restore: mail newer than every wake wakes the
// session, on the restored hub's stream.
func (w *restoredWake) newerMailWakes(e *engine, r *engineResolver, restored *store.Store, sender, target binding.WakeTarget, woken attempt, marked bool) {
	w.t.Helper()
	w.send(restored, sender, target, "m3")
	newest := wakes(w.t, e, r, r.starts+1)
	if newest.Cursor <= woken.Cursor || (newest.Stream == woken.Stream) == marked {
		w.t.Fatalf("newer mail woke for %s %d after the wake for %s %d (marked %v)", newest.Stream, newest.Cursor, woken.Stream, woken.Cursor, marked)
	}
}

// liveWake starts the sessions, takes the backup, and has the engine wake the target for mail
// at a cursor far above the backup's high-water mark.
func (w *restoredWake) liveWake(live *store.Store, binary string) (saved string, e *engine, r *engineResolver, reads *int, woken attempt, target, sender, other binding.WakeTarget) {
	w.t.Helper()
	target, sender, other = w.register(commandSession, 0), w.register("sender", 0), w.register("other", 0)
	saved = filepath.Join(w.t.TempDir(), "backup.db")
	if err := backup.Run(w.t.Context(), w.livePath, saved, io.Discard); err != nil {
		w.t.Fatal(err)
	}
	for i := range 20 {
		w.send(live, sender, other, fmt.Sprint("u", i))
	}
	w.send(live, sender, target, "m1")
	e, r, reads = w.engineOn(binary, target)
	woken = wakes(w.t, e, r, 1)
	return saved, e, r, reads, woken, target, sender, other
}

// A session was woken for mail at cursor R on the live hub, and the woken turn ran and was told
// it. The hub is then restored from a backup taken before that, and the restored hub's
// high-water mark reaches R, so the runner's mark is not above the hub's history and cannot
// show the restore. Mail sent after the restore has attention at or below R. On a backup's
// marked copy the hub runs on a new stream, whose identity is new, with its own allowance, and
// the runner wakes the session for that mail. On an unmarked copy the stream is the same and the
// runner stays silent: the gap the restore marker closes, which an unmarked copy still has.
// Newer mail wakes the session either way.
func TestARestoredHubWakesForAttentionBelowAWokenCursor(t *testing.T) {
	binary := commandBinary(t)
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "unmarked"}[marked], func(t *testing.T) {
			w, live := newRestoredWake(t)
			saved, e, r, _, woken, target, sender, other := w.liveWake(live, binary)
			// The woken turn runs: its prompt hook consumes the wake and tells what is pending.
			consume(t, e)
			engineTold(t, e.store, woken.Stream, woken.Cursor, e.now(), false)

			restored := w.restoreBelow(live, saved, marked, woken, target, sender, other)
			if marked {
				again := wakes(t, e, r, 2)
				if again.Stream == woken.Stream || again.Cursor > woken.Cursor {
					t.Fatalf("the wake after the restore was for %s %d, after %s %d", again.Stream, again.Cursor, woken.Stream, woken.Cursor)
				}
				consume(t, e)
			} else {
				before := mustSnapshot(t, e.store)
				if got := e.step(t.Context()); got != (stepResult{}) || r.starts != 1 || !sameState(before, mustSnapshot(t, e.store)) {
					t.Fatalf("an unmarked copy woke for attention below the woken cursor: %+v, starts %d", got, r.starts)
				}
			}
			w.newerMailWakes(e, r, restored, sender, target, woken, marked)
		})
	}
}

// A wake queued on the live hub has not run when the hub is restored. Its reservation is kept,
// on either copy, and the runner neither reads nor wakes until that prompt consumes it, because
// the prompt may still run and a second wake would double it. Once it is consumed, the runner
// reads again: on a marked copy the new stream's identity wakes the session, and on an unmarked
// copy attention at or below the consumed wake's cursor does not.
func TestARestoredHubKeepsAnOutstandingWakeUntilItIsConsumed(t *testing.T) {
	binary := commandBinary(t)
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "unmarked"}[marked], func(t *testing.T) {
			w, live := newRestoredWake(t)
			saved, e, r, reads, woken, target, sender, other := w.liveWake(live, binary)

			restored := w.restoreBelow(live, saved, marked, woken, target, sender, other)
			before, read := mustSnapshot(t, e.store), *reads
			for range 2 {
				if got := e.step(t.Context()); got != e.waitFile() || r.starts != 1 || *reads != read || !sameState(before, mustSnapshot(t, e.store)) {
					t.Fatalf("the restore cleared or passed the outstanding wake: %+v, starts %d, reads %d", got, r.starts, *reads-read)
				}
			}
			// Its prompt runs at last. The hook consumes the wake before it asks the hub anything,
			// and here it learns nothing more, as when the hub does not answer it.
			consume(t, e)
			if marked {
				if again := wakes(t, e, r, 2); again.Stream == woken.Stream {
					t.Fatalf("the wake after the restore kept the old stream %s", again.Stream)
				}
				consume(t, e)
			} else if got := e.step(t.Context()); got != (stepResult{}) || r.starts != 1 {
				t.Fatalf("an unmarked copy woke for attention below the consumed wake: %+v, starts %d", got, r.starts)
			}
			w.newerMailWakes(e, r, restored, sender, target, woken, marked)
		})
	}
}
