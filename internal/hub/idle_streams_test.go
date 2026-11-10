package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/store"
)

// The Go test server's in-memory network durably blocks inside synctest. This
// exercises the actual HTTP upgrade and WebSocket code without wall-clock waits.
func idleMemoryHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	return idleMemoryHubClock(t, nil)
}

func idleMemoryHubClock(t *testing.T, clock store.Clock) (*Hub, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "idle.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := New(s, "test-secret", []string{"example.com", "127.0.0.1"})
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The fake network uses documentation addresses. This fixture models a
		// local connection so account routes still exercise their loopback rule.
		r.RemoteAddr = "127.0.0.1:1234"
		h.ServeHTTP(w, r)
	}))
	_ = server.Client() // Initializes its fake network and URL before register uses it.
	return h, server
}

type idleObservedCalls struct{ validate, events, ui atomic.Int64 }

func idleCountCalls(h *Hub) *idleObservedCalls {
	calls := new(idleObservedCalls)
	real := h.call
	h.call = func(ctx context.Context, op string, request store.Request) (map[string]any, error) {
		switch op {
		case "validate":
			calls.validate.Add(1)
		case "events":
			calls.events.Add(1)
		case "ui_events":
			calls.ui.Add(1)
		}
		return real(ctx, op, request)
	}
	return calls
}

func (c *idleObservedCalls) snapshot() [3]int64 {
	return [3]int64{c.validate.Load(), c.events.Load(), c.ui.Load()}
}

type idleMemorySocket struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	frames []map[string]any
	closed bool
	err    error
}

func idleDial(t *testing.T, server *httptest.Server, endpoint string, headers http.Header, protocols []string) *idleMemorySocket {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), endpoint, &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: headers, Subprotocols: protocols,
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := &idleMemorySocket{conn: conn}
	t.Cleanup(func() { _ = conn.CloseNow() })
	go func() {
		for {
			_, raw, err := conn.Read(t.Context())
			socket.mu.Lock()
			if err != nil {
				socket.closed, socket.err = true, err
				socket.mu.Unlock()
				return
			}
			var frame map[string]any
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Error(err)
			} else {
				socket.frames = append(socket.frames, frame)
			}
			socket.mu.Unlock()
		}
	}()
	return socket
}

func idleWatch(t *testing.T, server *httptest.Server, execution any, after int64) *idleMemorySocket {
	t.Helper()
	return idleDial(t, server, watchURL(server, execution, after), http.Header{"Authorization": {"Bearer test-secret"}}, []string{"iris.v1"})
}

func idleNamedConsole(t *testing.T, server *httptest.Server, cookie *http.Cookie) *idleMemorySocket {
	t.Helper()
	return idleDial(t, server, "wss://127.0.0.1/v1/ui/watch", http.Header{
		"Origin": {"https://127.0.0.1"}, "Cookie": {cookie.String()},
	}, nil)
}

func (s *idleMemorySocket) snapshot() ([]map[string]any, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.frames...), s.closed, s.err
}

func TestIdleStreamsDoNotQueryForPollingOrPings(t *testing.T) {
	for _, count := range []int{1, 5, 20} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, server := idleMemoryHub(t)
				h.PingInterval = 200 * time.Millisecond
				calls := idleCountCalls(h)
				var pings atomic.Int64
				realPing := h.pingSocket
				h.pingSocket = func(ctx context.Context, conn *websocket.Conn) error {
					pings.Add(1)
					return realPing(ctx, conn)
				}
				session := register(t, server, 1)
				h.console = newConsoleAuth(make([]byte, 32))
				_, cookie, _ := routeConsoleAccount(t, h, "Idle operator", "operator")
				for range count {
					idleWatch(t, server, session["execution_id"], 0)
				}
				consoles := []*idleMemorySocket{idleNamedConsole(t, server, cookie), idleNamedConsole(t, server, cookie)}
				synctest.Wait()
				beforeCalls, beforeTransactions := calls.snapshot(), h.store.TransactionCount()
				beforePings := pings.Load()
				// This advances only synctest's virtual clock. Protocol pings and
				// the one central maintenance loop may run; sockets do no polling.
				time.Sleep(3 * time.Second)
				synctest.Wait()
				delta := h.store.TransactionCount() - beforeTransactions
				t.Logf("%d idle watches plus two consoles: %d transactions in three virtual seconds", count, delta)
				if pings.Load() <= beforePings {
					t.Fatal("idle period did not exercise protocol pings")
				}
				if after := calls.snapshot(); after != beforeCalls {
					t.Errorf("idle sockets queried the store: before %v after %v (validate/events/ui)", beforeCalls, after)
				}
				if delta > 3 {
					t.Errorf("%d idle watches plus two consoles made %d transactions; only one maintenance transaction per second is allowed", count, delta)
				}
				for _, socket := range consoles {
					if frames, closed, err := socket.snapshot(); closed || len(frames) != 1 || len(frames[0]) != 2 || frames[0]["kind"] != "changed" {
						t.Fatalf("named idle console did not retain its content-free initial revision: %v %v %v", frames, closed, err)
					}
				}
			})
		})
	}
}

func TestWatchDrainsAFullPageWithoutAnotherWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		a, b := register(t, server, 1), register(t, server, 2)
		before, err := h.store.Call(t.Context(), "events", store.Request{ExecutionID: b["execution_id"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		for i := range boundedPageSize + 1 {
			send(t, server, a, b, fmt.Sprintf("backlog-%d", i))
		}
		socket := idleWatch(t, server, b["execution_id"], before["high_water"].(int64))
		synctest.Wait()
		frames, closed, err := socket.snapshot()
		if closed || len(frames) != boundedPageSize+1 {
			t.Fatalf("initial backlog was not drained without another wake: %d frames, closed %v, error %v", len(frames), closed, err)
		}
		var cursor float64
		for _, frame := range frames {
			if len(frame) != 3 || frame["kind"] != "inbox_changed" || frame["stream"] == nil || frame["cursor"].(float64) <= cursor {
				t.Fatal("non-monotonic or content-bearing frame", frame)
			}
			cursor = frame["cursor"].(float64)
		}
	})
}

func TestStreamWakeCoalescesACommittedBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const watches, messages = 5, 12
		h, server := idleMemoryHub(t)
		a, b := register(t, server, 1), register(t, server, 2)
		h.console = newConsoleAuth(make([]byte, 32))
		_, cookie, _ := routeConsoleAccount(t, h, "Burst operator", "operator")
		calls := idleCountCalls(h)
		var sockets []*idleMemorySocket
		for range watches {
			sockets = append(sockets, idleWatch(t, server, b["execution_id"], 0))
		}
		consoles := []*idleMemorySocket{idleNamedConsole(t, server, cookie), idleNamedConsole(t, server, cookie)}
		synctest.Wait()
		beforeCalls, beforeTransactions := calls.snapshot(), h.store.TransactionCount()
		for i := range messages {
			send(t, server, a, b, fmt.Sprintf("coalesced-%d", i))
		}
		// The bounded debounce is activated by commits, never by idle polling.
		// Advance the virtual clock to let it fire, without reaching a sweep tick.
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		for _, socket := range sockets {
			frames, closed, err := socket.snapshot()
			if closed || len(frames) != messages {
				t.Fatalf("coalesced wake lost events: %d frames, closed %v, error %v", len(frames), closed, err)
			}
		}
		after := calls.snapshot()
		if after[0] != beforeCalls[0] || after[1]-beforeCalls[1] > watches || after[2]-beforeCalls[2] > 2 {
			t.Fatalf("burst was not coalesced into one observation per socket: before %v after %v", beforeCalls, after)
		}
		if delta := h.store.TransactionCount() - beforeTransactions; delta > messages+watches+2 {
			t.Fatalf("%d commits and %d sockets made %d transactions", messages, watches+2, delta)
		}
		for _, socket := range consoles {
			if frames, closed, err := socket.snapshot(); closed || len(frames) != 2 || len(frames[1]) != 2 || frames[1]["kind"] != "changed" || frames[1]["cursor"].(float64) <= frames[0]["cursor"].(float64) {
				t.Fatalf("named console did not receive one coalesced scoped revision: %v %v %v", frames, closed, err)
			}
		}
	})
}

func TestIdleWatchClosesWhenItsLeaseExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, server := idleMemoryHub(t)
		session := register(t, server, 1)
		request(t, server, http.MethodPost, "/v1/executions/"+session["execution_id"].(string)+"/heartbeat", map[string]any{"generation": 1, "source": "shim"}, http.StatusOK)
		socket := idleWatch(t, server, session["execution_id"], 0)
		synctest.Wait()
		if _, closed, err := socket.snapshot(); closed {
			t.Fatal("live Execution stream closed", err)
		}
		// The strong lease lasts 60 seconds; a central one-second sweep then
		// makes its expiry visible even with no API traffic or unrelated commits.
		time.Sleep(61 * time.Second)
		synctest.Wait()
		if _, closed, err := socket.snapshot(); !closed || websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("expired Execution kept its idle stream: closed %v error %v", closed, err)
		}
	})
}

func TestIdleWatchReceivesOneMissedReceiptWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		a, b := register(t, server, 1), register(t, server, 2)
		send(t, server, a, b, "missed-receipt")
		request(t, server, http.MethodGet, "/v1/inbox?execution_id="+b["execution_id"].(string)+"&receipt=1", nil, http.StatusOK)
		before, err := h.store.Call(t.Context(), "events", store.Request{ExecutionID: b["execution_id"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		socket := idleWatch(t, server, b["execution_id"], before["high_water"].(int64))
		synctest.Wait()
		if frames, closed, err := socket.snapshot(); closed || len(frames) != 0 {
			t.Fatalf("receipt deadline woke early: %v %v %v", frames, closed, err)
		}
		time.Sleep(61 * time.Second)
		synctest.Wait()
		frames, closed, err := socket.snapshot()
		if closed || len(frames) != 1 || len(frames[0]) != 3 || frames[0]["kind"] != "inbox_changed" {
			t.Fatalf("missing or content-bearing receipt wake: %v %v %v", frames, closed, err)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if frames, closed, err := socket.snapshot(); closed || len(frames) != 1 {
			t.Fatalf("receipt attempt woke more than once: %v %v %v", frames, closed, err)
		}
	})
}

func TestIdleWatchReceivesClaimExpiryWithoutTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		a, b := register(t, server, 1), register(t, server, 2)
		claim := authority(a)
		claim["key"], claim["kind"], claim["path"], claim["ttl_s"] = "short-claim", "exact", "expires.go", 2
		request(t, server, http.MethodPost, "/v1/claims", claim, http.StatusOK)
		before, err := h.store.Call(t.Context(), "events", store.Request{ExecutionID: b["execution_id"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		socket := idleWatch(t, server, b["execution_id"], before["high_water"].(int64))
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		frames, closed, err := socket.snapshot()
		if closed || len(frames) != 1 || len(frames[0]) != 3 || frames[0]["kind"] != "claim_released" {
			t.Fatalf("claim expiry was not committed and announced without API traffic: %v %v %v", frames, closed, err)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if frames, closed, err := socket.snapshot(); closed || len(frames) != 1 {
			t.Fatalf("claim expiry was announced more than once: %v %v %v", frames, closed, err)
		}
	})
}

func TestIdleWakeTicketRotationKeepsOtherExecutionsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, server := idleMemoryHub(t)
		a, b := register(t, server, 1), register(t, server, 2)
		open := func(session map[string]any) *idleMemorySocket {
			issued := request(t, server, http.MethodPost, "/v1/wake-tickets", authority(session), http.StatusOK)
			protocols := issued["protocols"].([]any)
			return idleDial(t, server, issued["url"].(string), nil, []string{protocols[0].(string), protocols[1].(string)})
		}
		first, other := open(a), open(b)
		synctest.Wait()
		request(t, server, http.MethodPost, "/v1/wake-tickets", authority(a), http.StatusOK)
		time.Sleep(time.Second)
		synctest.Wait()
		if _, closed, err := first.snapshot(); !closed || websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("rotated ticket socket survived: closed %v error %v", closed, err)
		}
		if _, closed, err := other.snapshot(); closed {
			t.Fatal("another Execution's ticket rotation closed this socket", err)
		}
	})
}

func TestIdleNamedConsolesDoNotQueryForPollingOrPings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		h.console = newConsoleAuth(make([]byte, 32))
		h.PingInterval = 200 * time.Millisecond
		_, cookie, _ := routeConsoleAccount(t, h, "Idle operator", "operator")
		first, second := idleNamedConsole(t, server, cookie), idleNamedConsole(t, server, cookie)
		synctest.Wait()
		before := h.store.TransactionCount()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if delta := h.store.TransactionCount() - before; delta > 3 {
			t.Errorf("two idle account consoles made %d transactions; only global maintenance is allowed", delta)
		}
		for _, socket := range []*idleMemorySocket{first, second} {
			frames, closed, err := socket.snapshot()
			if closed || len(frames) != 1 || len(frames[0]) != 2 || frames[0]["kind"] != "changed" {
				t.Fatalf("idle account stream changed or exposed more than its revision: %v %v %v", frames, closed, err)
			}
		}
	})
}

type idleManualClock struct{ wall, tick atomic.Int64 }

func (c *idleManualClock) Now() time.Time { return time.UnixMilli(c.wall.Load()) }
func (c *idleManualClock) Tick() int64    { return c.tick.Load() }
func (c *idleManualClock) advance(d time.Duration) {
	c.wall.Add(d.Milliseconds())
	c.tick.Add(d.Milliseconds())
}

func TestIdleNamedConsoleExpiresWithoutAnotherCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := new(idleManualClock)
		clock.wall.Store(time.Now().UnixMilli())
		h, server := idleMemoryHubClock(t, clock)
		h.console = newConsoleAuth(make([]byte, 32))
		_, cookie, _ := routeConsoleAccount(t, h, "Expiring operator", "operator")
		socket := idleNamedConsole(t, server, cookie)
		synctest.Wait()
		if _, closed, err := socket.snapshot(); closed {
			t.Fatal("valid account stream closed", err)
		}
		// Move the store's persisted expiry clock, then let the one central
		// maintenance timer run. No login, logout or repository commit follows.
		clock.advance(store.ConsoleSessionLifetime)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if _, closed, err := socket.snapshot(); !closed || err == nil {
			t.Fatalf("expired account retained its idle stream: closed %v error %v", closed, err)
		}
	})
}

func TestAccountRevocationClosesADurablyStalledWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		h.console = newConsoleAuth(make([]byte, 32))
		_, cookie, _ := routeConsoleAccount(t, h, "Blocked operator", "operator")
		writing := make(chan struct{})
		h.writeSocket = func(ctx context.Context, _ *websocket.Conn, _ []byte) error {
			close(writing)
			<-ctx.Done()
			return ctx.Err()
		}
		socket := idleNamedConsole(t, server, cookie)
		synctest.Wait()
		select {
		case <-writing:
		default:
			t.Fatal("stream did not reach its blocked network write")
		}
		if err := h.store.DeleteConsoleSession(t.Context(), h.console.mac("session", cookie.Value)); err != nil {
			t.Fatal("stalled network write retained the store lock", err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if _, closed, err := socket.snapshot(); !closed || err == nil {
			t.Fatalf("revocation left a stalled account stream open: closed %v error %v", closed, err)
		}
	})
}

func TestWakeTicketRotatedDuringUpgradeCannotObserveEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		calls := idleCountCalls(h)
		sender, recipient := register(t, server, 1), register(t, server, 2)
		send(t, server, sender, recipient, "waiting-during-upgrade")
		firstTicket := request(t, server, http.MethodPost, "/v1/wake-tickets", authority(recipient), http.StatusOK)
		accepted, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseUpgrade := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseUpgrade)
		var first atomic.Bool
		realAccept := h.acceptSocket
		h.acceptSocket = func(w http.ResponseWriter, r *http.Request, options *websocket.AcceptOptions) (*websocket.Conn, error) {
			conn, err := realAccept(w, r, options)
			if err == nil && first.CompareAndSwap(false, true) {
				close(accepted)
				<-release
			}
			return conn, err
		}
		open := func(issued map[string]any) *idleMemorySocket {
			protocols := issued["protocols"].([]any)
			return idleDial(t, server, issued["url"].(string), nil, []string{protocols[0].(string), protocols[1].(string)})
		}
		old := open(firstTicket)
		synctest.Wait()
		select {
		case <-accepted:
		default:
			t.Fatal("upgrade did not reach its post-accept barrier")
		}
		newTicket := request(t, server, http.MethodPost, "/v1/wake-tickets", authority(recipient), http.StatusOK)
		before := calls.events.Load()
		releaseUpgrade()
		synctest.Wait()
		frames, closed, err := old.snapshot()
		if len(frames) != 0 || !closed || websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("rotated upgrade observed the Execution: frames %v closed %v error %v", frames, closed, err)
		}
		if calls.events.Load() != before {
			t.Fatal("rotated ticket queried events before its initial presence check")
		}
		fresh := open(newTicket)
		synctest.Wait()
		frames, closed, err = fresh.snapshot()
		if closed || len(frames) != 1 || len(frames[0]) != 3 || frames[0]["kind"] != "inbox_changed" {
			t.Fatalf("fresh ticket did not receive waiting attention: frames %v closed %v error %v", frames, closed, err)
		}
	})
}

func TestSlowAccountWriterKeepsOnlyTheLatestQueuedRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server := idleMemoryHub(t)
		h.console = newConsoleAuth(make([]byte, 32))
		account, cookie, _ := routeConsoleAccount(t, h, "Slow operator", "operator")
		principal := store.Principal{Kind: store.ConsolePrincipal, SessionDigest: h.console.mac("session", cookie.Value)}
		initial, err := h.store.CallConsole(t.Context(), principal, "ui_events", store.Request{})
		if err != nil {
			t.Fatal(err)
		}
		writing, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseWrite)
		var writes atomic.Int64
		realWrite := h.writeSocket
		h.writeSocket = func(ctx context.Context, conn *websocket.Conn, raw []byte) error {
			if writes.Add(1) == 1 {
				close(writing)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return realWrite(ctx, conn, raw)
		}
		socket := idleNamedConsole(t, server, cookie)
		synctest.Wait()
		select {
		case <-writing:
		default:
			t.Fatal("initial admitted frame did not reach the blocked writer")
		}
		previous := initial["cursor"].(int64)
		for i := range 3 {
			account, err = h.store.EditConsoleProfile(t.Context(), principal, account.EditRevision, fmt.Sprintf("Profile revision %d", i), account.UILocale, account.CorrespondenceLocale)
			if err != nil {
				t.Fatal("profile edit revoked the observer or failed", err)
			}
			time.Sleep(2 * streamCoalesce)
			synctest.Wait()
			current, err := h.store.CallConsole(t.Context(), principal, "ui_events", store.Request{})
			if err != nil || current["cursor"].(int64) <= previous {
				t.Fatal("profile edit did not advance the scoped revision", current, err)
			}
			previous = current["cursor"].(int64)
			if frames, closed, err := socket.snapshot(); closed || len(frames) != 0 {
				t.Fatalf("blocked writer emitted a frame: %v %v %v", frames, closed, err)
			}
		}
		latest, err := h.store.CallConsole(t.Context(), principal, "ui_events", store.Request{})
		if err != nil {
			t.Fatal(err)
		}
		releaseWrite()
		synctest.Wait()
		frames, closed, err := socket.snapshot()
		if closed || len(frames) != 2 || writes.Load() != 2 {
			t.Fatalf("slow writer did not coalesce queued revisions: frames %v writes %d closed %v error %v", frames, writes.Load(), closed, err)
		}
		for _, frame := range frames {
			if len(frame) != 2 || frame["kind"] != "changed" {
				t.Fatal("scoped refresh frame exposed more than its revision", frame)
			}
		}
		if frames[0]["cursor"] != float64(initial["cursor"].(int64)) || frames[1]["cursor"] != float64(latest["cursor"].(int64)) {
			t.Fatalf("writer replayed a stale intermediate revision: frames %v initial %v latest %v", frames, initial, latest)
		}
	})
}
