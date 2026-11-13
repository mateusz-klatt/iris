package watch

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/backup"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

// restoringHub serves one address whose store a test can replace with a restored copy, as an
// operator does by restoring a backup while clients keep their state. While paused it answers
// 503, so a monitor keeps what it remembers and retries.
type restoringHub struct {
	server  *httptest.Server
	current atomic.Pointer[hub.Hub]
	paused  atomic.Bool
	mu      sync.Mutex
	streams []net.Conn
}

func newRestoringHub(t *testing.T, s *store.Store) *restoringHub {
	t.Helper()
	r := &restoringHub{}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.paused.Load() {
			http.Error(w, "restoring", http.StatusServiceUnavailable)
			return
		}
		r.current.Load().ServeHTTP(w, req)
	}))
	// A monitor's socket is hijacked from the server, so closing the store does not end it:
	// the test ends it, as a hub that stops for a restore does.
	r.server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			r.mu.Lock()
			r.streams = append(r.streams, c)
			r.mu.Unlock()
		}
	}
	r.serve(s)
	r.server.Start()
	t.Cleanup(r.server.Close)
	return r
}

func (r *restoringHub) serve(s *store.Store) {
	h := hub.New(s, "token", []string{r.server.Listener.Addr().String()})
	h.PingInterval = 20 * time.Millisecond
	r.current.Store(h)
}

// stop pauses the address and ends every monitor socket.
func (r *restoringHub) stop() {
	r.paused.Store(true)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.streams {
		_ = c.Close()
	}
	r.streams = nil
}

func within(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
	}
}

func cursorOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return -1
}

// A monitor whose cursor is ahead of attention committed after a restore. It reconnects only
// once the restored hub's high-water mark has passed that cursor, so the cursor rule of ADR 006
// decision 7 cannot tell the hub went back. Two messages wait for it then: one whose attention
// is at or below its cursor, and one above. On a backup's marked copy the hub runs on a new
// stream, so the monitor is reset and announces both. On an unmarked copy it keeps the stream
// and announces only the one above its cursor: the gap the restore marker closes, which an
// unmarked copy still has.
func TestARestoredHubAnnouncesMailBelowAMonitorsCursor(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "unmarked"}[marked], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			livePath := filepath.Join(dir, "live.db")
			live, err := store.Open(livePath, nil)
			if err != nil {
				t.Fatal(err)
			}
			// The restore closes it; this closes it on an earlier failure too, so that Windows can
			// remove the directory.
			t.Cleanup(func() { _ = live.Close() })
			rh := newRestoringHub(t, live)
			h := realHub{live, rh.server.URL}
			sender, senderGen := h.session(t, "sender")
			reader, readerGen := h.session(t, "reader")
			other, otherGen := h.session(t, "other")
			name, otherName, senderName := h.agentName(t, reader), h.agentName(t, other), h.agentName(t, sender)
			saved := filepath.Join(dir, "backup.db")
			if err := backup.Run(ctx, livePath, saved, io.Discard); err != nil {
				t.Fatal(err)
			}
			// After the backup: unrelated mail first, so the monitor's cursor ends far beyond the
			// backup's high-water mark, then mail for the reader, which the monitor announces.
			for i := range 10 {
				h.call(t, "messages", store.Request{ExecutionID: sender, Generation: senderGen, Key: fmt.Sprint("u", i), To: []string{otherName}, Body: "x"})
			}
			out := &lines{}
			var remembered atomic.Int64
			r := &Runner{
				Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: rh.server.URL}}, Client: "claude", Host: "host", Getenv: func(key string) string {
					if key == "IRIS_EXECUTION" {
						return reader
					}
					return ""
				}},
				Output: out,
				// A short window, so the line after the restore is not held behind the one before.
				Window:       time.Millisecond,
				Backoff:      5 * time.Millisecond,
				checkpointed: func(cursor int64) { remembered.Store(cursor) },
			}
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(runCtx) }()
			t.Cleanup(func() { stop(); <-done })
			h.call(t, "messages", store.Request{ExecutionID: sender, Generation: senderGen, Key: "m1", To: []string{name}, Body: "before"})
			high := cursorOf(h.call(t, "events", store.Request{ExecutionID: reader})["high_water"])
			within(t, "the live announcement and its save", func() bool {
				return strings.Contains(out.String(), "1 new message") && remembered.Load() >= high
			})
			R := remembered.Load()

			// The restore: the hub stops, the backup's copy goes in its place, sessions reattach.
			rh.stop()
			_ = live.Close()
			restoredPath := filepath.Join(dir, "restored.db")
			raw, err := os.ReadFile(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(restoredPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if !marked {
				unmark(t, restoredPath)
			}
			restored, err := store.Open(restoredPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Close() })
			if restored.Restored() != marked {
				t.Fatalf("restored %v, marked %v", restored.Restored(), marked)
			}
			rh.serve(restored)
			hb := realHub{restored, rh.server.URL}
			for _, s := range []struct {
				id  string
				gen int64
			}{{"sender", senderGen}, {"reader", readerGen}, {"other", otherGen}} {
				hb.call(t, "sessions", store.Request{Client: "claude", Host: "host", SessionID: s.id, Cwd: "/repo", Generation: s.gen})
			}
			// m2's attention lands at or below the monitor's cursor. Unrelated mail then moves the
			// high-water mark past that cursor, so m3's attention lands above it.
			attention := func() int64 {
				var at int64
				for _, e := range hb.call(t, "events", store.Request{ExecutionID: reader})["events"].([]map[string]any) {
					if e["kind"] == "inbox_changed" {
						at = cursorOf(e["cursor"])
					}
				}
				return at
			}
			hb.call(t, "messages", store.Request{ExecutionID: sender, Generation: senderGen, Key: "m2", To: []string{name}, Body: "after"})
			A := attention()
			for i := 0; cursorOf(hb.call(t, "events", store.Request{ExecutionID: reader})["high_water"]) <= R; i++ {
				hb.call(t, "messages", store.Request{ExecutionID: other, Generation: otherGen, Key: fmt.Sprint("v", i), To: []string{senderName}, Body: "y"})
			}
			hb.call(t, "messages", store.Request{ExecutionID: sender, Generation: senderGen, Key: "m3", To: []string{name}, Body: "later"})
			B := attention()
			if A <= 0 || A > R || R >= B {
				t.Fatalf("the case needs 0 < A ≤ R < B: A %d, R %d, B %d", A, R, B)
			}
			before := len(out.String())
			rh.paused.Store(false)
			// The monitor saves a cursor at or past B only once every line up to it is out.
			within(t, "the monitor's save past m3", func() bool { return remembered.Load() >= B })
			want := map[bool]string{true: "Iris: 2 new messages. Use the inbox tool.\n", false: "Iris: 1 new message. Use the inbox tool.\n"}[marked]
			if said := out.String()[before:]; said != want {
				t.Fatalf("after the restore the monitor said %q, want %q", said, want)
			}
		})
	}
}

// unmark deletes the restore marker from the copy at path, which no hub has open, as a copy made
// some other way than `iris admin backup` would lack it.
func unmark(t *testing.T, path string) {
	t.Helper()
	db, _ := sql.Open("sqlite", path)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("DELETE FROM RestoreMarker"); err != nil {
		t.Fatal(err)
	}
}
