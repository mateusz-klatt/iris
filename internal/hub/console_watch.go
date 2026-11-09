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
	wakes, release := h.subscribeStreams()
	defer release()
	// One observer owns admission and revocation, including while the network
	// writer is stalled. The latest scoped revision is enough for a UI refresh;
	// it is enqueued only after its admission transaction has committed.
	frames := make(chan map[string]any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		previous := int64(-1)
		for {
			changes := wakes.Changes()
			var admitted map[string]any
			// The callback copies data only. A failed COMMIT must not queue it.
			err := h.admitConsoleFrame(ctx, p, func(frame map[string]any) error { admitted = frame; return nil })
			if err != nil {
				_ = conn.CloseNow()
				cancel()
				return
			}
			cursor := admitted["cursor"].(int64)
			if cursor != previous {
				select {
				case frames <- admitted:
				default:
					// Coalesce queued UI revisions, never Execution event pages.
					select {
					case <-frames:
					default:
					}
					// This observer is the only producer. Either it removed the
					// old frame or the writer already did, so this cannot block.
					frames <- admitted
				}
				previous = cursor
			}
			select {
			case <-ctx.Done():
				return
			case <-changes:
			}
		}
	}()
	defer func() { cancel(); <-done }()
	pings := time.NewTicker(h.pingInterval())
	defer pings.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case admitted := <-frames:
			raw, _ := json.Marshal(admitted)
			if h.writeSocket(ctx, conn, raw) != nil {
				return
			}
		case <-pings.C:
			if !h.ping(ctx, conn) {
				return
			}
		}
	}
}
