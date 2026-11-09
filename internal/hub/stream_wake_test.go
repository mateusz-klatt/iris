package hub

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestStreamMaintenanceStopsWithItsLastSubscriberAndRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := idleMemoryHub(t)
		_, first := h.subscribeStreams()
		_, second := h.subscribeStreams()
		synctest.Wait()
		before := h.store.TransactionCount()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if delta := h.store.TransactionCount() - before; delta != 2 {
			t.Fatalf("two subscribers require one shared sweep: %d transactions", delta)
		}
		first()
		time.Sleep(time.Second)
		synctest.Wait()
		if delta := h.store.TransactionCount() - before; delta != 3 {
			t.Fatalf("one remaining subscriber lost maintenance: %d transactions", delta)
		}
		second()
		before = h.store.TransactionCount()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if h.store.TransactionCount() != before {
			t.Fatal("maintenance continued without a stream")
		}
		_, restarted := h.subscribeStreams()
		time.Sleep(time.Second)
		synctest.Wait()
		if delta := h.store.TransactionCount() - before; delta != 1 {
			t.Fatalf("restarted maintenance duplicated or missed its sweep: %d transactions", delta)
		}
		restarted()
	})
}

func TestMaintenanceFailureWakesStreamsToFailClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		var output bytes.Buffer
		h.Log = log.New(&output, "", 0)
		var unavailable atomic.Bool
		real := h.call
		h.call = func(ctx context.Context, op string, req store.Request) (map[string]any, error) {
			if unavailable.Load() && (op == "expire" || op == "events") {
				return nil, errors.New("synthetic store unavailable")
			}
			return real(ctx, op, req)
		}
		session := register(t, server, 1)
		socket := idleWatch(t, server, session["execution_id"], 0)
		synctest.Wait()
		unavailable.Store(true)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if _, closed, err := socket.snapshot(); !closed || websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("unavailable store retained a stream: closed %v error %v", closed, err)
		}
		if !strings.Contains(output.String(), "stream maintenance failed: synthetic store unavailable") {
			t.Fatal("maintenance failure was not reported", output.String())
		}
	})
}

func TestCancelingLastStreamDoesNotReportAStoreFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := idleMemoryHub(t)
		var output bytes.Buffer
		h.Log = log.New(&output, "", 0)
		entered := make(chan struct{})
		h.call = func(ctx context.Context, op string, _ store.Request) (map[string]any, error) {
			if op != "expire" {
				t.Errorf("unexpected maintenance operation %s", op)
			}
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_, release := h.subscribeStreams()
		<-entered
		release()
		if output.Len() != 0 {
			t.Fatal("normal stream shutdown was logged as a failure", output.String())
		}
	})
}
