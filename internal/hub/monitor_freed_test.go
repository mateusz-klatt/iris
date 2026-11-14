package hub

import (
	"net/http/httptest"
	"testing"
)

// freePath has b refused src/a.go while a holds it, then has a release it, so the hub
// records a claim_available for b.
func freePath(t *testing.T, s *httptest.Server, a, b map[string]any) {
	t.Helper()
	r := authority(a)
	r["key"] = "claim"
	r["kind"] = "exact"
	r["path"] = "src/a.go"
	claim := request(t, s, "POST", "/v1/claims", r, 200)
	r = authority(b)
	r["tool_call_id"] = "touch"
	r["paths"] = []string{"src/a.go"}
	if request(t, s, "POST", "/v1/claims/touch", r, 200)["action"] != "deny" {
		t.Fatal("missing deny")
	}
	r = authority(a)
	r["fence"] = claim["fence"]
	r["token"] = claim["token"]
	request(t, s, "POST", "/v1/claims/release", r, 200)
}

// A path freed for a session before its first monitor connected is the monitor's first frame,
// and mail it fetched in the meantime replays as read, never as new mail.
func TestAFirstMonitorHearsAPathFreedBeforeIt(t *testing.T) {
	_, s := setup(t)
	a, b := register(t, s, 1), register(t, s, 2)
	freePath(t, s, a, b)
	send(t, s, a, b, "fetched")
	request(t, s, "GET", "/v1/inbox?execution_id="+b["execution_id"].(string), nil, 200)
	conn := monitor(t, s, b["execution_id"], 0)
	for _, want := range []string{"claim_available", "inbox_read"} {
		if got := frame(t, conn); got["kind"] != want {
			t.Fatalf("want %s, got %v", want, got)
		}
	}
	send(t, s, a, b, "later")
	if got := frame(t, conn); got["kind"] != "inbox_changed" {
		t.Fatal(got)
	}
}

// With mail also waiting, from before the freed path or after it, the replay announces both and
// no reset follows. A monitor moves its cursor to the newest event on a reset, so a reset before
// the replay would skip it, and one after it would count the replayed mail again.
func TestAFirstMonitorReplaysWaitingMailWithTheFreedPath(t *testing.T) {
	for name, tc := range map[string]struct {
		before bool
		want   []string
	}{
		"mail before": {true, []string{"inbox_changed", "claim_released", "claim_available"}},
		"mail after":  {false, []string{"claim_available", "inbox_changed"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, s := setup(t)
			a, b := register(t, s, 1), register(t, s, 2)
			if tc.before {
				send(t, s, a, b, "waiting")
			}
			freePath(t, s, a, b)
			if !tc.before {
				send(t, s, a, b, "waiting")
			}
			conn := monitor(t, s, b["execution_id"], 0)
			for _, want := range tc.want {
				if got := frame(t, conn); got["kind"] != want {
					t.Fatalf("want %s, got %v", want, got)
				}
			}
			send(t, s, a, b, "later")
			if got := frame(t, conn); got["kind"] != "inbox_changed" {
				t.Fatalf("a reset or another frame followed the replay: %v", got)
			}
		})
	}
}
