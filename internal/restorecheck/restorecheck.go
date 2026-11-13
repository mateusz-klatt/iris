// Package restorecheck tests a hub backup the way ADR 006 asks: on a copy, in a throwaway hub,
// never on the live database. It shows that a restored hub gives no session the authority it
// had at backup time, and that a monitor whose cursor is ahead of the restored history resets.
package restorecheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
)

// Run copies backup into the empty or missing directory scratch, starts a hub on the copy on a
// loopback port with a token of its own, and checks how the restored hub treats what it holds.
// It writes one line per check to out and fails if any check fails. Nothing outside scratch is
// changed: the backup is only read.
func Run(ctx context.Context, backup, scratch string, out io.Writer) error {
	return run(ctx, backup, scratch, "127.0.0.1:0", out)
}

func run(ctx context.Context, backup, scratch, listen string, out io.Writer) error {
	db, err := prepare(backup, scratch)
	if err != nil {
		return err
	}
	active, sample, err := activeSessions(db)
	if err != nil {
		return err
	}
	backedUp, marked := restoreState(db)
	s, err := store.Open(db, nil)
	if err != nil {
		return fmt.Errorf("open the copy: %w", err)
	}
	defer func() { _ = s.Close() }()
	c := &checker{out: out}

	recovering, _ := scalar(db, "SELECT COUNT(*) FROM Execution WHERE state='active' AND recovery_deadline IS NOT NULL") // the copy just opened
	c.check(recovering == active, "%d of %d sessions active at backup time are recovering, none live", recovering, active)

	token := randomToken()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: hub.New(s, token, []string{listener.Addr().String()}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	// Plain HTTP on purpose: this is a throwaway hub on a loopback port, for this run only,
	// with a token of its own, and nothing leaves the machine.
	base := "http://" + listener.Addr().String()

	var status struct {
		DatabaseBytes int64  `json:"database_bytes"`
		Stream        string `json:"stream"`
	}
	c.check(call(ctx, base, token, "GET", "/v1/status", nil, &status) == nil, "the restored hub answers status (database %d bytes)", status.DatabaseBytes)
	// A backup made by `iris admin backup` carries a restore marker, and a hub restored from it
	// runs on a new stream (ADR 006). A copy made any other way, or by an older binary, keeps
	// the stream; that is not a failed check, but the operator should know.
	if marked {
		c.check(s.Restored() && status.Stream != "" && status.Stream != backedUp, "the backup carries a restore marker, and the restored hub runs on a new stream")
	} else {
		c.line("warn the backup has no restore marker, so the restored hub keeps its stream: a client that remembers a cursor of that stream can miss attention at or below it (ADR 006). Run `iris admin mark-restored` on the copy you restore, before its first start.")
	}

	if sample != nil {
		// A host that moved on since the backup presents a newer generation, which the copy has
		// never seen. It must not get the old Execution back.
		reg := map[string]any{"client": sample.client, "host": sample.host, "session_id": sample.session, "cwd": sample.worktree, "worktree": sample.worktree, "generation": sample.generation + 1, "source": "hook"}
		var got map[string]any
		err := call(ctx, base, token, "POST", "/v1/sessions", reg, &got)
		c.check(err != nil || got["execution_id"] != sample.id, "a session presenting a generation the backup does not have gets no restored authority")
	} else {
		c.line("no session was active at backup time, so there is no restored authority to test")
	}

	var fresh struct {
		ExecutionID string `json:"execution_id"`
	}
	// The monitor needs a session that is new to the copy. A fixed session id is not: the scratch
	// copy an earlier restore-check left holds that check's session, recovering after this start,
	// and a recovering session refuses a registration that brings no generation.
	monitor := map[string]any{"client": "codex", "host": "restore-check", "session_id": "restore-check-" + randomToken()[:16], "cwd": "/restore-check", "worktree": "/restore-check", "source": "hook"}
	if err := call(ctx, base, token, "POST", "/v1/sessions", monitor, &fresh); err != nil {
		c.check(false, "a new session registers on the restored hub (%v), so its monitor cannot be tested", err)
	} else {
		c.check(resets(ctx, base, token, fresh.ExecutionID), "a monitor whose cursor is ahead of the restored history gets a reset")
	}

	removed, err := s.Sweep(ctx)
	c.check(err == nil, "the first retention sweep on the copy would remove %d events, %d idempotency records, %d finished claims, %d Executions", removed["events"], removed["operations"], removed["claims"], removed["executions"])
	if c.failed > 0 {
		return fmt.Errorf("%d of the restore checks failed", c.failed)
	}
	return nil
}

type checker struct {
	out    io.Writer
	failed int
}

func (c *checker) line(format string, args ...any) { _, _ = fmt.Fprintf(c.out, format+"\n", args...) }

func (c *checker) check(ok bool, format string, args ...any) {
	mark := "ok  "
	if !ok {
		mark = "FAIL"
		c.failed++
	}
	c.line(mark+" "+format, args...)
}

// prepare refuses a scratch directory that holds anything, copies the backup into it, and
// returns the copy's path.
func prepare(backup, scratch string) (string, error) {
	info, err := os.Stat(backup)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("the backup is not a regular file")
	}
	if entries, err := os.ReadDir(scratch); err == nil && len(entries) > 0 {
		return "", errors.New("the scratch directory is not empty")
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(backup)
	if err != nil {
		return "", err
	}
	db := filepath.Join(scratch, "hub.db")
	return db, os.WriteFile(db, raw, 0o600)
}

type session struct {
	id, host, client, session, worktree string
	generation                          int64
}

// activeSessions counts the Executions active at backup time and returns one of them.
func activeSessions(path string) (int, *session, error) {
	n, err := scalar(path, "SELECT COUNT(*) FROM Execution WHERE state='active'")
	if err != nil {
		return 0, nil, fmt.Errorf("read the copy: %w", err)
	}
	if n == 0 {
		return 0, nil, nil
	}
	db, _ := sql.Open("sqlite", path) // the driver only records the name; the query reports errors
	defer func() { _ = db.Close() }()
	var s session
	err = db.QueryRow("SELECT id,host_id,client,session_id,worktree,generation FROM Execution WHERE state='active' LIMIT 1").Scan(&s.id, &s.host, &s.client, &s.session, &s.worktree, &s.generation)
	return n, &s, err
}

// scalar reads one number from the copy, on a connection of its own.
// restoreState reads, before the copy is opened, the stream it was backed up on and whether it
// carries a restore marker. A copy from before migration 025, without the marker's table,
// carries none.
func restoreState(path string) (stream string, marked bool) {
	db, _ := sql.Open("sqlite", path) // the driver only records the name; a failed read leaves the zero value
	defer func() { _ = db.Close() }()
	_ = db.QueryRow("SELECT stream_id FROM Meta WHERE id=1").Scan(&stream)
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM RestoreMarker").Scan(&n)
	return stream, n > 0
}

func scalar(path, query string) (int, error) {
	db, _ := sql.Open("sqlite", path) // the driver only records the name; the query reports errors
	defer func() { _ = db.Close() }()
	var n int
	err := db.QueryRow(query).Scan(&n)
	return n, err
}

// dialWatch opens the monitor's socket, and httpClient makes the other calls. Tests replace
// them to see a check fail.
var (
	dialWatch  = websocket.Dial
	httpClient = http.DefaultClient
)

func randomToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read always fills b or crashes the program.
	return hex.EncodeToString(b[:])
}

func call(ctx context.Context, base, token, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequestWithContext(ctx, method, base+path, reader) // method and URL are this package's own
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// resets opens a monitor with a cursor far beyond the restored history, as a monitor that
// lived on after the backup has, and reports whether its first frame is a reset.
func resets(ctx context.Context, base, token, execution string) bool {
	url := "ws" + strings.TrimPrefix(base, "http") + "/v1/watch?execution_id=" + execution + "&after=1000000000&stream=from-before-the-restore"
	dial, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := dialWatch(dial, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{"iris.v1"}})
	if err != nil {
		return false
	}
	defer func() { _ = conn.CloseNow() }()
	_, raw, err := conn.Read(dial)
	return err == nil && strings.Contains(string(raw), `"kind":"reset"`)
}
