package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	wall time.Time
	tick int64
}

func (c *fakeClock) Now() time.Time          { return c.wall }
func (c *fakeClock) Tick() int64             { return c.tick }
func (c *fakeClock) advance(d time.Duration) { c.wall = c.wall.Add(d); c.tick += d.Milliseconds() }
func testStore(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	c := &fakeClock{wall: time.Unix(1800000000, 0)}
	file := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.lock != nil {
			_ = s.Close()
		}
	})
	return s, c, file
}
func call(t *testing.T, s *Store, op string, r Request) map[string]any {
	t.Helper()
	out, err := s.Call(context.Background(), op, r)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return out
}
func expectError(t *testing.T, s *Store, op string, r Request, code string) {
	t.Helper()
	_, err := s.Call(context.Background(), op, r)
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("%s: got %v, wanted %s", op, err, code)
	}
}
func session(t *testing.T, s *Store, n int) (Request, map[string]any) {
	t.Helper()
	r := Request{Client: "codex", Host: "host", Cwd: fmt.Sprintf("/repo/w%d", n), Worktree: fmt.Sprintf("/repo/w%d", n), RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: fmt.Sprint(n), Source: "hook"}
	out := call(t, s, "sessions", r)
	return Request{ExecutionID: out["execution_id"].(string), Generation: out["generation"].(int64)}, out
}
func number(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		value, err := n.Int64()
		if err != nil {
			panic(err)
		}
		return value
	}
	panic(fmt.Sprintf("number: %T", v))
}
func claimRequest(r Request, key, p, kind string) Request {
	r.Key = key
	r.Path = p
	r.Kind = kind
	return r
}
func touchRequest(r Request, key string, paths ...string) Request {
	r.ToolCallID = key
	r.Paths = paths
	return r
}

func TestFiftyConcurrentContenders(t *testing.T) {
	s, _, _ := testStore(t)
	requests := make([]Request, 50)
	for i := range requests {
		r, _ := session(t, s, i)
		requests[i] = claimRequest(r, "one", "shared/file", "exact")
	}
	var winners atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, r := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Call(context.Background(), "claim", r)
			if err == nil {
				winners.Add(1)
			} else {
				var e *Error
				if !errors.As(err, &e) || e.Code != "conflict" {
					failures.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 || failures.Load() != 0 {
		t.Fatalf("winners=%d unexpected errors=%d", winners.Load(), failures.Load())
	}
}
func TestClaimAtomicityBoundariesAndReplay(t *testing.T) {
	s, c, _ := testStore(t)
	a, ao := session(t, s, 1)
	b, _ := session(t, s, 2)
	repo := ao["repository_id"].(string)
	if err := s.SetRepositoryPolicy(context.Background(), repo, "folded", "deny"); err != nil {
		t.Fatal(err)
	}
	granted := call(t, s, "claim", claimRequest(a, "prefix", "SRC/", "prefix"))
	deny := touchRequest(b, "rename", "free/new")
	deny.RenameFrom = "sRc/Old"
	deny.RenameTo = "src/New"
	out := call(t, s, "touch", deny)
	if out["action"] != "deny" || !strings.Contains(out["message"].(string), "codex-host-w1") || !strings.Contains(out["message"].(string), "until") {
		t.Fatal(out)
	}
	list := call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	if len(list) != 1 {
		t.Fatalf("partial grant: %v", list)
	}
	call(t, s, "touch", touchRequest(b, "boundary", "src2/new"))
	self := touchRequest(a, "self", "src/a")
	call(t, s, "touch", self)
	list = call(t, s, "claims", Request{RepositoryID: repo})["claims"].([]map[string]any)
	if len(list) != 2 || list[0]["origin"] != "explicit" || list[0]["expires_at"] != nil {
		t.Fatal(list)
	}
	// Both rename endpoints grant atomically on success.
	success := touchRequest(a, "rename-ok", "another")
	success.RenameFrom = "old"
	success.RenameTo = "new"
	call(t, s, "touch", success)
	auto := touchRequest(a, "auto", "temporary")
	call(t, s, "touch", auto)
	before := call(t, s, "claims", Request{RepositoryID: repo})
	c.advance(time.Minute)
	call(t, s, "touch", auto)
	after := call(t, s, "claims", Request{RepositoryID: repo})
	x, _ := json.Marshal(before)
	y, _ := json.Marshal(after)
	if string(x) != string(y) {
		t.Fatalf("retry changed expiry: %s / %s", x, y)
	}
	changed := auto
	changed.Paths = []string{"other"}
	expectError(t, s, "touch", changed, "key_reused")
	c.advance(10 * time.Minute)
	expectError(t, s, "touch", auto, "lost")
	// Explicit claims on ignored paths still participate in explicit conflicts.
	ignored := call(t, s, "touch", touchRequest(a, "ignore", "go.sum", "CHANGELOG.md", "generated/types.go"))
	if len(ignored["ignored"].([]string)) != 3 {
		t.Fatal(ignored)
	}
	call(t, s, "claim", claimRequest(a, "lockfile", "go.sum", "exact"))
	expectError(t, s, "claim", claimRequest(b, "lockfile", "go.sum", "exact"), "conflict")
	release := a
	release.Fence = number(granted["fence"])
	release.Token = granted["token"].(string)
	call(t, s, "release", release)
	expectError(t, s, "claim", claimRequest(a, "prefix", "SRC/", "prefix"), "lost")
	for _, p := range []string{"../escape", "x/../escape", "/absolute", "x\\bad", "*.go", "C:/file", ""} {
		expectError(t, s, "claim", claimRequest(a, "invalid-"+p, p, "exact"), "bad_request")
	}
	wide := claimRequest(a, "wide", "", "prefix")
	expectError(t, s, "claim", wide, "bad_request")
	wide.RepoWide = true
	call(t, s, "claim", wide)
}
func TestLeasesFencesAndClientClock(t *testing.T) {
	s, c, _ := testStore(t)
	a, ao := session(t, s, 1)
	b, _ := session(t, s, 2)
	q := claimRequest(a, "claim", "file", "exact")
	out := call(t, s, "claim", q)
	wrong := b
	wrong.Fence = number(out["fence"])
	wrong.Token = out["token"].(string)
	expectError(t, s, "release", wrong, "conflict")
	bad := a
	bad.Fence = wrong.Fence
	bad.Token = "old-epoch-token"
	expectError(t, s, "release", bad, "conflict")
	hb := a
	hb.Source = "shim"
	hb.ClientPIDStartMS = 1 << 62
	call(t, s, "heartbeat", hb)
	c.wall = c.wall.Add(365 * 24 * time.Hour) // wall skew cannot expire or extend a monotonic execution lease
	call(t, s, "validate", a)
	c.tick += 60000
	expectError(t, s, "heartbeat", hb, "lost")
	expectError(t, s, "claim", q, "lost")
	replacement := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", SessionID: "1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root"})
	if replacement["execution_id"] == ao["execution_id"] || number(replacement["generation"]) != 2 {
		t.Fatal(replacement)
	}
	expectError(t, s, "release", bad, "lost")
}
func TestHeartbeatReattachesAfterRestart(t *testing.T) {
	s, c, file := testStore(t)
	a, _ := session(t, s, 1)
	b, _ := session(t, s, 2)
	call(t, s, "claim", claimRequest(a, "claim", "reserved", "exact"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	expectError(t, reopened, "validate", a, "lost")
	stale := a
	stale.Source, stale.Generation = "watch", a.Generation+1
	expectError(t, reopened, "heartbeat", stale, "lost")
	hb := a
	hb.Source = "watch"
	resumed := call(t, reopened, "heartbeat", hb)
	if claims := resumed["claims"].([]map[string]any); len(claims) != 1 {
		t.Fatalf("reattachment must return rotated claims: %v", resumed)
	}
	call(t, reopened, "validate", a)
	expectError(t, reopened, "claim", claimRequest(b, "blocked", "reserved", "exact"), "lost")
	// The same heartbeat cannot revive a session whose reservation has run out.
	if number(call(t, reopened, "heartbeat", hb)["lease_ms"]) != 60000 {
		t.Fatal("a reattached shim keeps the strong lease")
	}
	c.advance(121 * time.Second)
	_ = reopened.Close()
	reopened.lock = nil
	late, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = late.Close() }()
	c.advance(121 * time.Second)
	expectError(t, late, "heartbeat", hb, "lost")
}
func TestRestartRecoveryDoesNotExtend(t *testing.T) {
	s, c, file := testStore(t)
	a, _ := session(t, s, 1)
	b, _ := session(t, s, 2)
	claim := claimRequest(a, "claim", "reserved", "exact")
	token := call(t, s, "claim", claim)["token"].(string)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	reopened, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if reopened.lock != nil {
			_ = reopened.Close()
		}
	}()
	expectError(t, reopened, "validate", a, "lost")
	reattach := Request{Client: "codex", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", SessionID: "2", Generation: b.Generation}
	call(t, reopened, "sessions", reattach)
	expectError(t, reopened, "claim", claimRequest(b, "blocked", "reserved", "exact"), "conflict")
	// Recovery needs the same session AND generation.
	reattach.SessionID = "1"
	reattach.Generation = 0
	expectError(t, reopened, "sessions", reattach, "lost")
	c.advance(time.Minute)
	_ = reopened.Close()
	reopened.lock = nil
	reopened, err = Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(61 * time.Second)
	reattach.Generation = a.Generation
	expectError(t, reopened, "sessions", reattach, "lost")
	fresh := reattach
	fresh.Generation = 0
	next := call(t, reopened, "sessions", fresh)
	if number(next["generation"]) != 2 {
		t.Fatal(next)
	}
	// A live recovered generation retains claims, but invalidates complete old-epoch tokens.
	r := Request{ExecutionID: next["execution_id"].(string), Generation: 2}
	newclaim := call(t, reopened, "claim", claimRequest(r, "new", "reserved", "exact"))
	if number(newclaim["fence"]) <= 0 {
		t.Fatal(newclaim)
	}
	old := r
	old.Fence = number(newclaim["fence"])
	old.Token = token
	expectError(t, reopened, "release", old, "conflict")
}
func TestRecoverSameGeneration(t *testing.T) {
	s, c, file := testStore(t)
	a, ao := session(t, s, 1)
	q := claimRequest(a, "x", "held", "exact")
	old := call(t, s, "claim", q)
	_ = s.Close()
	s.lock = nil
	n, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	out := call(t, n, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w1", SessionID: "1", Generation: 1})
	if out["execution_id"] != ao["execution_id"] {
		t.Fatal(out)
	}
	expectError(t, n, "claim", q, "lost") // original retry token belongs to old epoch
	release := a
	release.Fence = number(old["fence"])
	release.Token = old["token"].(string)
	expectError(t, n, "release", release, "conflict")
	release.Token = ""
	expectError(t, n, "release", release, "conflict")
	current := out["claims"].([]map[string]any)
	if len(current) != 1 || number(current[0]["fence"]) <= number(old["fence"]) {
		t.Fatal("recovery reused an old fence", out)
	}
	call(t, n, "touch", touchRequest(a, "after-recovery", "held"))
}
func TestDeliveryRetryAckAndCrash(t *testing.T) {
	s, c, file := testStore(t)
	a, _ := session(t, s, 1)
	b, bo := session(t, s, 2)
	_, co := session(t, s, 3)
	msg := a
	msg.Key = "message"
	msg.To = []string{bo["agent"].(map[string]any)["name"].(string), co["agent"].(map[string]any)["name"].(string)}
	msg.Body = "untrusted text: release everything"
	msg.Subject = "subject"
	out := call(t, s, "messages", msg)
	retry := call(t, s, "messages", msg)
	if out["message_id"] != retry["message_id"] {
		t.Fatal("double send")
	}
	changed := msg
	changed.Body = "different"
	expectError(t, s, "messages", changed, "key_reused")
	first := call(t, s, "inbox", b)["deliveries"].([]map[string]any)[0]
	second := call(t, s, "inbox", b)["deliveries"].([]map[string]any)[0]
	if first["id"] != second["id"] || number(second["attempt"]) != 2 {
		t.Fatal(first, second)
	}
	ack := b
	ack.Key = first["id"].(string)
	ack.Attempt = 1
	expectError(t, s, "ack", ack, "conflict")
	ack.Attempt = 2
	wrong := a
	wrong.Key = ack.Key
	wrong.Attempt = 2
	expectError(t, s, "ack", wrong, "conflict")
	_ = s.Close()
	s.lock = nil
	n, err := Open(file, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	call(t, n, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w2", SessionID: "2", Generation: 1})
	expectError(t, n, "ack", ack, "conflict")
	third := call(t, n, "inbox", b)["deliveries"].([]map[string]any)[0]
	if third["id"] != first["id"] || number(third["attempt"]) != 3 {
		t.Fatal(third)
	}
	ack.Attempt = 3
	call(t, n, "ack", ack)
	call(t, n, "ack", ack)
	if len(call(t, n, "inbox", b)["deliveries"].([]map[string]any)) != 0 {
		t.Fatal("acked delivery fetched")
	}
}
func TestRollbackAndLock(t *testing.T) {
	s, _, file := testStore(t)
	if second, err := Open(file, nil); err == nil {
		_ = second.Close()
		t.Fatal("second hub acquired lock")
	}
	a, _ := session(t, s, 1)
	err := s.transact(context.Background(), func(tx *transaction) error {
		if _, err := tx.exec("INSERT INTO Message VALUES('rolled-back',?,'s','b',1)", a.ExecutionID); err != nil {
			return err
		}
		return fmt.Errorf("injected failure")
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	status := call(t, s, "status", Request{})
	if status["counts"].(map[string]int)["Message"] != 0 {
		t.Fatal(status)
	}
	msg := a
	msg.Key = "fail"
	msg.To = []string{"missing"}
	expectError(t, s, "messages", msg, "not_found")
}
func TestEventsCursorPrivacyReset(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, bo := session(t, s, 2)
	c, _ := session(t, s, 3)
	msg := a
	msg.To = []string{bo["agent"].(map[string]any)["name"].(string)}
	msg.Body = "PRIVATE-BODY"
	msg.Subject = "PRIVATE-SUBJECT"
	for i := range 5 {
		msg.Key = fmt.Sprint(i)
		call(t, s, "messages", msg)
	}
	req := b
	req.Limit = 2
	req.ConsumerID = "watch"
	page := call(t, s, "events", req)
	events := page["events"].([]map[string]any)
	if len(events) != 2 {
		t.Fatal(page)
	}
	req.After = number(events[1]["cursor"])
	rest := call(t, s, "events", req)["events"].([]map[string]any)
	if len(rest) != 2 || number(rest[0]["cursor"]) <= req.After {
		t.Fatal(rest)
	}
	req.After = number(rest[1]["cursor"])
	call(t, s, "events", req)
	req.After = 1
	call(t, s, "events", req)
	var stored int64
	err := s.transact(context.Background(), func(tx *transaction) error {
		return tx.row("SELECT seq FROM Cursor WHERE consumer_id=?", b.ExecutionID+":watch").Scan(&stored)
	})
	if err != nil || stored != number(rest[1]["cursor"]) {
		t.Fatalf("cursor regressed: %d %v", stored, err)
	}
	other := call(t, s, "events", c)
	if len(other["events"].([]map[string]any)) != 0 {
		t.Fatal(other)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "body") {
		t.Fatal(string(raw))
	}
	if err = s.PruneEvents(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	req.After = 0
	reset := call(t, s, "events", req)
	if reset["reset"] != true || reset["snapshot"].(map[string]any)["unread"] != 5 {
		t.Fatal(reset)
	}
	req.After = 5
	req.Stream = "different-stream"
	if call(t, s, "events", req)["reset"] != true {
		t.Fatal("missing stream reset")
	}
}
func TestBindingAliasesAndPolicy(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "codex", Host: "host", SessionID: "one", Cwd: "/one", Worktree: "/one", RemoteURL: "git@alias:repo", ClientPID: 42, ClientPIDStartMS: 99, SingleSession: false}
	one := call(t, s, "sessions", registration)
	bind := Request{Client: "codex", Host: "host", ClientPID: 42, ClientPIDStartMS: 99}
	expectError(t, s, "bind", bind, "unbound")
	registration.SessionID = "two"
	registration.Cwd = "/two"
	registration.Worktree = "/two"
	two := call(t, s, "sessions", registration)
	if one["repository_id"] == two["repository_id"] {
		t.Fatal("ambiguous URL merged")
	}
	registration.SessionID = "three"
	registration.Cwd = "/one"
	registration.Worktree = "/one"
	registration.SingleSession = true
	three := call(t, s, "sessions", registration)
	resolved := call(t, s, "bind", bind)
	if resolved["execution_id"] != three["execution_id"] {
		t.Fatal(resolved)
	}
	registration.SessionID = "four"
	call(t, s, "sessions", registration)
	expectError(t, s, "bind", bind, "ambiguous")
	bind.SessionID = "four"
	call(t, s, "bind", bind)
	a, ao := session(t, s, 10)
	b, _ := session(t, s, 11)
	if err := s.SetRepositoryPolicy(context.Background(), ao["repository_id"].(string), "sensitive", "ask"); err != nil {
		t.Fatal(err)
	}
	call(t, s, "claim", claimRequest(a, "holder", "x", "exact"))
	if call(t, s, "touch", touchRequest(b, "ask", "x"))["action"] != "deny" {
		t.Fatal("unsupported ask weakened")
	}
	// Root-only matches and contradictory remote roots do not auto-join.
	registration.SingleSession = false
	registration.SessionID = "root-only"
	registration.Cwd = "/root-only"
	registration.Worktree = "/root-only"
	registration.RootCommit = "root"
	registration.RemoteURL = ""
	local := call(t, s, "sessions", registration)
	if local["repository_id"] == ao["repository_id"] {
		t.Fatal("root-only merge")
	}
	registration.SessionID = "contradiction"
	registration.Cwd = "/contradiction"
	registration.Worktree = "/contradiction"
	registration.RemoteURL = "https://example.com/team/project.git"
	registration.RootCommit = "other"
	different := call(t, s, "sessions", registration)
	if different["repository_id"] == ao["repository_id"] {
		t.Fatal("contradictory merge")
	}
}
func TestParentAndSlots(t *testing.T) {
	s, _, _ := testStore(t)
	registration := Request{Client: "claude", Host: "host", SessionID: "parent", Cwd: "/repo", Worktree: "/repo"}
	parent := call(t, s, "sessions", registration)
	registration.SessionID = "child"
	registration.ParentSessionID = "parent"
	child := call(t, s, "sessions", registration)
	if child["agent"].(map[string]any)["name"] != "claude-host-repo-2" {
		t.Fatal(child)
	}
	a := Request{ExecutionID: parent["execution_id"].(string), Generation: 1}
	b := Request{ExecutionID: child["execution_id"].(string), Generation: 1}
	call(t, s, "claim", claimRequest(a, "parent", "file", "exact"))
	call(t, s, "claim", claimRequest(b, "child", "file", "exact"))
	call(t, s, "end", a)
	call(t, s, "end", a)
	expectError(t, s, "heartbeat", Request{ExecutionID: a.ExecutionID, Generation: 1, Source: "shim"}, "lost")
}

// Exit during a write transaction without Close: SQLite must retain the committed
// delivery/outbox and discard the uncommitted tail after the OS releases the lock.
func TestCrashRecovery(t *testing.T) {
	if file := os.Getenv("IRIS_CORE_CRASH_DB"); file != "" {
		s, err := Open(file, nil)
		if err != nil {
			os.Exit(20)
		}
		a, _ := session(t, s, 1)
		_, b := session(t, s, 2)
		msg := a
		msg.Key = "committed"
		msg.To = []string{b["agent"].(map[string]any)["name"].(string)}
		msg.Body = "durable"
		call(t, s, "messages", msg)
		_ = s.transact(context.Background(), func(tx *transaction) error {
			if _, err := tx.exec("INSERT INTO Message(id,sender_execution_id,subject,body,created_ms) VALUES('uncommitted',?,'subject','tail',1)", a.ExecutionID); err != nil {
				os.Exit(21)
			}
			os.Exit(27)
			return nil
		})
		os.Exit(22)
	}
	file := filepath.Join(t.TempDir(), "crash.db")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCrashRecovery$")
	cmd.Env = append(os.Environ(), "IRIS_CORE_CRASH_DB="+file)
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 27 {
		t.Fatalf("crash helper: %v", err)
	}
	s, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	counts := call(t, s, "status", Request{})["counts"].(map[string]int)
	if counts["Message"] != 1 || counts["Delivery"] != 1 {
		t.Fatal(counts)
	}
	out := call(t, s, "sessions", Request{Client: "codex", Host: "host", Cwd: "/repo/w2", SessionID: "2", Generation: 1})
	r := Request{ExecutionID: out["execution_id"].(string), Generation: 1}
	if len(call(t, s, "inbox", r)["deliveries"].([]map[string]any)) != 1 {
		t.Fatal("delivery lost on crash")
	}
	if len(call(t, s, "events", r)["events"].([]map[string]any)) != 1 {
		t.Fatal("committed outbox event lost")
	}
}

func TestOutboxNotificationFollowsCommit(t *testing.T) {
	s, _, _ := testStore(t)
	a, out := session(t, s, 1)
	agent := out["agent"].(map[string]any)["id"].(string)
	changed := s.Changes()
	err := s.transact(context.Background(), func(tx *transaction) error {
		if err := tx.event(agent, a.ExecutionID, "inbox_changed", "test"); err != nil {
			return err
		}
		select {
		case <-changed:
			t.Fatal("notification escaped uncommitted transaction")
		default:
		}
		return fmt.Errorf("rollback")
	})
	if err == nil {
		t.Fatal("failure not injected")
	}
	select {
	case <-changed:
		t.Fatal("rolled-back event notified")
	default:
	}
	if len(call(t, s, "events", a)["events"].([]map[string]any)) != 0 {
		t.Fatal("rolled-back event exposed")
	}
	if err = s.transact(context.Background(), func(tx *transaction) error { return tx.event(agent, a.ExecutionID, "inbox_changed", "committed") }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("committed event did not notify")
	}
	if len(call(t, s, "events", a)["events"].([]map[string]any)) != 1 {
		t.Fatal("notification preceded committed outbox")
	}
}

// A new monitor starts at the newest event and is told about the waiting mail. Asking
// again before the monitor has saved a cursor tells it again: the first connection may
// have failed before its reset frame went out (gap review A5). Once the monitor saves
// its cursor, which it does after printing, the mail behind it is not announced again.
func TestMonitorCursorAnnouncesWaitingMailUntilTheMonitorSavesItsCursor(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, br := session(t, s, 2)
	name := br["agent"].(map[string]any)["name"].(string)
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m", To: []string{name}, Body: "x"})
	first := call(t, s, "monitor_cursor", b)
	if number(first["cursor"]) == 0 || first["pending"] != 1 {
		t.Fatal(first)
	}
	again := call(t, s, "monitor_cursor", b)
	if number(again["cursor"]) != number(first["cursor"]) || again["pending"] != 1 {
		t.Fatalf("a cursor nobody saved hid the waiting mail: %v", again)
	}
	saved := b
	saved.ConsumerID, saved.After = "iris-watch", number(first["cursor"])
	call(t, s, "events", saved)
	after := call(t, s, "monitor_cursor", b)
	if number(after["cursor"]) != number(first["cursor"]) || after["pending"] != 0 {
		t.Fatal(after)
	}
}

func TestEventsMarkMailAlreadyFetched(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, br := session(t, s, 2)
	name := br["agent"].(map[string]any)["name"].(string)
	call(t, s, "messages", Request{ExecutionID: a.ExecutionID, Generation: a.Generation, Key: "m", To: []string{name}, Body: "x"})
	before := call(t, s, "events", b)
	if before["pending"] != 1 || before["events"].([]map[string]any)[0]["settled"] != nil {
		t.Fatal(before)
	}
	call(t, s, "inbox", b)
	after := call(t, s, "events", b)
	if after["pending"] != 0 || after["events"].([]map[string]any)[0]["settled"] != true {
		t.Fatal(after)
	}
}
