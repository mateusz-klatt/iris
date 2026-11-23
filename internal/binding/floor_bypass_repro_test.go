package binding

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
	_ "modernc.org/sqlite"
)

// These tests drive the generation-zero fallback through the real resolver and the real store.
// After SessionEnd the session keeps <hash>.ended and no <hash>. An unbound Rung 2 shim or
// watch must not obtain that same session's next generation. A Rung 3 shim that cannot see a
// session id follows D6: it binds a live hook Execution for its process, and otherwise registers
// mcp-<pid>-<start> without advancing the hook's session.

func TestFloorBypassAfterSessionEndBlocksRung2AndWatch(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		damage bool
	}{{"shim", false}, {"watch", false}, {"shim", true}, {"watch", true}} {
		name := tc.mode
		if tc.damage {
			name += "/damaged_marker"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			h := newRestartableBindingHub(t)
			cwd := t.TempDir()
			token := filepath.Join(t.TempDir(), "hub.token")
			if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			client := &hubclient.Client{Config: config{h.srv.URL}}
			getenv := func(key string) string {
				switch key {
				case "IRIS_TOKEN_FILE":
					return token
				case "CLAUDE_CODE_SESSION_ID":
					return "S"
				default:
					return ""
				}
			}
			payload := `{"session_id":"S","cwd":` + quoteJSON(cwd) + `}`
			run := func(event string) {
				t.Helper()
				out := (hook.Runner{
					ClientName: "claude", EventName: event, Stdin: strings.NewReader(payload),
					Hub: client, Getenv: getenv, Host: func() string { return "floor-host" },
					Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
					Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
				}).Run()
				if fails, err := hook.ReadRecentFailures(token, 10); err != nil || len(fails) != 0 {
					t.Fatalf("%s failures: %v %v\nstdout %s\nstderr %s", event, fails, err, out.Stdout, out.Stderr)
				}
			}
			run("SessionStart")
			run("SessionEnd")

			dir := filepath.Join(filepath.Dir(token), "sessions")
			proof, marker := sessionProof(t, dir)
			if proof != "" || marker != "1" {
				t.Fatalf("Forget left proof %q and ended marker %q", proof, marker)
			}
			markerPath := sessionEndMarkerPath(t, dir)
			if tc.damage {
				if err := os.WriteFile(markerPath, []byte("damaged\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rows := executionRows(t, h.file)
			if len(rows) != 1 || rows[0].state != "ended" || rows[0].generation != 1 || rows[0].session != "S" {
				t.Fatalf("SessionEnd did not leave generation 1 ended: %+v", rows)
			}
			endedID := rows[0].id

			sessions := hook.NewSessions(token)
			var recalls []string
			r := &Resolver{
				Hub: client, Client: "claude", Host: "floor-host", CWD: cwd, Grace: 0,
				Getenv: getenv, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
				Recall: func(clientName, session string) (int, bool, error) {
					generation, present, err := sessions.RecallBindingGeneration(clientName, session)
					recalls = append(recalls, fmt.Sprintf("%s/%s -> %d present=%t err=%v", clientName, session, generation, present, err))
					return generation, present, err
				},
			}
			if tc.mode == "watch" {
				r.HeartbeatSource = "watch"
			}
			before := len(h.sessions())
			b, err := r.Resolve(ctx, "")
			requests := h.sessions()[before:]
			rows = executionRows(t, h.file)
			if err == nil || b.ExecutionID != "" || !IsCode(err, "lost") {
				t.Fatalf("%s obtained an Execution after SessionEnd: bound=%+v err=%v\nrecall %v\nrequests %+v\nrows %+v", tc.mode, b, err, recalls, requests, rows)
			}
			switch {
			case tc.damage:
				if !errors.Is(err, errMalformedGeneration) || len(requests) != 0 {
					t.Fatalf("damaged marker did not block locally: err=%v requests=%+v", err, requests)
				}
				assertBindingObservation(t, token, hook.DamagedGenerationProof+" at "+markerPath)
			case tc.mode == "watch":
				if !errors.Is(err, errGenerationFloor) || len(requests) != 0 {
					t.Fatalf("watch did not stop at the local floor: err=%v requests=%+v", err, requests)
				}
			default:
				if len(requests) != 1 || requests[0].Source != "shim" || requests[0].Generation != 1 || requests[0].RetryGeneration != 0 || requests[0].SessionID != "S" {
					t.Fatalf("shim did not stop at exact-generation reattach: %+v", requests)
				}
				var hubErr *hubclient.Error
				if !errors.As(err, &hubErr) {
					t.Fatalf("shim did not surface the hub refusal: %v", err)
				}
				highest, valid, present := hubErr.HighestGeneration()
				if !present || !valid || highest < 1 {
					t.Fatalf("shim lost answer omitted the generation floor: %v", err)
				}
			}
			if len(rows) != 1 || rows[0].id != endedID || rows[0].state != "ended" || rows[0].generation != 1 {
				t.Fatalf("%s changed the ended session: %+v\nrequests %+v", tc.mode, rows, requests)
			}
		})
	}
}

func TestFloorBypassUnhookedRung3RegistersSyntheticSession(t *testing.T) {
	ctx := t.Context()
	h := newRestartableBindingHub(t)
	cwd := t.TempDir()
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pid, start = 42, 100
	anc := proc.Ancestor{PID: pid, StartMS: start, Name: "cursor-agent"}
	sessions := hook.NewSessions(token)
	var recalls []string
	r := rung3Resolver(t, h, cwd, anc, func(clientName, session string) (int, bool, error) {
		generation, present, err := sessions.RecallBindingGeneration(clientName, session)
		recalls = append(recalls, fmt.Sprintf("%s/%s -> %d present=%t err=%v", clientName, session, generation, present, err))
		return generation, present, err
	})
	b, err := r.Resolve(ctx, "")
	requests := h.sessions()
	rows := executionRows(t, h.file)
	wantID := fmt.Sprintf("mcp-%d-%d", pid, start)
	if err != nil || b.SessionID != wantID || b.Generation != 1 || b.Rung != 3 || !r.own {
		t.Fatalf("unhooked rung 3 fallback: bound=%+v own=%t err=%v\nrecall %v\nrows %+v", b, r.own, err, recalls, rows)
	}
	if len(requests) != 1 || requests[0].SessionID != wantID || requests[0].Source != "mcp" || requests[0].Generation != 0 || requests[0].RetryGeneration != 0 || !requests[0].SingleSession {
		t.Fatalf("synthetic registration: %+v", requests)
	}
	if len(rows) != 1 || rows[0].session != wantID || rows[0].generation != 1 || rows[0].state != "active" || rows[0].hookSeen != 0 {
		t.Fatalf("synthetic execution: %+v", rows)
	}
	wantRecall := "cursor/" + wantID + " -> 0 present=false err=<nil>"
	if len(recalls) != 2 || recalls[0] != wantRecall || recalls[1] != wantRecall {
		t.Fatalf("floor was not checked against the synthetic session before and after registration: %v", recalls)
	}
}

func TestFloorBypassRung3BindsTheLiveHookSession(t *testing.T) {
	ctx := t.Context()
	h, cwd, token, anc := endedOrLiveCursor(t, false)
	hookRows := executionRows(t, h.file)
	if len(hookRows) != 1 || hookRows[0].state != "active" || hookRows[0].generation != 1 || hookRows[0].hookSeen == 0 {
		t.Fatalf("hook session did not register: %+v", hookRows)
	}
	sessions := hook.NewSessions(token)
	r := rung3Resolver(t, h, cwd, anc, sessions.RecallBindingGeneration)
	before := len(h.sessions())
	b, err := r.Resolve(ctx, "")
	if err != nil || b.ExecutionID != hookRows[0].id || b.Generation != 1 || b.Rung != 3 || b.SessionID != "real-session" {
		t.Fatalf("live hook session: bound=%+v err=%v", b, err)
	}
	if requests := h.sessions()[before:]; len(requests) != 0 {
		t.Fatalf("live lookup registered: %+v", requests)
	}
	if rows := executionRows(t, h.file); len(rows) != 1 || rows[0].id != hookRows[0].id {
		t.Fatalf("live lookup changed executions: %+v", rows)
	}
}

func TestFloorBypassRung3AfterEndRefusesSyntheticSession(t *testing.T) {
	ctx := t.Context()
	h, cwd, token, anc := endedOrLiveCursor(t, true)
	dir := filepath.Join(filepath.Dir(token), "sessions")
	proof, marker := sessionProof(t, dir)
	rows := executionRows(t, h.file)
	if proof != "" || marker != "1" || len(rows) != 1 || rows[0].state != "ended" || rows[0].session != "real-session" || rows[0].generation != 1 {
		t.Fatalf("setup proof %q marker %q rows %+v", proof, marker, rows)
	}
	endedID := rows[0].id
	sessions := hook.NewSessions(token)
	var recalls []string
	r := rung3Resolver(t, h, cwd, anc, func(clientName, session string) (int, bool, error) {
		generation, present, err := sessions.RecallBindingGeneration(clientName, session)
		recalls = append(recalls, fmt.Sprintf("%s/%s -> %d present=%t err=%v", clientName, session, generation, present, err))
		return generation, present, err
	})
	before := len(h.sessions())
	b, err := r.Resolve(ctx, "")
	requests := h.sessions()[before:]
	rows = executionRows(t, h.file)
	// Choice A refuses the synthetic generation-zero registration over hook process history.
	wantID := "mcp-42-100"
	if b.ExecutionID != "" || !IsCode(err, "lost") || r.own {
		t.Fatalf("rung 3 fallback after SessionEnd: bound=%+v own=%t err=%v\nrecall %v\nrequests %+v\nrows %+v", b, r.own, err, recalls, requests, rows)
	}
	if len(requests) != 1 || requests[0].SessionID != wantID || requests[0].Source != "mcp" || requests[0].Generation != 0 || requests[0].RetryGeneration != 0 {
		t.Fatalf("synthetic registration: %+v", requests)
	}
	var hubErr *hubclient.Error
	if !errors.As(err, &hubErr) {
		t.Fatalf("synthetic refusal is not a hub error: %v", err)
	}
	if highest, valid, present := hubErr.HighestGeneration(); highest != 0 || !valid || !present {
		t.Fatalf("synthetic session acquired a generation floor: highest=%d valid=%t present=%t", highest, valid, present)
	}
	if len(rows) != 1 || rows[0].id != endedID || rows[0].state != "ended" || rows[0].generation != 1 || rows[0].session != "real-session" {
		t.Fatalf("synthetic refusal changed the ended session: %+v", rows)
	}
	for _, recall := range recalls {
		if strings.Contains(recall, "real-session") {
			t.Fatalf("fallback checked the hook session: %v", recalls)
		}
	}
}

func TestFloorBypassRetainedProofBlocksALostSession(t *testing.T) {
	ctx := t.Context()
	h := newRestartableBindingHub(t)
	cwd := t.TempDir()
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &hubclient.Client{Config: config{h.srv.URL}}
	getenv := func(key string) string {
		switch key {
		case "IRIS_TOKEN_FILE":
			return token
		case "CLAUDE_CODE_SESSION_ID":
			return "S"
		default:
			return ""
		}
	}
	payload := `{"session_id":"S","cwd":` + quoteJSON(cwd) + `}`
	out := (hook.Runner{
		ClientName: "claude", EventName: "SessionStart", Stdin: strings.NewReader(payload),
		Hub: client, Getenv: getenv, Host: func() string { return "floor-host" },
		Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
	}).Run()
	if fails, err := hook.ReadRecentFailures(token, 10); err != nil || len(fails) != 0 {
		t.Fatalf("SessionStart failures: %v %v\nstdout %s\nstderr %s", fails, err, out.Stdout, out.Stderr)
	}
	proof, marker := sessionProof(t, filepath.Join(filepath.Dir(token), "sessions"))
	rows := executionRows(t, h.file)
	if proof == "" || marker != "" || len(rows) != 1 || rows[0].state != "active" || rows[0].generation != 1 {
		t.Fatalf("setup proof %q marker %q rows %+v", proof, marker, rows)
	}
	original := rows[0].id
	// The hook lease is 15 minutes. With the stream sweep armed, stall credit holds it
	// until lease_renewed_tick_ms + lease + 60s, so the loss is the same 16 minute jump
	// the reattach tests use.
	h.clock.Advance(16 * time.Minute)
	sessions := hook.NewSessions(token)
	r := &Resolver{
		Hub: client, Client: "claude", Host: "floor-host", CWD: cwd, Grace: 0,
		Getenv: getenv, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Recall: sessions.RecallBindingGeneration,
	}
	before := len(h.sessions())
	b, err := r.Resolve(ctx, "")
	requests := h.sessions()[before:]
	// Proof G makes the shim reattach at G. The hub's registration transaction expires the
	// lease and then refuses a non-zero generation, so the answer is lost with
	// highest_generation. That refusal rolls the expiry back with the rest of the
	// transaction; a later committed expire is what records the loss. Neither call may
	// open another generation.
	if b.ExecutionID != "" || !IsCode(err, "lost") {
		t.Fatalf("retained proof allowed a binding: bound=%+v err=%v\nrequests %+v", b, err, requests)
	}
	var hubErr *hubclient.Error
	if !errors.As(err, &hubErr) {
		t.Fatalf("retained proof did not surface the hub refusal: %v", err)
	}
	highest, valid, present := hubErr.HighestGeneration()
	if !present || !valid || highest < 1 {
		t.Fatalf("retained proof refusal omitted the generation floor: %v", err)
	}
	if len(requests) != 1 || requests[0].Source != "shim" || requests[0].Generation != 1 || requests[0].RetryGeneration != 0 || requests[0].SessionID != "S" {
		t.Fatalf("retained proof did not stop at one exact-generation reattach: %+v", requests)
	}
	if _, err = h.store.Call(ctx, "expire", store.Request{}); err != nil {
		t.Fatal(err)
	}
	rows = executionRows(t, h.file)
	if len(rows) != 1 || rows[0].id != original || rows[0].state != "lost" || rows[0].generation != 1 {
		t.Fatalf("lost session changed: %+v", rows)
	}
}

func endedOrLiveCursor(t *testing.T, end bool) (*restartableBindingHub, string, string, proc.Ancestor) {
	t.Helper()
	h := newRestartableBindingHub(t)
	cwd := t.TempDir()
	token := filepath.Join(t.TempDir(), "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	anc := proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}
	client := &hubclient.Client{Config: config{h.srv.URL}}
	getenv := func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	payload := `{"session_id":"real-session","cwd":` + quoteJSON(cwd) + `}`
	for _, event := range []string{"sessionStart", "sessionEnd"} {
		if event == "sessionEnd" && !end {
			break
		}
		out := (hook.Runner{
			ClientName: "cursor", EventName: event, Stdin: strings.NewReader(payload),
			Hub: client, Getenv: getenv, Host: func() string { return "floor-host" },
			Ancestor: func() (proc.Ancestor, error) { return anc, nil },
			Find:     func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
		}).Run()
		if fails, err := hook.ReadRecentFailures(token, 10); err != nil || len(fails) != 0 {
			t.Fatalf("%s failures: %v %v\nstdout %s\nstderr %s", event, fails, err, out.Stdout, out.Stderr)
		}
	}
	return h, cwd, token, anc
}

func rung3Resolver(t *testing.T, h *restartableBindingHub, cwd string, anc proc.Ancestor, recall func(string, string) (int, bool, error)) *Resolver {
	t.Helper()
	return &Resolver{
		Hub: &hubclient.Client{Config: config{h.srv.URL}}, Client: "cursor", Host: "floor-host", CWD: cwd, Grace: 0,
		Getenv: func(string) string { return "" }, Ancestor: func() (proc.Ancestor, error) { return anc, nil },
		Recall: recall,
	}
}

func quoteJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func sessionEndMarkerPath(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".ended") && sessionFileName(strings.TrimSuffix(entry.Name(), ".ended")) {
			return filepath.Join(dir, entry.Name())
		}
	}
	t.Fatal("SessionEnd left no ended marker")
	return ""
}

func assertBindingObservation(t *testing.T, token, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		notes, err := hook.ReadRecentObservations(token, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(notes) > 0 {
			if len(notes) != 1 || notes[0].Client != "claude" || notes[0].Event != "bind" || notes[0].Error != want {
				t.Fatalf("binding observations: %+v, want %q", notes, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("binding observation %q was not recorded", want)
		}
		time.Sleep(time.Millisecond)
	}
}

func sessionProof(t *testing.T, dir string) (proof, marker string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		base := strings.TrimSuffix(name, ".ended")
		if !sessionFileName(base) {
			continue
		}
		if strings.HasSuffix(name, ".ended") {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			marker = strings.TrimSpace(string(raw))
			continue
		}
		proof = name
	}
	return proof, marker
}

func sessionFileName(name string) bool {
	if len(name) != 32 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type executionRow struct {
	id, session, state   string
	generation, hookSeen int64
}

func executionRows(t *testing.T, file string) []executionRow {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id, session_id, generation, state, COALESCE(hook_seen_ms,0) FROM Execution ORDER BY generation, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []executionRow
	for rows.Next() {
		var row executionRow
		if err = rows.Scan(&row.id, &row.session, &row.generation, &row.state, &row.hookSeen); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
