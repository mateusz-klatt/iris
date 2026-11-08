package hub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

func assertStreamClosedAndReleased(t *testing.T, h *Hub, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("stream did not close")
			}
			break
		}
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		h.mu.Lock()
		slots := len(h.socketCounts)
		monitors := len(h.monitors)
		h.mu.Unlock()
		if slots == 0 && monitors == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("stream leaked slots", slots, monitors)
		case <-tick.C:
		}
	}
}
func TestMonitorCursorFailureReleasesMonitor(t *testing.T) {
	h, s := setup(t)
	session := register(t, s, 1)
	original := h.call
	var fail atomic.Bool
	fail.Store(true)
	h.call = func(ctx context.Context, op string, r store.Request) (map[string]any, error) {
		if op == "monitor_cursor" && fail.Swap(false) {
			return nil, io.ErrUnexpectedEOF
		}
		return original(ctx, op, r)
	}
	path := "/v1/watch?execution_id=" + session["execution_id"].(string) + "&consumer_id=iris-watch"
	first, _ := http.NewRequest("GET", s.URL+path, nil)
	first.Header.Set("Authorization", "Bearer test-secret")
	first.Header.Set("Sec-WebSocket-Protocol", "iris.v1")
	response, err := s.Client().Do(first)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 400 || fail.Load() {
		t.Fatal("cursor failure was not exercised", response.StatusCode)
	}
	h.mu.Lock()
	n := len(h.monitors)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("cursor failure retained monitor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+path, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}})
	if err != nil {
		raw, _ := io.ReadAll(response.Body)
		t.Fatal("cursor retry failed", err, string(raw))
	}
	defer func() { _ = conn.CloseNow() }()
}
func TestStreamIOFailuresReleaseResources(t *testing.T) {
	for _, kind := range []string{"event-write", "waiting-write", "reset-write", "ui-write", "ui-query", "watch-ping", "ui-ping"} {
		t.Run(kind, func(t *testing.T) {
			h, s := setup(t)
			a := register(t, s, 1)
			b := register(t, s, 2)
			h.PingInterval = time.Millisecond
			if strings.Contains(kind, "write") {
				h.writeSocket = func(context.Context, *websocket.Conn, []byte) error { return io.ErrClosedPipe }
			}
			if strings.Contains(kind, "ping") {
				h.pingSocket = func(context.Context, *websocket.Conn) error { return io.ErrClosedPipe }
			}
			if kind == "ui-query" {
				original := h.call
				h.call = func(ctx context.Context, op string, r store.Request) (map[string]any, error) {
					if op == "ui_events" {
						return nil, io.ErrUnexpectedEOF
					}
					return original(ctx, op, r)
				}
			}
			endpoint := watchURL(s, b["execution_id"], 0) + "&consumer_id=iris-watch"
			if strings.HasPrefix(kind, "ui-") {
				endpoint = "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/ui/watch"
			}
			if kind == "event-write" {
				send(t, s, a, b, "pending")
				endpoint = watchURL(s, b["execution_id"], 0)
			}
			if kind == "waiting-write" {
				send(t, s, a, b, "pending")
			}
			if kind == "reset-write" {
				endpoint += "&stream=old"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.CloseNow() }()
			assertStreamClosedAndReleased(t, h, conn)
		})
	}
}
func TestRevokedDuringAcceptReleasesStream(t *testing.T) {
	for _, ui := range []bool{false, true} {
		t.Run(map[bool]string{false: "watch", true: "ui"}[ui], func(t *testing.T) {
			h, s := setup(t)
			session := register(t, s, 1)
			token, err := h.store.AddToken(context.Background(), "accept-race")
			if err != nil {
				t.Fatal(err)
			}
			h.acceptSocket = func(w http.ResponseWriter, r *http.Request, opts *websocket.AcceptOptions) (*websocket.Conn, error) {
				c, err := websocket.Accept(w, r, opts)
				if err != nil {
					return c, err
				}
				if _, err = h.store.RevokeToken(r.Context(), "accept-race"); err != nil {
					t.Error(err)
				}
				return c, nil
			}
			endpoint := watchURL(s, session["execution_id"], 0)
			if ui {
				endpoint = "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/ui/watch"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{"iris.v1"}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.CloseNow() }()
			assertStreamClosedAndReleased(t, h, conn)
		})
	}
}
func TestUIHandshakeFailureDoesNotReserveSlot(t *testing.T) {
	h, s := setup(t)
	r := httptest.NewRequest("GET", s.URL+"/v1/ui/watch", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 426 {
		t.Fatal(w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.socketCounts) != 0 {
		t.Fatal("handshake failure retained slot")
	}
}
func TestWakeUpgradeHonorsIssuerQuota(t *testing.T) {
	h, s := setup(t)
	session := register(t, s, 1)
	ticket := request(t, s, "POST", "/v1/wake-tickets", authority(session), 200)
	h.Limits.TokenPerMinute = 1
	raw := ticket["protocols"].([]any)
	protocols := []string{raw[0].(string), raw[1].(string)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, ticket["url"].(string), &websocket.DialOptions{Subprotocols: protocols})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("wake upgrade bypassed token quota")
	}
	if resp == nil || resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatal(resp, err)
	}
}

// Gap review B2: a monitor's first connection can fail on the very frame that tells it about
// waiting mail. Its cursor is not saved, so the next connection, with nothing else happening
// on the hub, must be told again.
func TestAFailedWaitingFrameIsSentAgainOnReconnect(t *testing.T) {
	h, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	send(t, s, a, b, "waiting")
	var failed atomic.Bool
	real := h.writeSocket
	h.writeSocket = func(ctx context.Context, conn *websocket.Conn, raw []byte) error {
		if failed.CompareAndSwap(false, true) {
			return io.ErrClosedPipe
		}
		return real(ctx, conn, raw)
	}
	path := "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/watch?execution_id=" + b["execution_id"].(string) + "&consumer_id=iris-watch"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-secret"}}, Subprotocols: []string{"iris.v1"}}
	first, _, err := websocket.Dial(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Read(ctx); err == nil {
		t.Fatal("the failed frame arrived")
	}
	_ = first.CloseNow()
	// The hub may still be releasing the first monitor, which refuses a second as busy.
	var second *websocket.Conn
	for second == nil {
		if second, _, err = websocket.Dial(ctx, path, options); err != nil {
			select {
			case <-ctx.Done():
				t.Fatal(err)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	defer func() { _ = second.CloseNow() }()
	_, raw, err := second.Read(ctx)
	if err != nil || !strings.Contains(string(raw), `"kind":"reset"`) || !failed.Load() {
		t.Fatalf("the reconnected monitor got %s, %v", raw, err)
	}
}
