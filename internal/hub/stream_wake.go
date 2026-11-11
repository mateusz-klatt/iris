package hub

import (
	"context"
	"sync"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

const streamCoalesce = 10 * time.Millisecond

// streamWakes is shared by the hub's current streams. It carries hints, never
// state: observers still read a committed store snapshot before sending a frame.
type streamWakes struct {
	mu      sync.Mutex
	changed chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	refs    int // protected by Hub.streamMu
}

func (w *streamWakes) Changes() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.changed
}

func (w *streamWakes) signal() {
	w.mu.Lock()
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()
}

func (h *Hub) subscribeStreams() (*streamWakes, func()) {
	h.streamMu.Lock()
	if h.streams == nil {
		ctx, cancel := context.WithCancel(context.Background())
		w := &streamWakes{changed: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
		// Capture before launching the worker or reading a stream's first snapshot.
		changes := h.store.Changes()
		h.streams = w
		go h.runStreamWakes(ctx, w, changes)
	}
	w := h.streams
	w.refs++
	h.streamMu.Unlock()
	return w, func() {
		h.streamMu.Lock()
		defer h.streamMu.Unlock()
		w.refs--
		if w.refs == 0 {
			w.cancel()
			// The worker never takes streamMu: joining here prevents a new first
			// subscriber from starting a second sweep before the old one exits.
			<-w.done
			h.streams = nil
		}
	}
}

// runStreamWakes connects two levels of change channel. Each notifying commit
// closes and replaces the channel from Store.Changes. Only this worker waits
// on it and takes its replacement at once, so commits during coalescing are
// not lost. signal closes and replaces streamWakes.changed once per window
// and on maintenance failure. Streams take streamWakes.Changes before reading,
// so a signal between their read and wait is not lost (ADR 016, decision 4).
func (h *Hub) runStreamWakes(ctx context.Context, w *streamWakes, changes <-chan struct{}) {
	defer close(w.done)
	ticks := time.NewTicker(time.Second)
	defer ticks.Stop()
	var pending *time.Timer
	var ready <-chan time.Time
	defer func() {
		if pending != nil {
			pending.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			changes = h.store.Changes()
			if ready == nil {
				// Bound the delay from the first change. Repeated commits never
				// reset this timer and therefore cannot starve observers.
				pending = time.NewTimer(streamCoalesce)
				ready = pending.C
			}
		case <-ready:
			ready = nil
			w.signal()
		case <-ticks.C:
			if _, err := h.call(ctx, "expire", store.Request{}); err != nil && ctx.Err() == nil {
				h.logger().Printf("Iris stream maintenance failed: %v", err)
				// Store failure is itself observable: let streams fail closed
				// rather than leave an idle connection waiting indefinitely.
				w.signal()
			}
		}
	}
}

func (h *Hub) ticketChanges() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ticketChanged
}

func (h *Hub) ticketPresent(key string) bool {
	if key == "" {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, exists := h.tickets[key]
	return exists
}
