package wake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const hubAdapterToken = "wake-adapter-synthetic-token"

type hubAdapterConfig struct {
	url, token string
	tokenErr   error
}

func (c hubAdapterConfig) HubURL() string         { return c.url }
func (c hubAdapterConfig) Token() (string, error) { return c.token, c.tokenErr }

func hubAdapterTarget() binding.WakeTarget {
	return binding.WakeTarget{SessionID: "session", Host: "synthetic-host", Bound: binding.Bound{ExecutionID: "execution", Generation: 7}}
}

func hubAdapterSource(t *testing.T, handler http.Handler) hubSource {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return hubSource{client: &hubclient.Client{Config: hubAdapterConfig{url: server.URL, token: hubAdapterToken}}}
}

func assertHubAdapterAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer "+hubAdapterToken {
		t.Errorf("the request omitted its synthetic bearer credential")
	}
	if strings.Contains(r.URL.String(), hubAdapterToken) {
		t.Error("the bearer credential appeared in the request URL")
	}
}

func TestHubSourceHookSeenRequiresTheExactPositiveGeneration(t *testing.T) {
	for name, tc := range map[string]struct {
		response string
		seen     bool
	}{
		"hook registered":  {`{"execution_id":"execution","generation":7,"hook_seen":true}`, true},
		"no hook":          {`{"execution_id":"execution","generation":7,"hook_seen":false}`, false},
		"older hub":        {`{"execution_id":"execution","generation":7}`, false},
		"other execution":  {`{"execution_id":"other","generation":7,"hook_seen":true}`, false},
		"other generation": {`{"execution_id":"execution","generation":8,"hook_seen":true}`, false},
		"missing binding":  {`{"hook_seen":true}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertHubAdapterAuth(t, r)
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := map[string]string{"execution": "execution", "client": "codex", "host": "synthetic-host"}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/bind" || !reflect.DeepEqual(body, want) {
					t.Errorf("binding request = %s %s, %#v", r.Method, r.URL.Path, body)
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			seen, err := source.HookSeen(t.Context(), hubAdapterTarget())
			if err != nil || seen != tc.seen {
				t.Fatalf("hook evidence = %v, %v; want %v", seen, err, tc.seen)
			}
		})
	}
}

func TestHubSourceAttentionReadsOnlyGenerationQualifiedAnonymousEvents(t *testing.T) {
	target := hubAdapterTarget()
	target.Bound.ExecutionID = "execution /?&"
	const stream = "stream /?&"
	source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHubAdapterAuth(t, r)
		want := url.Values{"execution_id": {target.Bound.ExecutionID}, "generation": {"7"}, "after": {"19"}, "stream": {stream}, "limit": {"1"}, "attention": {"1"}}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/events" || !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("attention request = %s %s, %#v", r.Method, r.URL.Path, r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"stream":"current","high_water":23,"events":[{"cursor":20,"kind":"changed"}],"attention":{"unread":2,"available":1,"monitored":false,"high_water":23,"identity":{"stream":"current","cursor":22}}}`))
	}))
	got, err := source.Attention(t.Context(), target, 19, stream)
	if err != nil || got.Stream != "current" || got.HighWater != 23 || got.Attention.Unread != 2 || got.Attention.Available != 1 || got.Attention.HighWater != 23 || got.Attention.Identity != (hubclient.Identity{Known: true, Pending: true, Stream: "current", Cursor: 22}) {
		t.Fatalf("authoritative attention read = %+v, %v", got, err)
	}
}

func TestHubSourceAttentionPreservesUnknownAndSettledIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		response string
		known    bool
	}{
		"old hub":    {`{"stream":"stream","high_water":2,"attention":{"unread":1}}`, false},
		"no pending": {`{"stream":"stream","high_water":2,"attention":{"identity":null}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.response)) }))
			got, err := source.Attention(t.Context(), hubAdapterTarget(), 0, "")
			if err != nil || got.Attention.Identity.Known != tc.known || got.Attention.Identity.Pending {
				t.Fatalf("identity compatibility = %+v, %v", got, err)
			}
		})
	}
}

func TestHubSourceJSONReadsPreserveFailuresWithoutPositiveEvidence(t *testing.T) {
	for name, response := range map[string]string{
		"typed unauthorized": `{"error":{"code":"unauthorized","message":"synthetic rejection"}}`,
		"malformed":          `{"attention":`,
	} {
		t.Run(name, func(t *testing.T) {
			source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if name == "typed unauthorized" {
					w.WriteHeader(http.StatusUnauthorized)
				}
				_, _ = w.Write([]byte(response))
			}))
			seen, bindErr := source.HookSeen(t.Context(), hubAdapterTarget())
			attention, readErr := source.Attention(t.Context(), hubAdapterTarget(), 0, "")
			if seen || bindErr == nil || readErr == nil || attention != (attentionRead{}) {
				t.Fatalf("failed read returned positive state: %v, %v, %+v, %v", seen, bindErr, attention, readErr)
			}
			if name == "typed unauthorized" && (!hubclient.IsCode(bindErr, "unauthorized") || !hubclient.IsCode(readErr, "unauthorized")) {
				t.Fatalf("typed errors were lost: %v, %v", bindErr, readErr)
			}
		})
	}
}

func TestHubSourceConnectAuthenticatesAnAnonymousBoundedStream(t *testing.T) {
	target := hubAdapterTarget()
	target.Bound.ExecutionID = "execution /?&"
	source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHubAdapterAuth(t, r)
		want := url.Values{"execution_id": {target.Bound.ExecutionID}, "generation": {"7"}, "after": {"19"}, "stream": {"stream /?&"}}
		if r.URL.Path != "/v1/watch" || !reflect.DeepEqual(r.URL.Query(), want) || r.Header.Get("Sec-WebSocket-Protocol") != "iris.v1" {
			t.Errorf("stream request = %s, %#v, protocol %q", r.URL.Path, r.URL.Query(), r.Header.Get("Sec-WebSocket-Protocol"))
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"stream":"stream","cursor":20,"kind":"inbox_changed","body":"untrusted ignored content"}`)); err != nil {
			t.Error(err)
		}
		<-conn.CloseRead(r.Context()).Done()
	}))
	conn, err := source.Connect(t.Context(), target, 19, "stream /?&")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	frame, err := conn.Read(t.Context())
	if err != nil || frame != (eventFrame{Stream: "stream", Cursor: 20, Kind: "inbox_changed"}) {
		t.Fatalf("content-free frame = %+v, %v", frame, err)
	}
	conn.Close()
	if frame, err := conn.Read(t.Context()); err == nil || frame != (eventFrame{}) {
		t.Fatalf("closed connection returned a frame: %+v, %v", frame, err)
	}
}

func TestHubSourceConnectUsesTLSBasePathAndCanOmitAnEmptyToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/v1/watch" || r.Header.Get("Authorization") != "" {
			t.Errorf("TLS base-path or empty-token request = %s, authorization present=%v", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		<-conn.CloseRead(r.Context()).Done()
	}))
	defer server.Close()
	source := hubSource{client: &hubclient.Client{Config: hubAdapterConfig{url: server.URL + "/proxy/"}, HTTP: server.Client()}}
	conn, err := source.Connect(t.Context(), hubAdapterTarget(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestHubSourceConnectRefusesConfigurationAndHandshakeFailures(t *testing.T) {
	tokenErr := errors.New("synthetic token read failure")
	for name, source := range map[string]hubSource{
		"nil client":       {},
		"nil config":       {client: &hubclient.Client{}},
		"cleartext remote": {client: &hubclient.Client{Config: hubAdapterConfig{url: "http://example.invalid", token: hubAdapterToken}}},
		"token failure":    {client: &hubclient.Client{Config: hubAdapterConfig{url: "http://127.0.0.1:1", tokenErr: tokenErr}}},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := source.Connect(t.Context(), hubAdapterTarget(), 0, "")
			if err == nil || conn != nil {
				t.Fatalf("invalid configuration opened a connection: %v", err)
			}
			if name == "token failure" && !errors.Is(err, tokenErr) {
				t.Fatalf("the configuration error was lost: %v", err)
			}
		})
	}
	for name, status := range map[string]int{"unauthorized": 401, "upstream unavailable": 503, "invalid upgrade": 101} {
		t.Run(name, func(t *testing.T) {
			source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("synthetic raw response must not enter diagnostics"))
			}))
			conn, err := source.Connect(t.Context(), hubAdapterTarget(), 0, "")
			if err == nil || conn != nil || strings.Contains(err.Error(), "synthetic raw response") {
				t.Fatalf("failed handshake leaked output or returned a connection: %v", err)
			}
			if status == 401 && !hubclient.IsCode(err, "unauthorized") {
				t.Fatalf("the unauthorized response lost its type: %v", err)
			}
			if status == 503 {
				var typed *hubclient.StatusError
				if !errors.As(err, &typed) || typed.Path != "/v1/watch" || typed.Code != status {
					t.Fatalf("the upstream status lost its type: %v", err)
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	source := hubAdapterSource(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a canceled dial reached the server") }))
	if conn, err := source.Connect(ctx, hubAdapterTarget(), 0, ""); !errors.Is(err, context.Canceled) || conn != nil {
		t.Fatalf("canceled dial = %v", err)
	}
}

func TestHubSourceNeverFollowsRedirectsOrLeaksAuthentication(t *testing.T) {
	var destination atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destination.Add(1) }))
	defer sink.Close()
	source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHubAdapterAuth(t, r)
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	_, bindErr := source.HookSeen(t.Context(), hubAdapterTarget())
	_, readErr := source.Attention(t.Context(), hubAdapterTarget(), 0, "")
	_, dialErr := source.Connect(t.Context(), hubAdapterTarget(), 0, "")
	if !errors.Is(bindErr, hubclient.ErrRedirect) || !errors.Is(readErr, hubclient.ErrRedirect) || !errors.Is(dialErr, hubclient.ErrRedirect) || destination.Load() != 0 {
		t.Fatalf("redirect refusal = %v, %v, %v; destination requests %d", bindErr, readErr, dialErr, destination.Load())
	}
}

func TestHubSourceReadRejectsMalformedAndOversizedFrames(t *testing.T) {
	for name, body := range map[string]string{
		"malformed": `{"stream":"stream","cursor":`,
		"oversized": `{"stream":"stream","cursor":1,"kind":"inbox_changed","body":"` + strings.Repeat("x", 8<<10) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			source := hubAdapterSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(body))
				<-conn.CloseRead(r.Context()).Done()
			}))
			conn, err := source.Connect(t.Context(), hubAdapterTarget(), 0, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(conn.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if frame, err := conn.Read(ctx); err == nil || frame != (eventFrame{}) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unusable frame escaped its bound or parser: %+v, %v", frame, err)
			}
		})
	}
}
