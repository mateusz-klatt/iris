//go:build compat

package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type historicalRef struct {
	name          string
	commit        string
	schemaVersion int
}

// historicalRefs pins the released and stage-1 baselines exercised by cross-version upgrade
// and control checks (ADR 039 plan §1 pinning table).
var historicalRefs = []historicalRef{
	{name: "v0.1.15", commit: "6deecd0c26d9bfdd9bf01dfd376acc23739cd26f", schemaVersion: 28},
	{name: "stage1a", commit: "02ed40ec73102fb178298dbc5057b9729d2da592", schemaVersion: 29},
	{name: "m1", commit: "90546a36ce0ea340d5b47776be1574851f6f45cd", schemaVersion: 29},
}

var (
	historicalWorktreeMu  sync.Mutex
	historicalWorktreeSeq atomic.Int64
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func buildHistoricalBinary(t *testing.T, root, outDir string, ref historicalRef) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), fmt.Sprintf("wt-%s-%d-%d", ref.name, os.Getpid(), historicalWorktreeSeq.Add(1)))
	var removeOnce sync.Once
	removeWorktree := func() {
		removeOnce.Do(func() {
			historicalWorktreeMu.Lock()
			defer historicalWorktreeMu.Unlock()
			_, _ = exec.Command("git", "-C", root, "worktree", "remove", "--force", wt).CombinedOutput()
		})
	}
	t.Cleanup(removeWorktree)
	defer removeWorktree()

	historicalWorktreeMu.Lock()
	out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", wt, ref.commit).CombinedOutput()
	historicalWorktreeMu.Unlock()
	if err != nil {
		t.Fatalf("git worktree add %s (%s): %v\n%s", ref.name, ref.commit, err, out)
	}

	bin := filepath.Join(outDir, "iris-"+ref.name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-o", bin, "./cmd/iris")
	cmd.Dir = wt
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CGO_ENABLED=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build historical iris %s (%s): %v\n%s", ref.name, ref.commit, err, out)
	}
	return bin
}

type historicalHub struct {
	baseURL string
	token   string
	client  *http.Client
	stopFn  func()
}

func startHistoricalHub(t *testing.T, bin, dbPath, tokenPath string) *historicalHub {
	t.Helper()
	cmd := exec.Command(bin, "serve", "--listen", "127.0.0.1:0", "--allowed-hosts", "historical.iris.test", "--db", dbPath, "--token-file", tokenPath)
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "IRIS_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}

	listenCh := make(chan string, 1)
	var logs bytes.Buffer
	var logsMu sync.Mutex
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			logsMu.Lock()
			logs.WriteString(line)
			logs.WriteByte('\n')
			logsMu.Unlock()
			if strings.HasPrefix(line, "Iris listening on ") {
				select {
				case listenCh <- strings.TrimSpace(strings.TrimPrefix(line, "Iris listening on ")):
				default:
				}
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() {
		<-drainDone
		waitCh <- cmd.Wait()
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			if cmd.Process == nil {
				return
			}
			if runtime.GOOS == "windows" {
				_ = cmd.Process.Kill()
			} else if err := cmd.Process.Signal(os.Interrupt); err != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case err := <-waitCh:
				if runtime.GOOS != "windows" && err != nil {
					t.Errorf("historical iris %s exited with error: %v", bin, err)
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Errorf("timed out stopping historical iris %s: %v", bin, <-waitCh)
			}
		})
	}
	t.Cleanup(stop)

	var addr string
	select {
	case addr = <-listenCh:
	case err := <-waitCh:
		logsMu.Lock()
		msg := logs.String()
		logsMu.Unlock()
		t.Fatalf("historical iris exited early (%v):\n%s", err, msg)
	case <-time.After(15 * time.Second):
		stop()
		logsMu.Lock()
		msg := logs.String()
		logsMu.Unlock()
		t.Fatalf("timed out waiting for historical iris listen line:\n%s", msg)
	}

	rawToken, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read hub token %s: %v", tokenPath, err)
	}
	return &historicalHub{
		baseURL: "http://" + addr,
		token:   strings.TrimSpace(string(rawToken)),
		client:  &http.Client{Timeout: 5 * time.Second},
		stopFn:  stop,
	}
}

func (h *historicalHub) post(t *testing.T, path string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "historical.iris.test"
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d want=%d body=%s", path, resp.StatusCode, wantStatus, respBody)
	}
	var out map[string]any
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &out); err != nil {
			t.Fatalf("decode %s body %q: %v", path, respBody, err)
		}
	}
	return out
}

func markRecoveryDeadlinePast(t *testing.T, dbPath, whereClause string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE Execution SET recovery_deadline=1 WHERE "+whereClause, args...); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalHubUpgradeAndChoiceAControl(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	bins := make(map[string]string, len(historicalRefs))
	for _, ref := range historicalRefs {
		bins[ref.name] = buildHistoricalBinary(t, root, binDir, ref)
	}

	for _, ref := range historicalRefs {
		t.Run(ref.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "hub.db")
			tokenPath := filepath.Join(dir, "hub.token")
			bin := bins[ref.name]

			// Run 1: seed genuine historical sessions through the historical hub's HTTP API and
			// establish Choice A absence on the historical hub as control.
			hub1 := startHistoricalHub(t, bin, dbPath, tokenPath)
			ctrlActive := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "claude", "host": "host", "session_id": "S-ctrl-active",
				"cwd": "/repo/w", "worktree": "/repo/w", "branch": "main", "source": "hook",
			}, http.StatusOK)
			ctrlAttach := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "claude", "host": "host", "session_id": "S-ctrl-active",
				"cwd": "/repo/w", "worktree": "/repo/w", "source": "mcp",
			}, http.StatusOK)
			if ctrlAttach["execution_id"] != ctrlActive["execution_id"] {
				t.Fatalf("old hub control did not attach non-hook g0 to active hook session: %+v vs %+v", ctrlAttach, ctrlActive)
			}

			realProc := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "cursor", "host": "host", "session_id": "real-proc",
				"cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
				"client_pid": 42, "client_pid_start_ms": 100, "single_session": true,
			}, http.StatusOK)
			hub1.post(t, "/v1/executions/"+realProc["execution_id"].(string)+"/end", map[string]any{
				"generation": realProc["generation"],
			}, http.StatusOK)
			synthOld := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "cursor", "host": "host", "session_id": "mcp-42-100",
				"cwd": "/repo/w", "worktree": "/repo/w", "source": "mcp",
				"client_pid": 42, "client_pid_start_ms": 100, "single_session": true,
			}, http.StatusOK)
			if synthOld["generation"] != float64(1) {
				t.Fatalf("old hub control rejected synthetic fallback after ended hook: %+v", synthOld)
			}

			procRefuse := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "cursor", "host": "host", "session_id": "real-proc-refuse",
				"cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
				"client_pid": 43, "client_pid_start_ms": 101, "single_session": true,
			}, http.StatusOK)
			hub1.post(t, "/v1/executions/"+procRefuse["execution_id"].(string)+"/end", map[string]any{
				"generation": procRefuse["generation"],
			}, http.StatusOK)

			claudeEnded := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "claude", "host": "host", "session_id": "S-claude-ended",
				"cwd": "/repo/w", "worktree": "/repo/w", "source": "hook",
				"client_pid": 77, "client_pid_start_ms": 770, "single_session": false,
			}, http.StatusOK)
			hub1.post(t, "/v1/executions/"+claudeEnded["execution_id"].(string)+"/end", map[string]any{
				"generation": claudeEnded["generation"],
			}, http.StatusOK)

			noHookOld := hub1.post(t, "/v1/sessions", map[string]any{
				"client": "claude", "host": "host", "session_id": "S-nohook",
				"cwd": "/repo/w", "worktree": "/repo/w", "source": "mcp",
			}, http.StatusOK)
			if noHookOld["generation"] != float64(1) {
				t.Fatalf("old hub failed hookless registration: %+v", noHookOld)
			}

			hub1.post(t, "/v1/sessions", map[string]any{
				"client": "codex", "host": "host", "session_id": "S-upg-live",
				"cwd": "/repo/w", "worktree": "/repo/w", "branch": "main", "source": "hook",
			}, http.StatusOK)
			hub1.post(t, "/v1/sessions", map[string]any{
				"client": "codex", "host": "host", "session_id": "S-upg-lost",
				"cwd": "/repo/w", "worktree": "/repo/w", "branch": "main", "source": "hook",
			}, http.StatusOK)
			hub1.stopFn()

			// Run 2: old hub's own startup expire() marks G=1 lost when its deadline passes, then
			// old hub creates unhooked G=2 ("mcp") on both upgraded-history sessions.
			markRecoveryDeadlinePast(t, dbPath, "session_id IN ('S-upg-live','S-upg-lost') AND generation=1")
			hub2 := startHistoricalHub(t, bin, dbPath, tokenPath)
			upgLiveG2 := hub2.post(t, "/v1/sessions", map[string]any{
				"client": "codex", "host": "host", "session_id": "S-upg-live",
				"cwd": "/repo/w", "worktree": "/repo/w", "branch": "main", "source": "mcp",
			}, http.StatusOK)
			upgLostG2 := hub2.post(t, "/v1/sessions", map[string]any{
				"client": "codex", "host": "host", "session_id": "S-upg-lost",
				"cwd": "/repo/w", "worktree": "/repo/w", "branch": "main", "source": "mcp",
			}, http.StatusOK)
			if upgLiveG2["generation"] != float64(2) || upgLostG2["generation"] != float64(2) {
				t.Fatalf("old hub did not create unhooked G=2 over lost G=1 hook: live=%+v lost=%+v", upgLiveG2, upgLostG2)
			}
			hub2.stopFn()

			// Run 3: old hub's startup expire() marks S-upg-lost G=2 lost cleanly too.
			markRecoveryDeadlinePast(t, dbPath, "id=?", upgLostG2["execution_id"])
			hub3 := startHistoricalHub(t, bin, dbPath, tokenPath)
			hub3.stopFn()

			raw, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			var preVersion int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&preVersion); err != nil {
				_ = raw.Close()
				t.Fatal(err)
			}
			_ = raw.Close()
			if preVersion != ref.schemaVersion {
				t.Fatalf("historical hub %s user_version=%d want=%d", ref.name, preVersion, ref.schemaVersion)
			}

			// Upgrade: open the real historical SQLite DB with the current Store.
			clock := &fakeClock{wall: time.Now()}
			s, err := Open(dbPath, clock)
			if err != nil {
				t.Fatalf("upgrade from %s: %v", ref.name, err)
			}
			defer func() { _ = s.Close() }()

			var postVersion, procIdx int
			if err := s.db.QueryRow("SELECT (SELECT user_version FROM pragma_user_version),(SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='execution_process_hook')").Scan(&postVersion, &procIdx); err != nil || postVersion != latestVersion || procIdx != 1 {
				t.Fatalf("upgraded DB from %s: user_version=%d (want %d) idx=%d err=%v", ref.name, postVersion, latestVersion, procIdx, err)
			}

			// Clear startup recovery on live rows so Choice A attach runs on active non-recovering rows too.
			call(t, s, "heartbeat", Request{ExecutionID: ctrlActive["execution_id"].(string), Generation: 1, Source: "shim"})
			call(t, s, "heartbeat", Request{ExecutionID: upgLiveG2["execution_id"].(string), Generation: 2, Source: "shim"})
			call(t, s, "heartbeat", Request{ExecutionID: synthOld["execution_id"].(string), Generation: 1, Source: "shim"})

			assertChoiceARefusedAllOps := func(req Request, wantHighest int64) {
				t.Helper()
				before := databaseState(t, s)
				for _, src := range []string{"mcp", "shim", "watch", ""} {
					for _, tc := range []struct {
						op string
						r  Request
					}{
						{"sessions", func() Request { x := req; x.Source = src; return x }()},
						{"sessions_outside", func() Request { x := req; x.Worktree = ""; x.Source = src; return x }()},
						{"reconcile", func() Request {
							x := req
							x.Source = src
							return full(x, "before", map[string]string{})
						}()},
						{"reconcile_scoped", func() Request {
							x := req
							x.Source = src
							return reduced(x, "before", map[string]string{}, "sub")
						}()},
					} {
						out, err := s.Call(t.Context(), tc.op, tc.r)
						if out != nil {
							t.Fatalf("%s %s(%q) unexpectedly bypassed Choice A after upgrade: %+v", ref.name, tc.op, src, out)
						}
						assertRegistrationLost(t, err, wantHighest)
					}
				}
				if databaseState(t, s) != before {
					t.Fatalf("%s Choice A refusal mutated upgraded DB state for %s", ref.name, req.SessionID)
				}
			}

			// Active hook history, live unhooked G+1 over lost G hook, lost unhooked G+1 over lost G hook,
			// live/expired synthetic over ended hook process, and fresh synthetic over ended hook process: all refused.
			assertChoiceARefusedAllOps(Request{Client: "claude", Host: "host", SessionID: "S-ctrl-active", Cwd: "/repo/w", Worktree: "/repo/w"}, 1)
			assertChoiceARefusedAllOps(Request{Client: "codex", Host: "host", SessionID: "S-upg-live", Cwd: "/repo/w", Worktree: "/repo/w"}, 2)
			assertChoiceARefusedAllOps(Request{Client: "codex", Host: "host", SessionID: "S-upg-lost", Cwd: "/repo/w", Worktree: "/repo/w"}, 2)
			assertChoiceARefusedAllOps(Request{Client: "cursor", Host: "host", SessionID: "mcp-42-100", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true}, 1)
			assertChoiceARefusedAllOps(Request{Client: "cursor", Host: "host", SessionID: "mcp-43-101", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 43, ClientPIDStartMS: 101, SingleSession: true}, 0)

			// Expire live active rows and verify insert path refusals too.
			clock.advance(16 * time.Minute)
			call(t, s, "expire", Request{})
			assertChoiceARefusedAllOps(Request{Client: "claude", Host: "host", SessionID: "S-ctrl-active", Cwd: "/repo/w", Worktree: "/repo/w"}, 1)
			assertChoiceARefusedAllOps(Request{Client: "cursor", Host: "host", SessionID: "mcp-42-100", Cwd: "/repo/w", Worktree: "/repo/w", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true}, 1)

			// Fresh native Claude Rung-2 with distinct session_id + single_session=false succeeds over old ended process history,
			// genuine hookless D6 session keeps succeeding, and genuine hooks attach/advance.
			claudeFresh := call(t, s, "sessions", Request{
				Client: "claude", Host: "host", SessionID: "S-claude-r2",
				Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp",
				ClientPID: 77, ClientPIDStartMS: 770, SingleSession: false,
			})
			if claudeFresh["generation"] != int64(1) {
				t.Fatalf("%s distinct native Claude Rung-2 failed after upgrade: %+v", ref.name, claudeFresh)
			}
			noHookUpgraded := call(t, s, "sessions", Request{
				Client: "claude", Host: "host", SessionID: "S-nohook",
				Cwd: "/repo/w", Worktree: "/repo/w", Source: "mcp",
			})
			if noHookUpgraded["generation"] != int64(2) {
				t.Fatalf("%s genuine no-hook session did not advance cleanly after expiry: %+v", ref.name, noHookUpgraded)
			}
			upgLiveHook := call(t, s, "sessions", Request{
				Client: "codex", Host: "host", SessionID: "S-upg-live",
				Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
				SuccessionProof: true,
			})
			if upgLiveHook["generation"] != int64(3) || upgLiveHook["hook_seen"] != true {
				t.Fatalf("%s hook did not advance S-upg-live: %+v", ref.name, upgLiveHook)
			}
			upgLostHook := call(t, s, "sessions", Request{
				Client: "codex", Host: "host", SessionID: "S-upg-lost",
				Cwd: "/repo/w", Worktree: "/repo/w", Branch: "main", Source: "hook",
				SuccessionProof: true,
			})
			if upgLostHook["generation"] != int64(3) || upgLostHook["hook_seen"] != true {
				t.Fatalf("%s hook did not advance S-upg-lost: %+v", ref.name, upgLostHook)
			}
		})
	}
}
