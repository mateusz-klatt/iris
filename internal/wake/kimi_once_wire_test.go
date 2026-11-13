package wake

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// pinnedKimiConfig is a pinned configuration as `iris setup` leaves it, in a directory of the test's
// own.
func pinnedKimiConfig(t *testing.T, hubURL string) (func() (hubclient.Resolved, string, error), string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte(`{"hub_url":"`+hubURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.token"), []byte("synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return hubclient.PinnedConfig{ConfigDir: func() (string, error) { return dir, nil }}.Load, dir
}

func lookupOnly(table map[int]int64) func(int) (proc.Snapshot, bool) {
	return func(pid int) (proc.Snapshot, bool) {
		start, ok := table[pid]
		return proc.Snapshot{PID: pid, StartMS: start}, ok
	}
}

// A watcher is wired only from a pinned configuration with a valid hub, and only when this process's
// own start time is known, which its claim on the record needs. Its state and the told state come
// from that pinned directory, and the process check matches a pid and start time together.
func TestKimiOnceIsWiredFromThePinnedConfiguration(t *testing.T) {
	load, dir := pinnedKimiConfig(t, "http://127.0.0.1:9")
	getpid := func() int { return 77 }
	nearest := func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }
	failed := func() (hubclient.Resolved, string, error) { return hubclient.Resolved{}, "", errors.New("no config") }
	badURL, _ := pinnedKimiConfig(t, "ftp://example.invalid")
	for name, c := range map[string]struct {
		load   func() (hubclient.Resolved, string, error)
		lookup map[int]int64
	}{
		"no configuration": {failed, map[int]int64{77: 5}},
		"bad hub URL":      {badURL, map[int]int64{77: 5}},
		"no own process":   {load, nil},
		"no start time":    {load, map[int]int64{77: 0}},
	} {
		if _, ok := newKimiOnce(io.Discard, c.load, nearest, lookupOnly(c.lookup), getpid); ok {
			t.Fatalf("%s: wired", name)
		}
	}
	w, ok := newKimiOnce(io.Discard, load, nearest, lookupOnly(map[int]int64{77: 5, 4242: 1700}), getpid)
	if !ok || w.store.Root != kimiwake.Root(dir) || w.self.PID != 77 || w.self.StartMS != 5 || w.self.ID == "" || w.now == nil {
		t.Fatalf("%+v", w)
	}
	if _, ok := w.hub.(hubSource); !ok {
		t.Fatalf("hub %T", w.hub)
	}
	if !w.alive(4242, 1700) || w.alive(4242, 1701) || w.alive(4243, 1700) || w.alive(4242, 0) {
		t.Fatal("the process check did not match a pid and start time together")
	}
	if err := (&kimiwake.Store{Root: kimiwake.Root(dir)}).MapSession(4242, 1700, onceSession, time.Second); err != nil {
		t.Fatal(err)
	}
	if session, ok, err := w.mapped(4242, 1700); err != nil || !ok || session != onceSession {
		t.Fatalf("the session map is not read from the pinned state: %q %v %v", session, ok, err)
	}
	if told, err := w.told(onceSession); err != nil || told.Present {
		t.Fatalf("%+v %v", told, err)
	}
	sessions := hook.NewSessions(filepath.Join(dir, "hub.token"))
	if _, err := os.Stat(sessions.Dir()); err != nil {
		t.Fatalf("the told state is not read beside the pinned token: %v", err)
	}
	synctest.Test(t, func(t *testing.T) {
		unlock, err := sessions.Lock(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if _, err := w.told(onceSession); err == nil {
			t.Fatal("read the told state past a held lock")
		}
	})
}

// RunKimiOnce fails, saying so, without a pinned configuration, and otherwise runs a watcher. Under a
// test runner, which is not a Kimi process with a published session, that run fails too.
func TestRunKimiOnceRunsAWatcherFromThePinnedConfiguration(t *testing.T) {
	original := loadKimiOnce
	t.Cleanup(func() { loadKimiOnce = original })
	loadKimiOnce = func() (hubclient.Resolved, string, error) { return hubclient.Resolved{}, "", errors.New("no config") }
	out := &onceOut{}
	if got := RunKimiOnce(t.Context(), out); got != Failed || out.String() != outcomeLines[Failed]+"\n" {
		t.Fatalf("%v %q", got, out)
	}
	loadKimiOnce, _ = pinnedKimiConfig(t, "http://127.0.0.1:9")
	out = &onceOut{}
	if got := RunKimiOnce(t.Context(), out); got != Failed || out.String() != outcomeLines[Failed]+"\n" {
		t.Fatalf("%v %q", got, out)
	}
}

// The bind asks for the session and its Kimi process together, and accepts only the combined
// lookup's answer with hook evidence and a live generation. A hub from before that lookup answers
// without a match, which is refused.
func TestKimiBindAcceptsOnlyTheCombinedMatch(t *testing.T) {
	for body, want := range map[string]error{
		`{"execution_id":"ex","generation":3,"hook_seen":true,"match":"process_session"}`: nil,
		`{"execution_id":"ex","generation":3,"hook_seen":true}`:                           errUnmatched,
		`{"execution_id":"ex","generation":3,"match":"process_session"}`:                  errUnmatched,
		`{"execution_id":"ex","hook_seen":true,"match":"process_session"}`:                errUnmatched,
		`{"generation":3,"hook_seen":true,"match":"process_session"}`:                     errUnmatched,
	} {
		source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertHubAdapterAuth(t, r)
			var req map[string]any
			if r.Method != http.MethodPost || r.URL.Path != "/v1/bind" || json.NewDecoder(r.Body).Decode(&req) != nil ||
				req["client"] != "kimi" || req["host"] != "host" || req["cwd"] != "/repo" || req["session_id"] != onceSession ||
				req["client_pid"] != float64(onceKimiPID) || req["client_pid_start_ms"] != float64(onceKimiStart) {
				t.Errorf("bind request %s %s %v", r.Method, r.URL.Path, req)
			}
			_, _ = w.Write([]byte(body))
		}))
		b, err := source.kimiBind(t.Context(), "host", "/repo", onceSession, onceKimiPID, onceKimiStart)
		if !errors.Is(err, want) || (want == nil && (b.ExecutionID != "ex" || b.Generation != 3)) {
			t.Fatalf("%s: %+v %v", body, b, err)
		}
	}
	source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"unbound","message":"no session"}}`))
	}))
	if _, err := source.kimiBind(t.Context(), "host", "/repo", onceSession, onceKimiPID, onceKimiStart); !hubclient.IsCode(err, "unbound") {
		t.Fatal(err)
	}
}

// The heartbeat renews the pinned generation as a shim, and the checkpoint saves the iris-watch
// cursor, exactly as the persistent watch's does.
func TestKimiHeartbeatAndCheckpointRequests(t *testing.T) {
	var seen []string
	source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHubAdapterAuth(t, r)
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		_, _ = w.Write([]byte(`{}`))
	}))
	target := hubAdapterTarget()
	if err := source.heartbeat(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if err := source.checkpoint(t.Context(), target, 12); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /v1/executions/execution/heartbeat {"generation":7,"source":"shim"}`,
		`GET /v1/events?after=12&consumer_id=iris-watch&execution_id=execution&generation=7&limit=1 null`,
	}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("%q", seen)
	}
}
