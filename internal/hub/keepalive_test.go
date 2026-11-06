package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestWatchKeepalive(t *testing.T) {
	for _, path := range []string{"/v1/watch", "/v1/ui/watch"} {
		t.Run(path, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			s := httptest.NewUnstartedServer(nil)
			h := New(db, "test-secret", []string{s.Listener.Addr().String()})
			h.PingInterval = 20 * time.Millisecond
			s.Config.Handler = h
			// HTTP deadlines must not survive the upgrade and close an idle socket.
			s.Config.ReadTimeout = 60 * time.Millisecond
			s.Config.IdleTimeout = 60 * time.Millisecond
			s.Start()
			defer s.Close()
			a, b := register(t, s, 1), register(t, s, 2)
			endpoint := "ws" + strings.TrimPrefix(s.URL, "http") + path
			if path == "/v1/watch" {
				endpoint = watchURL(s, b["execution_id"], 0)
			}
			var pings atomic.Int64
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}, OnPingReceived: func(context.Context, []byte) bool { pings.Add(1); return true }})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			frames := make(chan map[string]any, 10)
			failures := make(chan error, 1)
			go func() {
				for {
					_, raw, err := conn.Read(ctx)
					if err != nil {
						failures <- err
						return
					}
					var f map[string]any
					if err = json.Unmarshal(raw, &f); err != nil {
						failures <- err
						return
					}
					frames <- f
				}
			}()
			if path == "/v1/ui/watch" {
				select {
				case <-frames:
				case err := <-failures:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			// Remain idle for much longer than the HTTP timeout and several ping cycles.
			time.Sleep(180 * time.Millisecond)
			if pings.Load() < 2 {
				t.Fatalf("only %d pings", pings.Load())
			}
			select {
			case f := <-frames:
				t.Fatal("control frame surfaced as event", f)
			case err := <-failures:
				t.Fatal(err)
			default:
			}
			send(t, s, a, b, "after-idle")
			select {
			case f := <-frames:
				if f["kind"] == nil {
					t.Fatal(f)
				}
			case err := <-failures:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
