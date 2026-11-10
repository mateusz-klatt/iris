package hub

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

func wakeOrderMessage(ctx context.Context, h *Hub, from, to map[string]any, key string) error {
	_, err := h.store.Call(ctx, "messages", store.Request{
		ExecutionID: from["execution_id"].(string), Generation: int64(from["generation"].(float64)),
		Key: key, To: []string{to["agent"].(map[string]any)["id"].(string)}, Body: "committed in the observation-to-wait gap",
	})
	return err
}

func TestWatchRetainsACommitBetweenItsObservationAndWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		from, to := register(t, server, 1), register(t, server, 2)
		before, err := h.store.Call(t.Context(), "events", store.Request{ExecutionID: to["execution_id"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		real := h.call
		var injected atomic.Bool
		finished := make(chan struct{})
		h.call = func(ctx context.Context, operation string, req store.Request) (map[string]any, error) {
			result, err := real(ctx, operation, req)
			if err == nil && operation == "events" && req.ExecutionID == to["execution_id"] && injected.CompareAndSwap(false, true) {
				defer close(finished)
				if err := wakeOrderMessage(ctx, h, from, to, "watch-read-gap"); err != nil {
					return nil, err
				}
				// Return the actual old snapshot only after its change hint fired.
				// A subscription captured after this return misses that hint.
				time.Sleep(2 * streamCoalesce)
			}
			return result, err
		}
		socket := idleWatch(t, server, to["execution_id"], before["high_water"].(int64))
		synctest.Wait()
		time.Sleep(3 * streamCoalesce)
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Fatal("observation did not return after the committed change")
		}
		frames, closed, err := socket.snapshot()
		if closed || len(frames) != 1 || len(frames[0]) != 3 || frames[0]["kind"] != "inbox_changed" || frames[0]["cursor"].(float64) <= float64(before["high_water"].(int64)) {
			t.Fatalf("commit in the observation-to-wait gap was lost: %v, closed %v, error %v", frames, closed, err)
		}
	})
}

func TestNamedUIRetainsACommitBetweenAdmissionAndWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		h.console = newConsoleAuth(make([]byte, 32))
		account, cookie, _ := routeConsoleAccount(t, h, "Admission gap operator", "operator")
		principal := store.Principal{Kind: store.ConsolePrincipal, SessionDigest: h.console.mac("session", cookie.Value)}
		real := h.admitConsoleFrame
		var injected atomic.Bool
		finished := make(chan struct{})
		h.admitConsoleFrame = func(ctx context.Context, p store.Principal, admit func(map[string]any) error) error {
			if err := real(ctx, p, admit); err != nil {
				return err
			}
			if injected.CompareAndSwap(false, true) {
				defer close(finished)
				if _, err := h.store.EditConsoleProfile(ctx, principal, account.EditRevision, "Changed after admission", "pl", "en"); err != nil {
					return err
				}
				time.Sleep(2 * streamCoalesce)
			}
			return nil
		}
		socket := idleNamedConsole(t, server, cookie)
		synctest.Wait()
		time.Sleep(3 * streamCoalesce)
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Fatal("admission did not return after the committed profile change")
		}
		latest, err := h.store.CallConsole(t.Context(), principal, "ui_events", store.Request{})
		if err != nil {
			t.Fatal(err)
		}
		frames, closed, err := socket.snapshot()
		// The writer may already own the initial revision, or the one-slot queue
		// may replace it. In either case the committed latest revision must arrive.
		if closed || len(frames) == 0 || len(frames) > 2 {
			t.Fatalf("named UI failed to observe the admission-to-wait gap: %v, closed %v, error %v", frames, closed, err)
		}
		for _, frame := range frames {
			if len(frame) != 2 || frame["kind"] != "changed" {
				t.Fatal("named UI frame exposed more than a scoped revision", frame)
			}
		}
		if frames[len(frames)-1]["cursor"] != float64(latest["cursor"].(int64)) {
			t.Fatalf("named UI lost the committed revision between admission and wait: %v, latest %v", frames, latest)
		}
	})
}

func TestSustainedCommitsCannotPostponeTheFirstStreamWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const firstWakeLimit = 10 * time.Millisecond
		h, server := idleMemoryHub(t)
		from, to := register(t, server, 1), register(t, server, 2)
		before, err := h.store.Call(t.Context(), "events", store.Request{ExecutionID: to["execution_id"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		socket := idleWatch(t, server, to["execution_id"], before["high_water"].(int64))
		synctest.Wait()
		started := time.Now()
		var firstWake time.Duration
		observed := false
		for i := range 10 {
			if err := wakeOrderMessage(t.Context(), h, from, to, fmt.Sprintf("sustained-%d", i)); err != nil {
				t.Fatal(err)
			}
			// Ensure the manager observes each real commit at its virtual time.
			synctest.Wait()
			time.Sleep(5 * time.Millisecond)
			synctest.Wait()
			frames, closed, err := socket.snapshot()
			if closed {
				t.Fatal("sustained commit stream closed", err)
			}
			if !observed && len(frames) > 0 {
				observed, firstWake = true, time.Since(started)
			}
		}
		if !observed || firstWake > firstWakeLimit {
			t.Fatalf("continued commits postponed the first wake: observed %v at %s, bound %s", observed, firstWake, firstWakeLimit)
		}
		frames, closed, err := socket.snapshot()
		if closed || len(frames) != 10 {
			t.Fatalf("sustained commits lost event frames: %d, closed %v, error %v", len(frames), closed, err)
		}
	})
}
