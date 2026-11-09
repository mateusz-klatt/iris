package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

func (h *Hub) consoleWatch(w http.ResponseWriter, r *http.Request, p store.Principal, accountID string) {
	quota := principal{digest: "console:" + accountID}
	if !h.reserveSocket(w, quota) {
		return
	}
	defer h.releaseSocket(quota)
	conn, err := h.acceptSocket(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithCancel(conn.CloseRead(r.Context()))
	defer cancel()
	// A separate revocation observer closes even a socket whose network write is
	// stalled. Store changes wake it for logout/access/password changes without a
	// repository event; the ticker also enforces absolute session expiry.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			changes := h.store.Changes()
			if _, err := h.store.ConsoleSessionAccount(ctx, p.SessionDigest); err != nil {
				_ = conn.CloseNow()
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-changes:
			case <-ticker.C:
			}
		}
	}()
	defer func() { cancel(); <-done }()
	ticks := time.NewTicker(time.Second)
	defer ticks.Stop()
	pings := time.NewTicker(h.pingInterval())
	defer pings.Stop()
	previous := int64(-1)
	for {
		changes := h.store.Changes()
		var admitted map[string]any
		// Admission is serialized with revocation. The callback does no network I/O;
		// only after its transaction commits may an admitted frame enter the socket.
		err = h.store.AdmitConsoleFrame(ctx, p, func(frame map[string]any) error { admitted = frame; return nil })
		if err != nil {
			return
		}
		cursor := admitted["cursor"].(int64)
		if cursor != previous {
			raw, _ := json.Marshal(admitted)
			if h.writeSocket(ctx, conn, raw) != nil {
				return
			}
			previous = cursor
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-ticks.C:
		case <-pings.C:
			if !h.ping(ctx, conn) {
				return
			}
		}
	}
}
