package hub

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestConsoleStreamIOFailuresAndRevocationDuringUpgradeReleaseResources(t *testing.T) {
	for _, failure := range []string{"write", "ping", "revoked-during-upgrade", "store-unavailable-after-upgrade"} {
		t.Run(failure, func(t *testing.T) {
			h, server := setup(t)
			h.console = newConsoleAuth(make([]byte, 32))
			_, cookie, _ := routeConsoleAccount(t, h, "Operator", "operator")
			h.PingInterval = time.Millisecond
			switch failure {
			case "write":
				h.writeSocket = func(context.Context, *websocket.Conn, []byte) error { return io.ErrClosedPipe }
			case "ping":
				h.pingSocket = func(context.Context, *websocket.Conn) error { return io.ErrClosedPipe }
			default:
				h.acceptSocket = func(w http.ResponseWriter, r *http.Request, options *websocket.AcceptOptions) (*websocket.Conn, error) {
					conn, err := websocket.Accept(w, r, options)
					if err != nil {
						return conn, err
					}
					if failure == "revoked-during-upgrade" {
						err = h.store.DeleteConsoleSession(context.Background(), h.console.mac("session", cookie.Value))
					} else {
						err = h.store.Close()
					}
					if err != nil {
						t.Error("could not arrange post-upgrade storage state", err)
					}
					return conn, nil
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ui/watch", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {server.URL}, "Cookie": {cookie.String()}}})
			if err != nil {
				t.Fatal("console upgrade failed before stream failure", err)
			}
			defer func() { _ = conn.CloseNow() }()
			assertStreamClosedAndReleased(t, h, conn)
		})
	}
}

func TestConsoleRevocationClosesSocketDuringStalledWrite(t *testing.T) {
	h, server := setup(t)
	h.console = newConsoleAuth(make([]byte, 32))
	_, cookie, _ := routeConsoleAccount(t, h, "Operator", "operator")
	writing := make(chan struct{})
	h.writeSocket = func(ctx context.Context, _ *websocket.Conn, _ []byte) error {
		close(writing)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ui/watch", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {server.URL}, "Cookie": {cookie.String()}}})
	if err != nil {
		t.Fatal("console upgrade failed", err)
	}
	defer func() { _ = conn.CloseNow() }()
	select {
	case <-writing:
	case <-ctx.Done():
		t.Fatal("stream did not reach network write")
	}
	if err = h.store.DeleteConsoleSession(ctx, h.console.mac("session", cookie.Value)); err != nil {
		t.Fatal("stalled write held the store lock", err)
	}
	assertStreamClosedAndReleased(t, h, conn)
}
