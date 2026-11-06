package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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
		c.Close()
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
	faultDriverNumber++
	name := "iris-fault-" + strings.Repeat("x", faultDriverNumber)
	sql.Register(name, faultDriver{f})
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
	for _, op := range []string{"sessions", "session-resume", "bind", "heartbeat", "end", "touch", "claim", "claim-reuse", "claim-cached", "release", "claims", "messages", "message-cached", "inbox", "ack", "agents", "events", "events-reset", "status", "validate", "monitor_cursor", "check", "reconcile", "ui_overview", "ui_threads", "ui_thread", "ui_messages", "ui_revoke", "ui_doctor_get", "ui_doctor_put", "ui_events", "expire"} {
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
			route := op
			r := a
			switch op {
			case "sessions":
				r = Request{Client: "codex", Host: "new", SessionID: "new", Cwd: "/new", Worktree: "/new", RemoteURL: "https://new.example/repo.git"}
			case "session-resume":
				route = "sessions"
				r = Request{Client: "codex", Host: "host", SessionID: "1", Cwd: "/repo/w1", Worktree: "/repo/w1"}
			case "bind":
				r = Request{Client: "codex", Host: "host", SessionID: "1"}
			case "heartbeat":
				r.Source = "shim"
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
			case "claims", "agents":
				r.RepositoryID = repo
			case "messages":
				r = mail
				r.Key = "new-mail"
			case "message-cached":
				route = "messages"
				r = mail
			case "inbox":
				r = b
			case "ack":
				r = b
				r.Key = inbox[0]["id"].(string)
				r.Attempt = number(inbox[0]["attempt"])
			case "events":
				r = b
				r.ConsumerID = "reader"
			case "events-reset":
				route = "events"
				r = b
				r.Stream = "old-stream"
				r.ConsumerID = "reader"
			case "check":
				r = Request{Host: "elsewhere", Cwd: "/other", Worktree: "/other", RemoteURL: "https://example.com/team/iris.git", RootCommit: "root", Paths: []string{"held"}}
			case "reconcile":
				r.Kind = "after"
				r.Status = map[string]string{"file": "MM"}
			case "ui_threads":
				r = Request{RepositoryID: repo, Agent: name, Q: "persisted", Before: "9999999999999"}
			case "ui_thread":
				r.Key = sent["thread_id"].(string)
			case "ui_messages":
				r = Request{Key: "human", To: []string{name}, ReplyTo: sent["message_id"].(string), Body: "operator reply"}
			case "ui_revoke":
				r.Fence = number(existing["fence"])
			case "expire":
				route = "status"
				clock.advance(1000000000000)
			}
			f := installFaultDriver(t, s, file)
			// Discover the real statement/step sequence, forcing COMMIT to fail so the
			// discovery itself also proves atomicity and leaves the fixture unchanged.
			f.discover = true
			if _, err := s.Call(context.Background(), route, r); !errors.Is(err, errStorageFault) {
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
				_, err := s.Call(context.Background(), route, r)
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
			if _, err := s.Call(context.Background(), route, r); err != nil {
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
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("empty result changed", rows.Err())
	}
}
