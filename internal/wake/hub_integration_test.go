package wake

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

const integrationHubToken = "wake-integration-synthetic-token"

type integrationHubConfig struct{ url string }

func (c integrationHubConfig) HubURL() string       { return c.url }
func (integrationHubConfig) Token() (string, error) { return integrationHubToken, nil }

type integrationHubRequest struct {
	path          string
	query         url.Values
	authenticated bool
}

type integrationHub struct {
	server   *httptest.Server
	store    *store.Store
	readDB   *sql.DB
	client   *hubclient.Client
	source   hubSource
	mu       sync.Mutex
	requests []integrationHubRequest
}

func newIntegrationHub(t *testing.T) *integrationHub {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "hub.db")
	db, err := store.Open(filename, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	readDB, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := readDB.Close(); err != nil {
			t.Error(err)
		}
	})
	f := &integrationHub{store: db, readDB: readDB}
	server := httptest.NewUnstartedServer(nil)
	h := hub.New(db, integrationHubToken, []string{server.Listener.Addr().String()})
	h.Log = log.New(io.Discard, "", 0)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, integrationHubRequest{path: r.URL.Path, query: r.URL.Query(), authenticated: r.Header.Get("Authorization") == "Bearer "+integrationHubToken})
		f.mu.Unlock()
		h.ServeHTTP(w, r)
	})
	server.Start()
	t.Cleanup(server.Close)
	f.server = server
	f.client = &hubclient.Client{Config: integrationHubConfig{url: server.URL}, HTTP: server.Client()}
	f.source = hubSource{client: f.client}
	return f
}

func (f *integrationHub) register(t *testing.T, session, client, source string) binding.WakeTarget {
	t.Helper()
	registered, err := f.client.RegisterSession(t.Context(), hubclient.SessionRequest{
		Client: client, SessionID: session, Host: "wake-integration-host", CWD: t.TempDir(),
		RemoteURL: "https://example.com/wake-integration.git", RootCommit: "root", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return binding.WakeTarget{SessionID: session, Host: "wake-integration-host", Bound: binding.Bound{
		ExecutionID: registered.ExecutionID, Generation: registered.Generation, RepositoryID: registered.RepositoryID, Agent: registered.Agent, Rung: 2,
	}}
}

func (f *integrationHub) call(t *testing.T, operation string, r store.Request) map[string]any {
	t.Helper()
	response, err := f.store.Call(t.Context(), operation, r)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (f *integrationHub) send(t *testing.T, from, to binding.WakeTarget, key string) {
	t.Helper()
	f.call(t, "messages", store.Request{
		ExecutionID: from.Bound.ExecutionID, Generation: int64(from.Bound.Generation), Key: key,
		To: []string{to.Bound.Agent.Name}, Subject: "synthetic subject", Body: "synthetic body, never copied into a wake",
	})
}

type integrationCursorState struct {
	Count   int
	Monitor int64
}

func (f *integrationHub) cursors(t *testing.T, target binding.WakeTarget) integrationCursorState {
	t.Helper()
	var snapshot integrationCursorState
	if err := f.readDB.QueryRowContext(t.Context(), "SELECT COUNT(*),COALESCE(MAX(CASE WHEN consumer_id=? THEN seq END),-1) FROM Cursor", target.Bound.ExecutionID+":iris-watch").Scan(&snapshot.Count, &snapshot.Monitor); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *integrationHub) assertAnonymousRequests(t *testing.T, target binding.WakeTarget, after int64, stream string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	events, watches := 0, 0
	for _, r := range f.requests {
		if r.path != "/v1/events" && r.path != "/v1/watch" {
			continue
		}
		if r.query.Has("consumer_id") {
			if r.path == "/v1/events" {
				t.Fatal("wake events read retained a consumer cursor")
			}
			continue
		} // independent named monitor probes below
		initialRead := r.path == "/v1/events" && events == 0 && r.query.Get("after") == "0" && r.query.Get("stream") == ""
		if !r.authenticated || r.query.Get("execution_id") != target.Bound.ExecutionID || r.query.Get("generation") != strconv.Itoa(target.Bound.Generation) ||
			(!initialRead && (r.query.Get("after") != strconv.FormatInt(after, 10) || r.query.Get("stream") != stream)) {
			t.Fatalf("wake request did not retain its exact target/cursor: %+v", r)
		}
		if r.path == "/v1/events" {
			events++
			if r.query.Get("attention") != "1" {
				t.Fatal("authoritative read omitted attention")
			}
		} else {
			watches++
		}
	}
	if events == 0 || watches == 0 {
		t.Fatalf("missing anonymous events/watch requests: %d/%d", events, watches)
	}
}

func TestHubSourceRealHubHookEvidenceMatchesExactTarget(t *testing.T) {
	f := newIntegrationHub(t)
	target := f.register(t, "008521c3-8831-4be9-8895-0ce68ac753c0", "codex", "hook")
	if seen, err := f.source.HookSeen(t.Context(), target); err != nil || !seen {
		t.Fatalf("registered hook was not proved: %t %v", seen, err)
	}
	stale := target
	stale.Bound.Generation++
	if seen, err := f.source.HookSeen(t.Context(), stale); err != nil || seen {
		t.Fatalf("hook proof crossed generation: %t %v", seen, err)
	}
	wrongHost := target
	wrongHost.Host = "another-host"
	if seen, err := f.source.HookSeen(t.Context(), wrongHost); err == nil || seen {
		t.Fatalf("hook proof crossed host: %t %v", seen, err)
	}
	missing := target
	missing.Bound.ExecutionID = "ex_missing"
	if seen, err := f.source.HookSeen(t.Context(), missing); err == nil || seen {
		t.Fatalf("missing execution had hook proof: %t %v", seen, err)
	}
	otherClient := f.register(t, "another-client", "claude", "hook")
	if seen, err := f.source.HookSeen(t.Context(), otherClient); err == nil || seen {
		t.Fatalf("hook proof crossed client: %t %v", seen, err)
	}
	shimOnly := f.register(t, "shim-only", "codex", "mcp")
	if seen, err := f.source.HookSeen(t.Context(), shimOnly); err != nil || seen {
		t.Fatalf("MCP registration became hook proof: %t %v", seen, err)
	}
	if err := f.client.EndExecution(t.Context(), target.Bound.ExecutionID, target.Bound.Generation); err != nil {
		t.Fatal(err)
	}
	if seen, err := f.source.HookSeen(t.Context(), target); err == nil || seen {
		t.Fatalf("ended target retained hook proof: %t %v", seen, err)
	}
}

func TestHubSourceRealHubObservesAttentionWithoutMonitorOrCursorWrites(t *testing.T) {
	f := newIntegrationHub(t)
	target := f.register(t, "008521c3-8831-4be9-8895-0ce68ac753c0", "codex", "hook")
	sender := f.register(t, "sender", "codex", "hook")
	initial, err := f.source.Attention(t.Context(), target, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !initial.Attention.Identity.Known || initial.Attention.Identity.Pending || initial.Attention.Monitored || initial.Stream == "" {
		t.Fatalf("empty hub attention is not known-null: %+v", initial)
	}
	if cursor := f.cursors(t, target); cursor.Count != 0 {
		t.Fatalf("first wake sample created a consumer cursor: %+v", cursor)
	}
	// Give the real monitor an existing retention cursor. A wake must neither create its own
	// consumer nor move this one, even after receiving and resampling a newer event.
	f.call(t, "events", store.Request{ExecutionID: target.Bound.ExecutionID, Generation: int64(target.Bound.Generation), After: initial.HighWater, ConsumerID: "iris-watch"})
	before := f.cursors(t, target)
	conn, err := f.source.Connect(t.Context(), target, initial.HighWater, initial.Stream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	query := url.Values{"execution_id": {target.Bound.ExecutionID}, "generation": {strconv.Itoa(target.Bound.Generation)}, "consumer_id": {"iris-watch"}, "after": {strconv.FormatInt(initial.HighWater, 10)}, "stream": {initial.Stream}}
	endpoint := "ws" + strings.TrimPrefix(f.server.URL, "http") + "/v1/watch?" + query.Encode()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	monitor, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: f.server.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + integrationHubToken}}, Subprotocols: []string{"iris.v1"}})
	if err != nil {
		t.Fatal("anonymous wake occupied the real monitor slot", err)
	}
	t.Cleanup(func() { _ = monitor.CloseNow() })
	duplicate, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: f.server.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + integrationHubToken}}, Subprotocols: []string{"iris.v1"}})
	if duplicate != nil {
		_ = duplicate.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("named monitor probe did not test exclusivity: %v %v", response, err)
	}
	f.send(t, sender, target, "wake-event")
	frame, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != "inbox_changed" || frame.Stream != initial.Stream || frame.Cursor <= initial.HighWater {
		t.Fatalf("stream did not carry the new attention event: %+v", frame)
	}
	read, err := f.source.Attention(t.Context(), target, initial.HighWater, initial.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Attention.Identity.Pending || read.Attention.Identity.Stream != frame.Stream || read.Attention.Identity.Cursor != frame.Cursor || read.Attention.Unread != 1 || read.Attention.Monitored {
		t.Fatalf("wake fetched mail or claimed monitored delivery: %+v", read)
	}
	if read.HighWater < frame.Cursor || read.Attention.HighWater != read.HighWater {
		t.Fatalf("authoritative high-water did not match event: %+v", read)
	}
	if after := f.cursors(t, target); after != before {
		t.Fatalf("wake changed consumer retention: before %+v after %+v", before, after)
	}
	f.assertAnonymousRequests(t, target, initial.HighWater, initial.Stream)
}

func TestHubSourceRealHubRejectsStaleGenerationAndClosesCanceledReads(t *testing.T) {
	f := newIntegrationHub(t)
	target := f.register(t, "008521c3-8831-4be9-8895-0ce68ac753c0", "codex", "hook")
	initial, err := f.source.Attention(t.Context(), target, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	stale := target
	stale.Bound.Generation++
	if _, err := f.source.Attention(t.Context(), stale, initial.HighWater, initial.Stream); !binding.IsCode(err, "lost") {
		t.Fatalf("stale authoritative read was accepted: %v", err)
	}
	if conn, err := f.source.Connect(t.Context(), stale, initial.HighWater, initial.Stream); err == nil {
		conn.Close()
		t.Fatal("stale stream was accepted")
	} else {
		var status *hubclient.StatusError
		if !errors.As(err, &status) || status.Code != http.StatusConflict {
			t.Fatalf("stale stream error lost its HTTP refusal: %v", err)
		}
	}
	for _, closeFirst := range []bool{false, true} {
		conn, err := f.source.Connect(t.Context(), target, initial.HighWater, initial.Stream)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		if closeFirst {
			conn.Close()
		} else {
			cancel()
		}
		if _, err := conn.Read(ctx); err == nil {
			t.Fatal("closed or canceled stream remained readable")
		} else if !closeFirst && !errors.Is(err, context.Canceled) {
			t.Fatalf("read did not preserve cancellation: %v", err)
		}
		cancel()
		conn.Close()
	}
}
