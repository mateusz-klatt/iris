package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

type testConfig struct{ url string }

func (c testConfig) HubURL() string         { return c.url }
func (c testConfig) Token() (string, error) { return "token", nil }

type lines struct {
	mu   sync.Mutex
	text string
}

func (b *lines) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text += string(p)
	return len(p), nil
}
func (b *lines) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text }
func TestBurstReconnectNoDuplicateLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	register := func(id string) map[string]any {
		out, err := s.Call(ctx, "sessions", store.Request{Client: "claude", Host: "host", SessionID: id, Cwd: "/repo"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := register("a"), register("b")
	var sockets atomic.Int32
	cut := make(chan struct{})
	resume := make(chan struct{})
	server := httptest.NewUnstartedServer(nil)
	real := hub.New(s, "token", []string{server.Listener.Addr().String()})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/watch" {
			real.ServeHTTP(w, r)
			return
		}
		n := sockets.Add(1)
		if n > 1 {
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		// Relay a real hub socket, dropping the first transport after its first frame.
		upstreamURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/relay" + "?" + r.URL.RawQuery
		upstream, _, err := websocket.Dial(r.Context(), upstreamURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer token"}}, Subprotocols: []string{"iris.v1"}})
		if err != nil {
			http.Error(w, "retry", 503)
			return
		}
		defer upstream.CloseNow()
		downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"iris.v1"}})
		if err != nil {
			return
		}
		defer downstream.CloseNow()
		relayCtx := downstream.CloseRead(r.Context())
		for {
			_, raw, err := upstream.Read(relayCtx)
			if err != nil {
				return
			}
			if downstream.Write(r.Context(), websocket.MessageText, raw) != nil {
				return
			}
			if n == 1 {
				close(cut)
				return
			}
		}
	})
	// /relay enters the hub watch route without recursively traversing the proxy.
	proxyHandler := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/relay" {
			r.URL.Path = "/v1/watch"
			real.ServeHTTP(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})
	server.Start()
	defer server.Close()
	resolver := &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{server.URL}}, Client: "claude", Host: "host", CWD: "/repo", Getenv: func(k string) string {
		if k == "IRIS_EXECUTION" {
			return b["execution_id"].(string)
		}
		return ""
	}, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil }}
	output := &lines{}
	done := make(chan error, 1)
	go func() {
		done <- (&Runner{Resolver: resolver, Output: output, Window: 2 * time.Second, Backoff: 10 * time.Millisecond}).Run(ctx)
	}()
	send := func(key string) {
		_, err := s.Call(ctx, "messages", store.Request{ExecutionID: a["execution_id"].(string), Generation: 1, Key: key, To: []string{b["agent"].(map[string]any)["name"].(string)}, Body: "DO NOT OUTPUT THIS"})
		if err != nil {
			t.Fatal(err)
		}
	}
	send("first")
	select {
	case <-cut:
	case <-ctx.Done():
		t.Fatal("no first frame")
	}
	for i := 0; i < 3; i++ {
		send(fmt.Sprint(i))
	}
	close(resume)
	want := "Iris: 1 new message. Use the inbox tool.\nIris: 3 new messages. Use the inbox tool.\n"
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if output.String() == want {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(output.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(250 * time.Millisecond)
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if output.String() != want {
		t.Fatalf("lines: %q", output.String())
	}
	inbox, err := s.Call(context.Background(), "inbox", store.Request{ExecutionID: b["execution_id"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(inbox)
	if len(inbox["deliveries"].([]map[string]any)) != 4 {
		t.Fatal(string(raw))
	}
}
