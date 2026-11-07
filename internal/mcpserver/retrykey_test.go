package mcpserver

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// at fixes the clock derivedKey reads for the rest of the test.
func at(t *testing.T, when time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return when }
	t.Cleanup(func() { now = old })
}

// A send whose response is lost, as when the hub committed it but the connection dropped, is
// retried by the model with the same arguments and no key. The retry must carry the same key,
// so the hub returns the first send instead of making a second one (gap review A9).
func TestARetryAfterALostResponseReusesTheKey(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	at(t, start)
	var mu sync.Mutex
	var keys []string
	lose := true
	f := &fakeHub{reply: `{"message_id":"ms_1"}`}
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			mu.Lock()
			drop := lose
			lose = false
			mu.Unlock()
			f.ServeHTTP(w, r)
			if drop {
				// The hub has the message, but the caller never hears so.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			mu.Lock()
			keys = append(keys, f.last(t).body["key"].(string))
			mu.Unlock()
			return
		}
		f.ServeHTTP(w, r)
	})
	session := connectHandler(t, hub)
	args := map[string]any{"to": []string{"a"}, "subject": "s", "body": "b"}
	if _, failed := use(t, session, "send", args); !failed {
		t.Fatal("the lost response was reported as a success")
	}
	at(t, start.Add(time.Minute))
	out, failed := use(t, session, "send", args)
	if failed {
		t.Fatal(out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != keys[1] || out["key"] != keys[0] || !strings.HasPrefix(keys[0], "derived:") {
		t.Fatalf("keys %v, returned %v: a retry must reuse the first key", keys, out["key"])
	}
}

// The same message sent on purpose eleven minutes later is a new send, not a retry.
func TestAnIdenticalSendInALaterWindowGetsANewKey(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	f := &fakeHub{reply: `{}`}
	session := connect(t, f)
	args := map[string]any{"to": []string{"a"}, "body": "ping"}
	at(t, start)
	first, _ := use(t, session, "send", args)
	at(t, start.Add(11*time.Minute))
	second, _ := use(t, session, "send", args)
	if first["key"] == second["key"] {
		t.Fatalf("an identical send eleven minutes later reused key %v", first["key"])
	}
}

// Different arguments get different keys, a key the caller passes wins, and claim derives its
// key the same way.
func TestDerivedKeysFollowTheArgumentsAndAnExplicitKeyWins(t *testing.T) {
	at(t, time.Unix(1_800_000_000, 0))
	f := &fakeHub{reply: `{}`}
	session := connect(t, f)
	a, _ := use(t, session, "send", map[string]any{"to": []string{"a"}, "body": "one"})
	b, _ := use(t, session, "send", map[string]any{"to": []string{"a"}, "body": "two"})
	mine, _ := use(t, session, "send", map[string]any{"to": []string{"a"}, "body": "one", "key": "mine"})
	c1, _ := use(t, session, "claim", map[string]any{"kind": "exact", "path": "p"})
	c2, _ := use(t, session, "claim", map[string]any{"kind": "exact", "path": "p"})
	c3, _ := use(t, session, "claim", map[string]any{"kind": "exact", "path": "q"})
	if a["key"] == b["key"] || mine["key"] != "mine" || c1["key"] != c2["key"] || c1["key"] == c3["key"] || c1["key"] == a["key"] {
		t.Fatalf("send %v %v %v, claim %v %v %v", a["key"], b["key"], mine["key"], c1["key"], c2["key"], c3["key"])
	}
}
