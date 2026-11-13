package wake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
)

const (
	kimiTestVersion = "2.6.0"
	kimiTestSession = "session_" + commandSession
	kimiTestToken   = "test-only-kimi-token"
)

type kimiFake struct {
	t            *testing.T
	home         string
	server       *httptest.Server
	commands     *kimiCommands
	mu           sync.Mutex
	metaCode     int
	sessionCode  int
	promptCode   int
	session      map[string]any
	promptStatus string
	prompts      []map[string]any
	paths        []string
	rawResponse  string
}

func newKimiFake(t *testing.T) *kimiFake {
	t.Helper()
	f := &kimiFake{t: t, home: t.TempDir(), promptStatus: "running"}
	if err := os.MkdirAll(filepath.Join(f.home, "server", "instances"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "server.token"), []byte(kimiTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.resetSession()
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	u, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	f.writeInstance("server-good.json", map[string]any{
		"server_id": "server-good", "host": u.Hostname(), "port": port, "host_version": kimiTestVersion,
	})
	f.commands = newKimiCommands()
	f.commands.home = f.home
	f.commands.http = f.server.Client()
	f.commands.now = func() time.Time { return time.Unix(1800000000, 0) }
	return f
}

func (f *kimiFake) resetSession() {
	f.session = map[string]any{
		"id": kimiTestSession, "busy": false, "main_turn_active": false,
		"pending_interaction": "none", "archived": false,
	}
}

func (f *kimiFake) writeInstance(name string, value map[string]any) {
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "server", "instances", name), raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func writeKimiEnvelope(w http.ResponseWriter, code int, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": "test", "data": data, "request_id": "test"})
}

func (f *kimiFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+kimiTestToken {
		w.WriteHeader(http.StatusUnauthorized)
		writeKimiEnvelope(w, 40101, nil)
		return
	}
	if f.rawResponse != "" {
		_, _ = io.WriteString(w, f.rawResponse)
		return
	}
	switch r.URL.Path {
	case "/api/v1/meta":
		if f.metaCode != 0 {
			writeKimiEnvelope(w, f.metaCode, nil)
			return
		}
		writeKimiEnvelope(w, 0, map[string]any{"server_id": "live-server-id", "server_version": kimiTestVersion})
	case "/api/v1/sessions/" + kimiTestSession:
		if f.sessionCode != 0 {
			writeKimiEnvelope(w, f.sessionCode, nil)
			return
		}
		writeKimiEnvelope(w, 0, f.session)
	case "/api/v1/sessions/" + kimiTestSession + "/prompts":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			writeKimiEnvelope(w, 40001, nil)
			return
		}
		f.prompts = append(f.prompts, body)
		if f.promptCode != 0 {
			writeKimiEnvelope(w, f.promptCode, nil)
			return
		}
		writeKimiEnvelope(w, 0, map[string]any{
			"prompt_id": body["prompt_id"], "status": f.promptStatus, "content": body["content"],
		})
	default:
		writeKimiEnvelope(w, 40401, nil)
	}
}

func (f *kimiFake) check() (binaryInfo, string) {
	return f.commands.check(context.Background(), kimiTestVersion)
}

func admitKimi(start func() error) (bool, error) {
	if err := start(); err != nil {
		return false, err
	}
	return true, nil
}

func TestKimiNativeServerDiscoveryIdleAndPromptQueue(t *testing.T) {
	f := newKimiFake(t)
	info, detail := f.check()
	if detail != "" || info.Path != f.server.URL || info.Version != kimiTestVersion || info.ClientVersion != kimiTestVersion || info.CheckedMS == 0 {
		t.Fatalf("server preflight: %+v, %q", info, detail)
	}
	idle, detail := f.commands.idle(context.Background(), kimiTestSession)
	if !idle || detail != "" {
		t.Fatalf("idle session was not established: %t %q", idle, detail)
	}
	message := promptFor("0123456789abcdef0123456789abcdef")
	admissions := 0
	answer := f.commands.queue(context.Background(), kimiTestSession, message, func(start func() error) (bool, error) {
		admissions++
		return admitKimi(start)
	})
	if answer.answer != "submitted" || !answer.started || answer.detail != "" || admissions != 1 {
		t.Fatalf("prompt was not accepted: %+v, admissions %d", answer, admissions)
	}
	if len(f.prompts) != 1 || f.prompts[0]["prompt_id"] != kimiPromptID(message) {
		t.Fatalf("prompt id is not stable and client chosen: %+v", f.prompts)
	}
	content, ok := f.prompts[0]["content"].([]any)
	if !ok || len(content) != 1 || content[0].(map[string]any)["text"] != message || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("unexpected prompt content: %+v", f.prompts[0]["content"])
	}
	for _, path := range f.paths {
		if strings.Contains(path, "takeover") {
			t.Fatalf("native wake attempted a lock takeover: %s", path)
		}
	}
	f.promptStatus = "queued"
	if got := f.commands.queue(context.Background(), kimiTestSession, message, admitKimi); got.answer != "submitted" {
		t.Fatalf("native queued status was not accepted: %+v", got)
	}
	for _, status := range []string{"blocked", "failed"} {
		f.promptStatus = status
		if got := f.commands.queue(context.Background(), kimiTestSession, message, admitKimi); got.answer != "failed" || got.detail != "Kimi rejected the wake prompt" {
			t.Fatalf("%s was not a known rejected submission: %+v", status, got)
		}
	}
}

func TestKimiNativeIdleRequiresAnExactUsableSession(t *testing.T) {
	f := newKimiFake(t)
	if _, detail := f.check(); detail != "" {
		t.Fatal(detail)
	}
	for _, tc := range []struct {
		name   string
		mutate func()
		active bool
	}{
		{"busy", func() { f.session["busy"] = true }, true},
		{"turn active", func() { f.session["main_turn_active"] = true }, true},
		{"archived", func() { f.session["archived"] = true }, true},
		{"pending approval", func() { f.session["pending_interaction"] = "approval" }, true},
		{"wrong session", func() { f.session["id"] = "session_other" }, false},
		{"missing busy", func() { delete(f.session, "busy") }, false},
		{"missing turn", func() { delete(f.session, "main_turn_active") }, false},
		{"missing archive", func() { delete(f.session, "archived") }, false},
		{"lock conflict", func() { f.sessionCode = 40931 }, true},
		{"missing session", func() { f.sessionCode = 40401 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.resetSession()
			f.sessionCode = 0
			tc.mutate()
			idle, detail := f.commands.idle(context.Background(), kimiTestSession)
			want := "Kimi session status could not be verified"
			if tc.active {
				want = kimiActive
			}
			if idle || detail != want {
				t.Fatalf("idle %t, detail %q; want false, %q", idle, detail, want)
			}
		})
	}
}

func TestKimiNativePreflightFailuresAndCandidateFallback(t *testing.T) {
	f := newKimiFake(t)
	if _, detail := f.commands.check(context.Background(), ""); detail != "Kimi initialization did not name its version" {
		t.Fatalf("missing initialized version: %q", detail)
	}
	if _, detail := f.commands.check(context.Background(), "other-version"); detail != "Kimi executable and server versions differ" {
		t.Fatalf("version mismatch: %q", detail)
	}
	broken := &kimiCommands{homeFailure: "home failed", now: time.Now}
	if _, detail := broken.check(context.Background(), kimiTestVersion); detail != "home failed" {
		t.Fatalf("home failure: %q", detail)
	}
	missingToken := newKimiFake(t)
	if err := os.Remove(filepath.Join(missingToken.home, "server.token")); err != nil {
		t.Fatal(err)
	}
	if _, detail := missingToken.check(); detail != "Kimi server token could not be read" {
		t.Fatalf("missing token: %q", detail)
	}
	missingInstances := newKimiFake(t)
	if err := os.RemoveAll(filepath.Join(missingInstances.home, "server", "instances")); err != nil {
		t.Fatal(err)
	}
	if _, detail := missingInstances.check(); detail != "Kimi server instances could not be read" {
		t.Fatalf("missing instances: %q", detail)
	}

	fallback := newKimiFake(t)
	instances := filepath.Join(fallback.home, "server", "instances")
	for _, file := range []string{"z-ignore.txt", "broken.json", "z-large.json", "wrong-host.json", "wrong-port.json", "wrong-meta.json"} {
		if err := os.WriteFile(filepath.Join(instances, file), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(instances, "z-directory.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instances, "z-large.json"), []byte(strings.Repeat("x", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback.writeInstance("wrong-host.json", map[string]any{"server_id": "bad", "host": "localhost", "port": 1, "host_version": kimiTestVersion})
	fallback.writeInstance("wrong-port.json", map[string]any{"server_id": "bad", "host": "127.0.0.1", "port": 0, "host_version": kimiTestVersion})
	fallback.writeInstance("wrong-meta.json", map[string]any{"server_id": "bad", "host": "127.0.0.1", "port": 1, "host_version": kimiTestVersion})
	info, detail := fallback.check()
	if detail != "" || info.Path != fallback.server.URL {
		t.Fatalf("valid instance did not survive invalid candidates: %+v %q", info, detail)
	}
}

func TestKimiNativeTokenValidation(t *testing.T) {
	f := newKimiFake(t)
	path := filepath.Join(f.home, "server.token")
	for _, tc := range []struct {
		name, contents string
		valid          bool
	}{
		{"newline", " token\n", true},
		{"empty", "\n", false},
		{"embedded whitespace", "two tokens", false},
		{"control", "token\x00rest", false},
		{"oversized", strings.Repeat("x", 4097), false},
		{"too long", strings.Repeat("x", 513), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			token, err := f.commands.readSecret(path)
			if (err == nil) != tc.valid || tc.valid && token != "token" {
				t.Fatalf("token %q, error %v", token, err)
			}
		})
	}
}

func TestKimiNativeQueueOutcomesAndAdmission(t *testing.T) {
	f := newKimiFake(t)
	if _, detail := f.check(); detail != "" {
		t.Fatal(detail)
	}
	message := promptFor("fedcba9876543210fedcba9876543210")
	for _, code := range []int{40001, 40101, 40110, 40113, 40401, 40407, 40415, 40420, 40912, 40926, 40930, 40931, 40932} {
		f.promptCode = code
		if got := f.commands.queue(context.Background(), kimiTestSession, message, admitKimi); got.answer != "failed" || got.detail != "Kimi refused the wake prompt" {
			t.Fatalf("API code %d: %+v", code, got)
		}
	}
	for _, tc := range []struct {
		name, promptID, status string
		code                   int
	}{
		{"idempotency conflict", "", "running", 40927},
		{"wrong id", "another-prompt", "running", 0},
		{"unknown status", "", "future", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.promptCode, f.promptStatus = tc.code, tc.status
			if tc.promptID != "" {
				f.promptCode = 0
				f.rawResponse = fmt.Sprintf(`{"code":0,"data":{"prompt_id":%q,"status":"running","content":[{"type":"text","text":%q}]}}`, tc.promptID, message)
			}
			got := f.commands.queue(context.Background(), kimiTestSession, message, admitKimi)
			if got.answer != "unknown" || !got.started || got.detail != "Kimi prompt response was not a known outcome" {
				t.Fatalf("uncertain response: %+v", got)
			}
			f.rawResponse = ""
		})
	}
	f.promptCode = 0
	before := len(f.prompts)
	if got := f.commands.queue(context.Background(), kimiTestSession, message, func(func() error) (bool, error) { return false, nil }); got.answer != "aborted" || len(f.prompts) != before {
		t.Fatalf("binding refusal submitted a prompt: %+v", got)
	}
	if got := f.commands.queue(context.Background(), kimiTestSession, message, func(func() error) (bool, error) { return false, errors.New("admission") }); got.answer != "failed" || got.detail != "Kimi wake admission failed" || len(f.prompts) != before {
		t.Fatalf("admission failure submitted a prompt: %+v", got)
	}
	withoutServer := &kimiCommands{}
	if idle, detail := withoutServer.idle(context.Background(), kimiTestSession); idle || detail != "Kimi web server has not passed its preflight" {
		t.Fatalf("idle without preflight: %t %q", idle, detail)
	}
	if got := withoutServer.queue(context.Background(), kimiTestSession, message, admitKimi); got.answer != "failed" || got.started {
		t.Fatalf("queue without preflight: %+v", got)
	}
}

func TestKimiNativeTransportFailuresAreUnknown(t *testing.T) {
	f := newKimiFake(t)
	if _, detail := f.check(); detail != "" {
		t.Fatal(detail)
	}
	f.server.Close()
	if got := f.commands.queue(context.Background(), kimiTestSession, promptFor("token"), admitKimi); got.answer != "unknown" || !got.started {
		t.Fatalf("lost response was treated as a definite failure: %+v", got)
	}
}

func TestKimiPromptIDIsDeterministicAndMessageBound(t *testing.T) {
	first := promptFor("first")
	sum := sha256.Sum256([]byte(first))
	want := "iris-" + hex.EncodeToString(sum[:])
	if got := kimiPromptID(first); got != want || got == kimiPromptID(promptFor("second")) {
		t.Fatalf("prompt id = %q, want %q", got, want)
	}
}

func TestCanonicalTargetSessionAcceptsOnlyKimiSessionUUID(t *testing.T) {
	upper := "session_" + strings.ToUpper(commandSession)
	target := binding.WakeTarget{Client: "kimi", SessionID: upper}
	if got, ok := canonicalTargetSession(target); !ok || got != kimiTestSession {
		t.Fatalf("canonical Kimi session = %q, %t", got, ok)
	}
	for _, invalid := range []string{"", commandSession, "session_not-a-uuid", "other_" + commandSession} {
		target.SessionID = invalid
		if got, ok := canonicalTargetSession(target); ok || got != "" {
			t.Fatalf("invalid Kimi session %q became %q", invalid, got)
		}
	}
	target.Client = ""
	target.SessionID = strings.ToUpper(commandSession)
	if got, ok := canonicalTargetSession(target); !ok || got != commandSession {
		t.Fatalf("Codex canonicalization changed: %q %t", got, ok)
	}
}

func TestNewKimiRunnerUsesNativeDefaultsWithoutCreatingState(t *testing.T) {
	resolver := binding.New(nil, "kimi")
	sessions := hook.NewSessions(filepath.Join(t.TempDir(), "token"))
	r := NewKimi(resolver, sessions, func() string { return kimiTestVersion }, nil)
	if r.resolver != resolver || r.sessions != sessions || r.clientVersion() != kimiTestVersion {
		t.Fatal("Kimi runner dependencies were lost")
	}
	if _, ok := r.commands.(*kimiCommands); !ok {
		t.Fatalf("Kimi runner received %T", r.commands)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.Run(ctx)
	if _, err := os.Stat(sessions.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unarmed Kimi runner created state: %v", err)
	}
}

func TestKimiNativeReportsNoCompatibleInstance(t *testing.T) {
	f := newKimiFake(t)
	f.writeInstance("server-good.json", map[string]any{
		"server_id": "server-good", "host": "127.0.0.1", "port": mustKimiPort(t, f.server.URL), "host_version": "registry-old",
	})
	if _, detail := f.check(); detail != "Kimi web server is unavailable or incompatible" {
		t.Fatalf("mismatched server identity passed preflight: %q", detail)
	}
}

func mustKimiPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestKimiCommandsResolveTheirInheritedHome(t *testing.T) {
	t.Setenv("KIMI_CODE_HOME", filepath.Join("relative", "kimi-home"))
	if got := newKimiCommands().home; got != filepath.Join("relative", "kimi-home") {
		t.Fatalf("custom Kimi home = %q", got)
	}
	t.Setenv("KIMI_CODE_HOME", "")
	original := kimiUserHomeDir
	t.Cleanup(func() { kimiUserHomeDir = original })
	kimiUserHomeDir = func() (string, error) { return filepath.Join("home", "person"), nil }
	if got := newKimiCommands().home; got != filepath.Join("home", "person", ".kimi-code") {
		t.Fatalf("default Kimi home = %q", got)
	}
	kimiUserHomeDir = func() (string, error) { return "", errors.New("no home") }
	if got := newKimiCommands(); got.homeFailure != "Kimi home could not be determined" {
		t.Fatalf("home resolution error was lost: %+v", got)
	}
}

type kimiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kimiRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type kimiBrokenBody struct{}

func (kimiBrokenBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (kimiBrokenBody) Close() error             { return nil }

func TestKimiCallRejectsInvalidRequestsAndResponses(t *testing.T) {
	f := newKimiFake(t)
	server := &kimiServer{base: f.server.URL, token: kimiTestToken}
	if err := f.commands.call(context.Background(), server, http.MethodPost, "/api/v1/meta", make(chan int), nil); err == nil {
		t.Fatal("an unencodable request body was accepted")
	}
	if err := f.commands.call(context.Background(), server, "bad\nmethod", "/api/v1/meta", nil, nil); err == nil {
		t.Fatal("an invalid HTTP method was accepted")
	}
	for _, raw := range []string{"not json", `{}`, strings.Repeat("x", 65537)} {
		f.rawResponse = raw
		if err := f.commands.call(context.Background(), server, http.MethodGet, "/api/v1/meta", nil, nil); err == nil {
			t.Fatalf("malformed response was accepted: %.20s", raw)
		}
	}
	f.rawResponse = `{"code":40931}`
	err := f.commands.call(context.Background(), server, http.MethodGet, "/api/v1/meta", nil, nil)
	var api *kimiAPIError
	if !errors.As(err, &api) || api.code != 40931 || api.Error() != "Kimi API error 40931" {
		t.Fatalf("API error lost its fixed identity: %v", err)
	}
	f.rawResponse = `{"code":0}`
	if err := f.commands.call(context.Background(), server, http.MethodGet, "/api/v1/meta", nil, nil); err != nil {
		t.Fatalf("a response without a destination failed: %v", err)
	}
	var destination map[string]any
	if err := f.commands.call(context.Background(), server, http.MethodGet, "/api/v1/meta", nil, &destination); err == nil {
		t.Fatal("missing response data was accepted")
	}
	f.rawResponse = `{"code":0,"data":{}}`
	if err := f.commands.call(context.Background(), server, http.MethodGet, "/api/v1/meta", nil, make(chan int)); err == nil {
		t.Fatal("an undecodable response destination was accepted")
	}
	f.rawResponse = ""
	broken := &kimiCommands{http: &http.Client{Transport: kimiRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: kimiBrokenBody{}, Header: http.Header{}}, nil
	})}}
	if err := broken.call(context.Background(), server, http.MethodGet, "/api/v1/meta", nil, nil); err == nil {
		t.Fatal("a response body read failure was accepted")
	}
}

func TestKimiCallDoesNotForwardCredentialsAcrossRedirects(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/api/v1/meta", http.StatusFound)
			return
		}
		writeKimiEnvelope(w, 0, map[string]any{})
	}))
	defer server.Close()
	commands := &kimiCommands{http: server.Client()}
	err := commands.call(context.Background(), &kimiServer{base: server.URL, token: kimiTestToken}, http.MethodGet, "/redirect", nil, nil)
	if err == nil || requests != 1 {
		t.Fatalf("redirect was followed: requests %d, error %v", requests, err)
	}
}

func TestKimiEngineAdmitsCanonicalSessionAndUsesKimiGuard(t *testing.T) {
	e, resolver, source, now := newEngineTest(t)
	f := newKimiFake(t)
	target := binding.WakeTarget{
		Client: "kimi", SessionID: "session_" + strings.ToUpper(commandSession), Host: "kimi-host",
		Bound: binding.Bound{ExecutionID: "kimi-execution", Generation: 2, Rung: 3},
	}
	e.target, resolver.target = target, target
	e.commands = f.commands
	e.clientVersion = func() string { return kimiTestVersion }
	e.store = newClientStateStore(e.store.sessions, "kimi", target.SessionID)
	if got := e.step(context.Background()); !got.FileWait {
		t.Fatalf("Kimi admission did not reach its accepted prompt guard: %+v", got)
	}
	if resolver.starts != 1 || len(f.prompts) != 1 || source.target != target {
		t.Fatalf("wrong Kimi admission evidence: starts %d, prompts %d, target %+v", resolver.starts, len(f.prompts), source.target)
	}
	snapshot := mustSnapshot(t, e.store)
	if e.store.client != "kimi" || e.store.path() != e.store.sessions.WakePath("kimi", target.SessionID) || len(snapshot.Document.Attempts) != 1 || snapshot.Document.Attempts[0].Answer != "submitted" {
		t.Fatalf("Kimi guard was not retained under the native session: %+v", snapshot)
	}
	if snapshot.Document.Attempts[0].ConsumedMS != 0 {
		t.Fatal("prompt was consumed without its hook")
	}
	f.resetSession()
	f.session["busy"] = true
	*now = now.Add(15 * time.Second)
	snapshot.Document.Attempts[0].ConsumedMS = now.UnixMilli()
	writeState(t, e.store, snapshot.Document)
	source.read.Attention.Identity.Cursor++
	if got := e.step(context.Background()); got != engineDue(*now, 15*time.Second) {
		t.Fatalf("busy Kimi session did not get an ordinary retry: %+v", got)
	}
	if current := mustSnapshot(t, e.store).Document; current.LastFailure != nil {
		t.Fatalf("ordinary busy state became an operational failure: %+v", current.LastFailure)
	}
}
