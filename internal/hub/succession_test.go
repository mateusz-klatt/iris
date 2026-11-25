package hub

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

func TestHTTPSucceedRouteCreatesAndReplays(t *testing.T) {
	clock := &generationClock{now: time.Unix(1800000000, 0)}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := New(s, "test-secret", []string{"127.0.0.1"})
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)

	call := func(method, path string, req map[string]any, want int) map[string]any {
		t.Helper()
		var body *bytes.Reader
		if req != nil {
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(raw)
		} else {
			body = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, body)
		r.Header.Set("Authorization", "Bearer test-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: status %d: %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := call(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, http.StatusOK)

	clock.advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}

	// Exact proof succeeds once and creates G+1.
	succeedReq := map[string]any{
		"client": "codex", "host": "host", "session_id": "S", "source": "shim",
		"prior_generation": first["generation"], "prior_execution_id": first["execution_id"],
	}
	created := call(http.MethodPost, "/v1/sessions/succeed", succeedReq, http.StatusOK)
	childID := created["execution_id"].(string)
	if created["generation"] != float64(2) || childID == first["execution_id"] {
		t.Fatalf("created child: %+v", created)
	}
	if strings.Count(logs.String(), "session generation advanced") != 1 || !strings.Contains(logs.String(), "proof=shim_succession") {
		t.Fatalf("create generation log missing or duplicated: %s", logs.String())
	}

	eventsBefore := call(http.MethodGet, "/v1/events?execution_id="+childID+"&generation=2", nil, http.StatusOK)
	beforeWater := int64(eventsBefore["high_water"].(float64))

	// Replay returns the exact same live child without advancing the event high-water mark or logging a second bump.
	clock.advance(5 * time.Second)
	replayed := call(http.MethodPost, "/v1/sessions/succeed", succeedReq, http.StatusOK)
	if replayed["execution_id"] != childID || replayed["generation"] != float64(2) || replayed["lease_ms"] != float64(55000) {
		t.Fatalf("replayed child: %+v", replayed)
	}
	if strings.Count(logs.String(), "session generation advanced") != 1 {
		t.Fatalf("replay emitted a second generation bump log: %s", logs.String())
	}
	eventsAfter := call(http.MethodGet, "/v1/events?execution_id="+childID+"&generation=2", nil, http.StatusOK)
	if afterWater := int64(eventsAfter["high_water"].(float64)); afterWater != beforeWater {
		t.Fatalf("replay emitted event: before=%d after=%d", beforeWater, afterWater)
	}

	// Equal ended_through floor refuses with 409 lost + reason=ended_through.
	succeedReq["ended_through"] = float64(1)
	floorRefused := call(http.MethodPost, "/v1/sessions/succeed", succeedReq, http.StatusConflict)
	floorErr := floorRefused["error"].(map[string]any)
	if floorErr["code"] != "lost" || floorErr["details"].(map[string]any)["reason"] != "ended_through" {
		t.Fatalf("floor refusal: %+v", floorRefused)
	}
}

func TestHTTPSucceedRouteAuthAndUnexpectedCallerFences(t *testing.T) {
	clock := &generationClock{now: time.Unix(1800000000, 0)}
	dbFile := filepath.Join(t.TempDir(), "hub.db")
	s, err := store.Open(dbFile, clock)
	if err != nil {
		t.Fatal(err)
	}
	var openStores []*store.Store
	openStores = append(openStores, s)
	t.Cleanup(func() {
		for _, st := range openStores {
			if st != nil {
				_ = st.Close()
			}
		}
	})

	h := New(s, "owner-secret", []string{"127.0.0.1"})
	var logs bytes.Buffer
	h.Log = log.New(&logs, "", 0)

	doRaw := func(method, url string, rawBody []byte, headers map[string]string, cookies []*http.Cookie, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, url, bytes.NewReader(rawBody))
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, url, w.Code, want, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	do := func(method, path string, req map[string]any, token string, want int) map[string]any {
		t.Helper()
		var raw []byte
		if req != nil {
			var err error
			raw, err = json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
		}
		headers := map[string]string{}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		return doRaw(method, "http://127.0.0.1"+path, raw, headers, nil, want)
	}

	type hubSnapshot struct {
		status string
		logs   string
	}
	snapshot := func() hubSnapshot {
		t.Helper()
		status := do(http.MethodGet, "/v1/status", nil, "owner-secret", http.StatusOK)
		raw, err := json.Marshal(status["counts"])
		if err != nil {
			t.Fatal(err)
		}
		return hubSnapshot{status: string(raw), logs: logs.String()}
	}
	assertInert := func(before hubSnapshot, reason string) {
		t.Helper()
		after := snapshot()
		if after.status != before.status {
			t.Fatalf("%s mutated store counts: before=%s after=%s", reason, before.status, after.status)
		}
		if strings.Count(after.logs, "session generation advanced") != strings.Count(before.logs, "session generation advanced") {
			t.Fatalf("%s emitted generation bump log: before=%s after=%s", reason, before.logs, after.logs)
		}
	}

	// Mint and revoke host tokens via owner API.
	minted := do(http.MethodPost, "/v1/tokens", map[string]any{"host": "host"}, "owner-secret", http.StatusOK)
	hostToken := minted["token"].(string)
	revoked := do(http.MethodPost, "/v1/tokens", map[string]any{"host": "old-host"}, "owner-secret", http.StatusOK)
	revokedToken := revoked["token"].(string)
	do(http.MethodDelete, "/v1/tokens/"+revoked["name"].(string), nil, "owner-secret", http.StatusOK)

	// Register and expire a hook session for unexpected-caller fence tests.
	first := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-fence", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, hostToken, http.StatusOK)
	firstID := first["execution_id"].(string)
	clock.advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}

	validReq := map[string]any{
		"client": "codex", "host": "host", "session_id": "S-fence", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": firstID,
	}
	validRaw, err := json.Marshal(validReq)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Unauthenticated / bad-auth / disallowed-host requests fail before touching store.
	before := snapshot()
	for _, tc := range []struct {
		name    string
		url     string
		headers map[string]string
		cookies []*http.Cookie
	}{
		{"missing bearer", "http://127.0.0.1/v1/sessions/succeed", nil, nil},
		{"unknown bearer", "http://127.0.0.1/v1/sessions/succeed", map[string]string{"Authorization": "Bearer wrong"}, nil},
		{"revoked bearer", "http://127.0.0.1/v1/sessions/succeed", map[string]string{"Authorization": "Bearer " + revokedToken}, nil},
		{"console cookie without bearer", "http://127.0.0.1/v1/sessions/succeed", nil, []*http.Cookie{{Name: consoleCookieName, Value: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}},
		{"disallowed Host header", "http://evil.example/v1/sessions/succeed", map[string]string{"Authorization": "Bearer owner-secret"}, nil},
	} {
		t.Run("auth/"+tc.name, func(t *testing.T) {
			out := doRaw(http.MethodPost, tc.url, validRaw, tc.headers, tc.cookies, http.StatusUnauthorized)
			if out["error"].(map[string]any)["code"] != "unauthorized" {
				t.Fatalf("unexpected auth error: %+v", out)
			}
			assertInert(before, tc.name)
		})
	}

	// 2. Malformed inputs return bad_request without mutation.
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing source", func(m map[string]any) { delete(m, "source") }},
		{"hook source", func(m map[string]any) { m["source"] = "hook" }},
		{"mcp source", func(m map[string]any) { m["source"] = "mcp" }},
		{"zero prior_generation", func(m map[string]any) { m["prior_generation"] = 0 }},
		{"empty prior_execution_id", func(m map[string]any) { m["prior_execution_id"] = "" }},
		{"negative ended_through", func(m map[string]any) { m["ended_through"] = -1 }},
	} {
		t.Run("bad_request/"+tc.name, func(t *testing.T) {
			req := map[string]any{}
			for k, v := range validReq {
				req[k] = v
			}
			tc.change(req)
			out := do(http.MethodPost, "/v1/sessions/succeed", req, hostToken, http.StatusBadRequest)
			if out["error"].(map[string]any)["code"] != "bad_request" {
				t.Fatalf("unexpected error: %+v", out)
			}
			assertInert(before, tc.name)
		})
	}

	// 3. Wrong host, wrong client, wrong session, non-exact predecessor pair, and ended_through.
	for _, tc := range []struct {
		name       string
		change     func(map[string]any)
		wantReason string
	}{
		{"wrong host", func(m map[string]any) { m["host"] = "other-host" }, ""},
		{"wrong client", func(m map[string]any) { m["client"] = "claude" }, ""},
		{"wrong session_id", func(m map[string]any) { m["session_id"] = "S-other" }, ""},
		{"wrong prior_execution_id", func(m map[string]any) { m["prior_execution_id"] = "ex_wrong" }, ""},
		{"wrong prior_generation", func(m map[string]any) { m["prior_generation"] = float64(2) }, ""},
		{"equal ended_through", func(m map[string]any) { m["ended_through"] = float64(1) }, "ended_through"},
		{"higher ended_through", func(m map[string]any) { m["ended_through"] = float64(2) }, "ended_through"},
	} {
		t.Run("fence/"+tc.name, func(t *testing.T) {
			req := map[string]any{}
			for k, v := range validReq {
				req[k] = v
			}
			tc.change(req)
			out := do(http.MethodPost, "/v1/sessions/succeed", req, hostToken, http.StatusConflict)
			errObj := out["error"].(map[string]any)
			if errObj["code"] != "lost" {
				t.Fatalf("unexpected error: %+v", out)
			}
			if tc.wantReason != "" {
				if errObj["details"].(map[string]any)["reason"] != tc.wantReason {
					t.Fatalf("missing reason=%s: %+v", tc.wantReason, out)
				}
			}
			assertInert(before, tc.name)
		})
	}

	// 4. Live active predecessor and recovering in-window predecessor are refused inertly.
	active := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-live", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, hostToken, http.StatusOK)
	beforeLive := snapshot()
	liveRefused := do(http.MethodPost, "/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-live", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": active["execution_id"],
	}, hostToken, http.StatusConflict)
	if liveRefused["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("live predecessor error: %+v", liveRefused)
	}
	assertInert(beforeLive, "live active predecessor")

	// 5. Ended predecessor is refused inertly, even if a hook later starts G+1.
	ended := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-ended", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, hostToken, http.StatusOK)
	do(http.MethodPost, "/v1/executions/"+ended["execution_id"].(string)+"/end", map[string]any{"generation": float64(1)}, hostToken, http.StatusOK)
	beforeEnded := snapshot()
	endedRefused := do(http.MethodPost, "/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-ended", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": ended["execution_id"],
	}, hostToken, http.StatusConflict)
	if endedRefused["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("ended predecessor error: %+v", endedRefused)
	}
	assertInert(beforeEnded, "ended predecessor")
	// Hook advances S-ended to G2; caller replaying old ended G1 proof is still refused.
	do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-ended", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, hostToken, http.StatusOK)
	beforeEndedReplay := snapshot()
	endedReplayRefused := do(http.MethodPost, "/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-ended", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": ended["execution_id"],
	}, hostToken, http.StatusConflict)
	if endedReplayRefused["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("ended predecessor replay error: %+v", endedReplayRefused)
	}
	assertInert(beforeEndedReplay, "ended predecessor replay")

	// 6. Superseded predecessor (/clear on single-session cursor process) is refused inertly on create and replay.
	sup := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "cursor", "host": "host", "session_id": "S-sup-1", "cwd": "/repo/w", "worktree": "/repo/w",
		"source": "hook", "client_pid": 42, "client_pid_start_ms": 100, "single_session": true,
	}, hostToken, http.StatusOK)
	clock.advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	// First succeed succeeds with exact process identity, then /clear registers S-sup-2 on same PID.
	supReq := map[string]any{
		"client": "cursor", "host": "host", "session_id": "S-sup-1", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": sup["execution_id"],
		"client_pid": 42, "client_pid_start_ms": 100,
	}
	do(http.MethodPost, "/v1/sessions/succeed", supReq, hostToken, http.StatusOK)
	do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "cursor", "host": "host", "session_id": "S-sup-2", "cwd": "/repo/w", "worktree": "/repo/w",
		"source": "hook", "client_pid": 42, "client_pid_start_ms": 100, "single_session": true,
	}, hostToken, http.StatusOK)
	beforeSup := snapshot()
	supRefused := do(http.MethodPost, "/v1/sessions/succeed", supReq, hostToken, http.StatusConflict)
	if supRefused["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("superseded replay error: %+v", supRefused)
	}
	assertInert(beforeSup, "superseded replay")

	// 7. Choice A is deferred to stage 1b: existing v0.1.15 non-hook G=0 behavior after loss stays inert.
	inert := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-v15", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, hostToken, http.StatusOK)
	clock.advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	v15AfterLoss := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-v15", "cwd": "/repo/w", "worktree": "/repo/w", "source": "mcp",
	}, hostToken, http.StatusOK)
	if v15AfterLoss["generation"] != float64(2) || v15AfterLoss["execution_id"] == inert["execution_id"] {
		t.Fatalf("stage 1a altered v0.1.15 non-hook loss registration: %+v", v15AfterLoss)
	}

	// 8. Restarting hub puts active executions in recovery; succeed refuses recovering predecessor.
	rec := do(http.MethodPost, "/v1/sessions", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-rec", "cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
	}, hostToken, http.StatusOK)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	openStores[0] = nil
	reopened, err := store.Open(dbFile, clock)
	if err != nil {
		t.Fatal(err)
	}
	openStores = append(openStores, reopened)
	h = New(reopened, "owner-secret", []string{"127.0.0.1"})
	h.Log = log.New(&logs, "", 0)

	beforeRec := snapshot()
	recRefused := do(http.MethodPost, "/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host", "session_id": "S-rec", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": rec["execution_id"],
	}, hostToken, http.StatusConflict)
	if recRefused["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("recovering predecessor error: %+v", recRefused)
	}
	assertInert(beforeRec, "recovering predecessor")
}

// TestHTTPSucceedRouteTwoValidHostTokens exercises POST /v1/sessions/succeed with two distinct
// valid host tokens, verifying cross-host proof fencing and documenting that named HostToken
// records give attribution rather than host-to-host credential isolation (see docs/api.md).
func TestHTTPSucceedRouteTwoValidHostTokens(t *testing.T) {
	clock := &generationClock{now: time.Unix(1800000000, 0)}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := New(s, "owner-secret", []string{"127.0.0.1"})

	do := func(path string, req map[string]any, token string, want int) map[string]any {
		t.Helper()
		var body *bytes.Reader
		if req != nil {
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(raw)
		} else {
			body = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, body)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", path, w.Code, want, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	tokenA := do("/v1/tokens", map[string]any{"host": "host-a"}, "owner-secret", http.StatusOK)["token"].(string)
	tokenB := do("/v1/tokens", map[string]any{"host": "host-b"}, "owner-secret", http.StatusOK)["token"].(string)

	sessA := do("/v1/sessions", map[string]any{
		"client": "codex", "host": "host-a", "session_id": "S-a", "cwd": "/repo/wa", "worktree": "/repo/wa", "source": "hook",
	}, tokenA, http.StatusOK)
	sessB := do("/v1/sessions", map[string]any{
		"client": "codex", "host": "host-b", "session_id": "S-b", "cwd": "/repo/wb", "worktree": "/repo/wb", "source": "hook",
	}, tokenB, http.StatusOK)

	clock.advance(16 * time.Minute)
	if _, err := s.Call(t.Context(), "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}

	// Token B cannot attach host-b to host-a's predecessor proof, and vice versa.
	refusedAB := do("/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host-b", "session_id": "S-a", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": sessA["execution_id"],
	}, tokenB, http.StatusConflict)
	if refusedAB["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("expected 409 lost on cross-host mismatch: %+v", refusedAB)
	}
	refusedBA := do("/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host-a", "session_id": "S-b", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": sessB["execution_id"],
	}, tokenA, http.StatusConflict)
	if refusedBA["error"].(map[string]any)["code"] != "lost" {
		t.Fatalf("expected 409 lost on reverse cross-host mismatch: %+v", refusedBA)
	}

	// Token B succeeds cleanly for its own host-b session.
	succB := do("/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host-b", "session_id": "S-b", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": sessB["execution_id"],
	}, tokenB, http.StatusOK)
	if succB["generation"] != float64(2) {
		t.Fatalf("unexpected token-B succession: %+v", succB)
	}

	// Today's HostToken model has no host-to-token binding on any session route (attribution only):
	// token B presenting host-a's exact proof behaves the same as /v1/sessions and succeeds/replays.
	succCross := do("/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host-a", "session_id": "S-a", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": sessA["execution_id"],
	}, tokenB, http.StatusOK)
	replayedByA := do("/v1/sessions/succeed", map[string]any{
		"client": "codex", "host": "host-a", "session_id": "S-a", "source": "shim",
		"prior_generation": float64(1), "prior_execution_id": sessA["execution_id"],
	}, tokenA, http.StatusOK)
	if replayedByA["execution_id"] != succCross["execution_id"] {
		t.Fatalf("replayed child changed across tokens: %+v vs %+v", succCross, replayedByA)
	}
}
