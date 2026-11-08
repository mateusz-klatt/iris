package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"modernc.org/sqlite"
)

// The wrapper leaves transactions and results with SQLite. Only driver operations
// that can really fail (execution, query and stepping a result) can be interrupted.
// It models disk/driver failures without altering production SQL or schema.
var errStorageFault = errors.New("injected storage failure")

type storageFault struct {
	mu        sync.Mutex
	at, count int
	discover  bool
	trace     []string
}

func (f *storageFault) step(label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if label == "ROLLBACK" {
		return nil
	}
	f.count++
	f.trace = append(f.trace, label)
	if f.count == f.at || (f.discover && label == "COMMIT") {
		return errStorageFault
	}
	return nil
}

type faultDriver struct{ fault *storageFault }

func (d faultDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	if _, err = c.(driver.ExecerContext).ExecContext(context.Background(), "PRAGMA foreign_keys=ON", nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	return faultConn{Conn: c, fault: d.fault}, nil
}

type faultConn struct {
	driver.Conn
	fault *storageFault
}

func (c faultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.fault.step(q); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.fault.step(q); err != nil {
		return nil, err
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return faultRows{Rows: rows, fault: c.fault}, nil
}

type faultRows struct {
	driver.Rows
	fault *storageFault
}

func (r faultRows) Next(values []driver.Value) error {
	if err := r.fault.step("step result"); err != nil {
		return err
	}
	return r.Rows.Next(values)
}

var faultDriverNumber int

func installFaultDriver(t *testing.T, s *Store, file string) *storageFault {
	t.Helper()
	f := &storageFault{}
	name := registerFaultDriver(f)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(name, file)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s.db = db
	return f
}

func TestStorageFailuresRollBackAndRetry(t *testing.T) {
	for _, op := range []string{"sessions", "session-resume", "heartbeat-recovery", "session-new-slot", "session-learns-root", "touch-cached", "touch-conflict", "claim-conflict", "check-known", "policy", "bind", "heartbeat", "end", "end-handover", "touch", "claim", "claim-reuse", "claim-cached", "release", "claims", "messages", "message-repo", "contact", "message-cached", "inbox", "ack", "ack-handled", "inbox-receipt", "receipt-sweep", "receipts", "read_body", "agents", "agents-all", "hooks_seen", "events", "events-reset", "status", "validate", "monitor_cursor", "check", "check-alias", "session-alias", "reconcile", "reconcile-session", "ui_overview", "ui_threads", "ui_thread", "ui_read", "ui_messages", "ui_revoke", "ui_doctor_get", "ui_doctor_put", "ui_events", "expire", "sweep", "forget", "forget-repository"} {
		t.Run(op, func(t *testing.T) {
			s, clock, file := testStore(t)
			a, ar := session(t, s, 1)
			b, br := session(t, s, 2)
			repo := ar["repository_id"].(string)
			name := br["agent"].(map[string]any)["name"].(string)
			existing := call(t, s, "claim", claimRequest(a, "existing", "held", "exact"))
			mail := a
			mail.Key = "mail"
			mail.To = []string{name}
			mail.Body = "real persisted content"
			sent := call(t, s, "messages", mail)
			inbox := call(t, s, "inbox", b)["deliveries"].([]map[string]any)
			call(t, s, "ui_doctor_put", Request{CheckedAt: "2026-11-06", Clients: []map[string]any{{"status": "ok"}}})
			snap := a
			snap.Kind = "before"
			snap.Status = map[string]string{"file": " M"}
			call(t, s, "reconcile", snap)
			// b waits for a's "held", which a release frees, and for "dir", which a third
			// session still blocks, so a release both wakes a waiter and keeps one waiting.
			call(t, s, "touch", touchRequest(b, "wait-held", "held"))
			c, _ := session(t, s, 3)
			call(t, s, "touch", touchRequest(a, "dir-a", "dir/x"))
			call(t, s, "touch", touchRequest(c, "dir-c", "dir/y"))
			expectError(t, s, "claim", claimRequest(b, "wait-dir", "dir", "prefix"), "conflict")
			if op == "touch-conflict" {
				if err := s.SetRepositoryPolicy(context.Background(), repo, "sensitive", "warn"); err != nil {
					t.Fatal(err)
				}
			}
			route := op
			r := a
			switch op {
			case "sessions":
				r = Request{Client: "codex", Host: "new", SessionID: "new", Cwd: "/new", Worktree: "/new", RemoteURL: "https://new.example/repo.git"}
			case "session-learns-root":
				route = "sessions"
				call(t, s, "sessions", Request{Client: "codex", Host: "late", SessionID: "late", Cwd: "/late", Worktree: "/late", RemoteURL: "https://late.example/late.git"})
				r = Request{Client: "codex", Host: "late", SessionID: "late2", Cwd: "/late", Worktree: "/late", RemoteURL: "https://late.example/late.git", RootCommit: "late-root"}
			case "session-new-slot":
				route = "sessions"
				r = Request{Client: "codex", Host: "host", SessionID: "new-slot", Cwd: "/repo/w1", Worktree: "/repo/w1"}
			case "session-alias":
				// Another path to w1's directory, which the first path already named.
				route = "sessions"
				call(t, s, "sessions", Request{Client: "codex", Host: "host", SessionID: "alias-first", Cwd: "/repo/w1", Worktree: "/repo/w1", WorktreeID: "1:2"})
				r = Request{Client: "codex", Host: "host", SessionID: "alias-second", Cwd: "/mnt/w1/sub", Worktree: "/mnt/w1", WorktreeID: "1:2"}
			case "check-alias":
				route = "check"
				r = Request{Host: "host", Worktree: "/mnt/w1", WorktreeID: "1:3", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"held"}}
			case "check-known":
				route = "check"
				r = Request{Host: "host", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"held"}}
			case "policy":
				// Use an independent namespace whose policy is not pinned by claims.
				independent := call(t, s, "sessions", Request{Client: "codex", Host: "policy", SessionID: "policy", Cwd: "/policy", RemoteURL: "https://policy.example/policy.git"})
				r.RepositoryID = independent["repository_id"].(string)
			case "touch-cached":
				route = "touch"
				r = touchRequest(a, "cached-touch", "new")
				call(t, s, "touch", r)
			case "touch-conflict":
				route = "touch"
				r = touchRequest(b, "conflicting-touch", "held")
			case "claim-conflict":
				route = "claim"
				r = claimRequest(b, "conflicting-claim", "held", "exact")
			case "end-handover":
				// An MCP's stray Execution with mail ends, handing it to its process's hook's.
				route = "end"
				same := Request{Client: "codex", Host: "host", Cwd: "/repo/w1", Worktree: "/repo/w1", ClientPID: 6, ClientPIDStartMS: 6}
				hookReq, strayReq := same, same
				hookReq.SessionID, hookReq.Source = "handover-hook", "hook"
				strayReq.SessionID, strayReq.Source = "handover-stray", "mcp"
				hook := call(t, s, "sessions", hookReq)
				stray := call(t, s, "sessions", strayReq)
				note := a
				note.Key, note.To, note.Body = "handover", []string{stray["agent"].(map[string]any)["name"].(string)}, "stray mail"
				call(t, s, "messages", note)
				// One to both mailboxes, whose stray copy is settled rather than moved.
				both := a
				both.Key, both.To, both.Body = "handover-both", []string{stray["agent"].(map[string]any)["name"].(string), hook["agent"].(map[string]any)["name"].(string)}, "to both"
				call(t, s, "messages", both)
				r = Request{ExecutionID: stray["execution_id"].(string), Generation: stray["generation"].(int64), HandOver: hook["execution_id"].(string)}
			case "reconcile-session":
				// The combined form: the session registers and reconciles in one transaction.
				route = "reconcile"
				r = Request{Client: "codex", Host: "host", SessionID: "1", Cwd: "/repo/w1", Worktree: "/repo/w1", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Source: "hook",
					Generation: a.Generation, Kind: "after", Status: map[string]string{"file": "MM", "dir/x": " M"}}
			case "session-resume":
				route = "sessions"
				r = Request{Client: "codex", Host: "host", SessionID: "1", Cwd: "/repo/w1", Worktree: "/repo/w1"}
			case "bind":
				r = Request{Client: "codex", Host: "host", SessionID: "1"}
			case "heartbeat":
				r.Source = "shim"
			case "heartbeat-recovery":
				route = "heartbeat"
				r.Source = "watch"
				if _, err := s.db.Exec("UPDATE Execution SET recovery_deadline=? WHERE id=?", clock.Now().UnixMilli()+60000, a.ExecutionID); err != nil {
					t.Fatal(err)
				}
			case "touch":
				r = touchRequest(a, "new-touch", "held", "new")
			case "claim":
				r = claimRequest(a, "new-claim", "new", "exact")
			case "claim-reuse":
				route = "claim"
				r = claimRequest(a, "reuse", "held", "exact")
			case "claim-cached":
				route = "claim"
				r = claimRequest(a, "existing", "held", "exact")
			case "release":
				r.All = true
			case "agents-all":
				route = "agents"
				r = Request{All: true}
			case "claims", "agents":
				r.RepositoryID = repo
			case "messages":
				r = mail
				r.Key = "new-mail"
			case "message-repo":
				// By repository name, which resolves to the repository's contact.
				route = "messages"
				call(t, s, "contact", Request{RepositoryID: repo, Agent: name})
				r = mail
				r.Key, r.To = "repo-mail", []string{"repo:project"}
			case "contact":
				r = Request{RepositoryID: repo, Agent: name}
			case "message-cached":
				route = "messages"
				r = mail
			case "inbox":
				r = b
			case "ack":
				r = b
				r.Key = inbox[0]["id"].(string)
				r.Attempt = number(inbox[0]["attempt"])
			case "ack-handled":
				// An accepted delivery, said handled afterwards (ADR 005).
				route = "ack"
				r = b
				r.Key = inbox[0]["id"].(string)
				r.Attempt = number(inbox[0]["attempt"])
				call(t, s, "ack", r)
				r.Handled = true
			case "hooks_seen":
				r = Request{Host: "host"}
			case "read_body":
				r = b
				r.Key = inbox[0]["id"].(string)
			case "receipts":
				r = a
				r.Key = sent["message_id"].(string)
			case "events":
				r = b
				r.ConsumerID = "reader"
			case "events-reset":
				route = "events"
				r = b
				r.Stream = "old-stream"
				r.ConsumerID = "reader"
			case "check":
				r = Request{Host: "elsewhere", Cwd: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", Paths: []string{"held"}}
			case "reconcile":
				// dir/x is a's own automatic claim, renewed; dir/y is c's, so it is checked
				// against the family and left alone.
				r.Kind = "after"
				r.Status = map[string]string{"file": "MM", "dir/x": " M", "dir/y": " M"}
			case "ui_threads":
				r = Request{RepositoryID: repo, Agent: name, Q: "persisted", Before: "9999999999999"}
			case "ui_read":
				// The operator reads a thread with human mail in it.
				human := mail
				human.Key, human.To = "to-human", []string{"human"}
				r = Request{Key: call(t, s, "messages", human)["thread_id"].(string)}
			case "ui_thread":
				r.Key = sent["thread_id"].(string)
			case "ui_messages":
				r = Request{Key: "human", To: []string{name}, ReplyTo: sent["message_id"].(string), Body: "operator reply"}
			case "ui_revoke":
				r.Fence = number(existing["fence"])
			case "inbox-receipt":
				// b fetched its mail asking for a receipt; this is the receipt.
				route = "inbox_receipt"
				asked := b
				asked.Receipt = true
				got := call(t, s, "inbox", asked)["deliveries"].([]map[string]any)
				r = b
				for _, d := range got {
					r.Receipts = append(r.Receipts, Receipt{ID: d["id"].(string), Attempt: d["attempt"].(int64)})
				}
			case "receipt-sweep":
				// b's fetch asked for a receipt that never came; the sweep wakes it once.
				route = "status"
				asked := b
				asked.Receipt = true
				call(t, s, "inbox", asked)
				clock.advance(61 * 1000 * 1000 * 1000) // past the 60 s receipt window, inside b's lease
				r = Request{}
			case "expire":
				route = "status"
				clock.advance(1000000000000)
			case "sweep":
				// Old enough for every rule, so each of its deletes has rows to remove.
				call(t, s, "end", c)
				clock.advance(31 * 24 * 3600 * 1000 * 1000 * 1000) // 31 days
			case "forget":
				// b holds no claims, so once its session ends it can go, mail and all.
				call(t, s, "end", b)
				r = Request{Agent: name}
			case "forget-repository":
				route = "forget"
				other := call(t, s, "sessions", Request{Client: "codex", Host: "gone", SessionID: "gone", Cwd: "/gone", RemoteURL: "https://gone.example/gone.git"})
				call(t, s, "end", Request{ExecutionID: other["execution_id"].(string), Generation: other["generation"].(int64)})
				r = Request{RepositoryID: other["repository_id"].(string)}
			}
			invoke := func() (map[string]any, error) {
				if op == "policy" {
					return nil, s.SetRepositoryPolicy(context.Background(), r.RepositoryID, "sensitive", "deny")
				}
				if op == "sweep" {
					_, err := s.Sweep(context.Background())
					return nil, err
				}
				return s.Call(context.Background(), route, r)
			}
			f := installFaultDriver(t, s, file)
			// Discover the real statement/step sequence, forcing COMMIT to fail so the
			// discovery itself also proves atomicity and leaves the fixture unchanged.
			f.discover = true
			// A refused claim commits the waiter it leaves, so it too reports the failed COMMIT.
			if _, err := invoke(); !errors.Is(err, errStorageFault) {
				t.Fatalf("discovery: %v", err)
			}
			trace := append([]string(nil), f.trace...)
			f.discover = false
			f.at = 0
			before := databaseState(t, s)
			for i, label := range trace {
				if label == "ROLLBACK" {
					continue
				}
				f.count = 0
				f.at = i + 1
				f.trace = nil
				_, err := invoke()
				f.at = 0
				if !errors.Is(err, errStorageFault) {
					// Some domain lookups deliberately map storage errors to a typed failure.
					var domain *Error
					if !errors.As(err, &domain) {
						t.Fatalf("fault %d (%s) was not reported: %v", i+1, label, err)
					}
				}
				if after := databaseState(t, s); after != before {
					t.Fatalf("fault %d (%s) exposed partial state", i+1, label)
				}
			}
			f.at = 0
			f.count = 0
			if _, err := invoke(); op == "claim-conflict" {
				var domain *Error
				if !errors.As(err, &domain) || domain.Code != "conflict" {
					t.Fatal("overlap was not refused on retry", err)
				}
			} else if err != nil {
				t.Fatal("retry after transient storage failure:", err)
			}
		})
	}
}

// Keep the wrapper's EOF behavior explicit: an empty SQLite result remains empty.
func TestFaultDriverPreservesEOF(t *testing.T) {
	s, _, file := testStore(t)
	installFaultDriver(t, s, file)
	rows, err := s.db.Query("SELECT name FROM HostToken")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("empty result changed", rows.Err())
	}
}

func registerFaultDriver(f *storageFault) string {
	faultDriverNumber++
	name := "iris-fault-" + strings.Repeat("x", faultDriverNumber)
	sql.Register(name, faultDriver{f})
	return name
}
func TestMigrationStorageFailuresAreAtomic(t *testing.T) {
	for _, old := range []bool{false, true} {
		file := filepath.Join(t.TempDir(), "migration.db")
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		version := 0
		if old {
			for _, q := range []string{schema, threadsSchema, `INSERT INTO Repository(id,root_commit) VALUES('remote','root'),('local','root')`, `INSERT INTO RepositoryAlias VALUES('remote:example.com/team/iris','remote'),('local:host:/work/local','local')`} {
				if _, err = raw.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			version = 2
		}
		_ = raw.Close()
		f := &storageFault{discover: true}
		name := registerFaultDriver(f)
		if s, err := openDatabase(file, nil, name); !errors.Is(err, errStorageFault) {
			if s != nil {
				_ = s.Close()
			}
			t.Fatal("migration discovery", err)
		}
		trace := append([]string(nil), f.trace...)
		f.discover = false
		for i, label := range trace {
			f.count = 0
			f.at = i + 1
			f.trace = nil
			s, err := openDatabase(file, nil, name)
			f.at = 0
			if !errors.Is(err, errStorageFault) {
				if s != nil {
					_ = s.Close()
				}
				t.Fatalf("migration fault %d %s: %v", i+1, label, err)
			}
			// Every failure releases the OS lock and rolls back schema/data changes.
			raw, err = sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			var got int
			if err = raw.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != version {
				t.Fatal("partial migration", got, err)
			}
			_ = raw.Close()
		}
		f.at = 0
		s, err := openDatabase(file, nil, name)
		if err != nil {
			t.Fatal("migration retry", err)
		}
		_ = s.Close()
	}
}
func TestOpenFilesystemFailuresReleaseOwnership(t *testing.T) {
	dir := t.TempDir()
	// A directory cannot be opened as the writable SQLite file.
	if s, err := Open(dir, nil); err == nil {
		_ = s.Close()
		t.Fatal("directory accepted as database")
	}
	path := filepath.Join(t.TempDir(), "blocked.db")
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path, nil); err == nil {
		_ = s.Close()
		t.Fatal("invalid lock file accepted")
	}
	if s, err := openDatabase(filepath.Join(t.TempDir(), "unknown.db"), nil, "unregistered-iris-driver"); err == nil {
		_ = s.Close()
		t.Fatal("unknown driver accepted")
	}

}

func TestCorruptSnapshotAndUnencodableDoctorReportAreRejected(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	if _, err := s.db.Exec("INSERT INTO ShellSnapshot VALUES(?,?)", a.ExecutionID, "{"); err != nil {
		t.Fatal(err)
	}
	r := a
	r.Kind = "after"
	if _, err := s.Call(context.Background(), "reconcile", r); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	call(t, s, "ui_doctor_put", Request{Clients: []map[string]any{{"status": "ok"}}})
	// JSON encoding failure is reachable through the Go API's arbitrary metadata.
	if _, err := s.Call(context.Background(), "ui_doctor_put", Request{Clients: []map[string]any{{"status": "ok", "invalid": make(chan int)}}}); err == nil {
		t.Fatal("unencodable doctor metadata accepted")
	}
}
