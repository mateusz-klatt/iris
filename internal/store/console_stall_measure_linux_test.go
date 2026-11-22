//go:build linux

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type consoleMeasureConfig struct {
	SizeMB            int    `json:"size_target_mb"`
	CacheMode         string `json:"cache_mode"`
	OutPath           string `json:"out_path"`
	WorkDir           string `json:"work_dir"`
	Label             string `json:"label,omitempty"`
	Repos             int    `json:"repos"`
	Agents            int    `json:"agents"`
	Threads           int    `json:"threads"`
	MessagesPerThread int    `json:"messages_per_thread"`
	BodyBytes         int    `json:"body_bytes"`
	AckedPercent      int    `json:"acked_percent"`
	HumanThreadPct    int    `json:"human_thread_percent"`
	Worker            bool   `json:"-"`
	WorkerOp          string `json:"-"`
	WorkerDB          string `json:"-"`
	WorkerExecID      string `json:"-"`
}

func parseConsoleMeasureConfig(t *testing.T) (consoleMeasureConfig, bool) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("IRIS_MEASURE_CONSOLE"))
	if raw == "" {
		return consoleMeasureConfig{}, false
	}
	cfg := consoleMeasureConfig{
		SizeMB:            40,
		CacheMode:         "warm",
		Repos:             4,
		Agents:            17,
		MessagesPerThread: 16,
		BodyBytes:         2048,
		AckedPercent:      96,
		HumanThreadPct:    85,
	}
	msgsExplicit := os.Getenv("IRIS_MEASURE_CONSOLE_MSGS_PER_THREAD") != ""
	bodyExplicit := os.Getenv("IRIS_MEASURE_CONSOLE_BODY_BYTES") != ""
	if raw != "1" {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				t.Fatalf("invalid IRIS_MEASURE_CONSOLE token %q (expected 1 or key=value,...)", part)
			}
			key := strings.TrimSpace(k)
			if key == "messages_per_thread" {
				msgsExplicit = true
			}
			if key == "body_bytes" {
				bodyExplicit = true
			}
			applyMeasureKV(t, &cfg, key, strings.TrimSpace(v))
		}
	}
	envOverrides := map[string]string{
		"size_mb":             "IRIS_MEASURE_CONSOLE_SIZE_MB",
		"cache":               "IRIS_MEASURE_CONSOLE_CACHE",
		"out":                 "IRIS_MEASURE_CONSOLE_OUT",
		"dir":                 "IRIS_MEASURE_CONSOLE_DIR",
		"label":               "IRIS_MEASURE_CONSOLE_LABEL",
		"repos":               "IRIS_MEASURE_CONSOLE_REPOS",
		"agents":              "IRIS_MEASURE_CONSOLE_AGENTS",
		"threads":             "IRIS_MEASURE_CONSOLE_THREADS",
		"messages_per_thread": "IRIS_MEASURE_CONSOLE_MSGS_PER_THREAD",
		"body_bytes":          "IRIS_MEASURE_CONSOLE_BODY_BYTES",
		"acked_percent":       "IRIS_MEASURE_CONSOLE_ACKED_PCT",
		"human_thread_pct":    "IRIS_MEASURE_CONSOLE_HUMAN_PCT",
	}
	for k, envName := range envOverrides {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			applyMeasureKV(t, &cfg, k, v)
		}
	}
	if os.Getenv("IRIS_MEASURE_CONSOLE_WORKER") == "1" {
		cfg.Worker = true
		cfg.WorkerOp = os.Getenv("IRIS_MEASURE_CONSOLE_OP")
		cfg.WorkerDB = os.Getenv("IRIS_MEASURE_CONSOLE_DB")
		cfg.WorkerExecID = os.Getenv("IRIS_MEASURE_CONSOLE_EXEC")
		if cfg.WorkerOp == "" || cfg.WorkerDB == "" || cfg.OutPath == "" {
			t.Fatal("worker requires IRIS_MEASURE_CONSOLE_OP, IRIS_MEASURE_CONSOLE_DB, IRIS_MEASURE_CONSOLE_OUT")
		}
		return cfg, true
	}
	if cfg.CacheMode != "warm" && cfg.CacheMode != "cold" {
		t.Fatalf("cache mode must be 'warm' or 'cold', got %q", cfg.CacheMode)
	}
	if cfg.SizeMB <= 0 {
		t.Fatalf("size_mb must be positive, got %d", cfg.SizeMB)
	}
	if cfg.Threads <= 0 {
		// Ensure at least 80 threads even in small 2-4 MB smoke runs so ui_threads(limit=50)
		// fills a full page and exercises grouping/sorting past the page limit.
		cfg.Threads = max(80, cfg.SizeMB*18)
	}
	if cfg.SizeMB <= 8 {
		if !msgsExplicit {
			cfg.MessagesPerThread = 6
		}
		if !bodyExplicit {
			cfg.BodyBytes = 768
		}
	}
	if cfg.BodyBytes <= 0 {
		cfg.BodyBytes = 2048
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	if cfg.OutPath == "" {
		cfg.OutPath = filepath.Join(cfg.WorkDir, fmt.Sprintf("console-stall-%dmb-%s.json", cfg.SizeMB, cfg.CacheMode))
	}
	return cfg, true
}

func applyMeasureKV(t *testing.T, cfg *consoleMeasureConfig, k, v string) {
	t.Helper()
	atoi := func() int {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("invalid integer for %s=%q: %v", k, v, err)
		}
		return n
	}
	switch k {
	case "size_mb":
		cfg.SizeMB = atoi()
	case "cache":
		cfg.CacheMode = v
	case "out":
		cfg.OutPath = v
	case "dir":
		cfg.WorkDir = v
	case "label":
		cfg.Label = v
	case "repos":
		cfg.Repos = atoi()
	case "agents":
		cfg.Agents = atoi()
	case "threads":
		cfg.Threads = atoi()
	case "messages_per_thread":
		cfg.MessagesPerThread = atoi()
	case "body_bytes":
		cfg.BodyBytes = atoi()
	case "acked_percent":
		cfg.AckedPercent = atoi()
	case "human_thread_pct":
		cfg.HumanThreadPct = atoi()
	default:
		t.Fatalf("unknown IRIS_MEASURE_CONSOLE parameter %q", k)
	}
}

type rusageDelta struct {
	UserMS  float64 `json:"user_ms"`
	SysMS   float64 `json:"sys_ms"`
	TotalMS float64 `json:"total_ms"`
	Minflt  int64   `json:"minflt"`
	Majflt  int64   `json:"majflt"`
	Inblock int64   `json:"inblock"`
	Oublock int64   `json:"oublock"`
	Nvcsw   int64   `json:"nvcsw"`
	Nivcsw  int64   `json:"nivcsw"`
}

func readSelfRusage(t testing.TB) syscall.Rusage {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return ru
}

func diffRusage(before, after syscall.Rusage) rusageDelta {
	user := timevalSubMS(after.Utime, before.Utime)
	sys := timevalSubMS(after.Stime, before.Stime)
	return rusageDelta{
		UserMS:  user,
		SysMS:   sys,
		TotalMS: user + sys,
		Minflt:  after.Minflt - before.Minflt,
		Majflt:  after.Majflt - before.Majflt,
		Inblock: after.Inblock - before.Inblock,
		Oublock: after.Oublock - before.Oublock,
		Nvcsw:   after.Nvcsw - before.Nvcsw,
		Nivcsw:  after.Nivcsw - before.Nivcsw,
	}
}

func timevalSubMS(after, before syscall.Timeval) float64 {
	us := (after.Sec-before.Sec)*1_000_000 + (after.Usec - before.Usec)
	return float64(us) / 1000.0
}

type opBreakdown struct {
	BeginMS             float64 `json:"begin_ms"`
	ExpireMS            float64 `json:"expire_ms"`
	QueryMS             float64 `json:"query_ms"`
	CommitMS            float64 `json:"commit_ms"`
	RollbackMS          float64 `json:"rollback_ms,omitempty"`
	NonSQLGoMS          float64 `json:"non_sql_go_ms"`
	ThreadsMainMS        float64 `json:"threads_main_query_ms,omitempty"`
	ThreadsUnreadMS      float64 `json:"threads_unread_query_ms,omitempty"`
	ThreadSubjectCount   int     `json:"thread_subjects_count,omitempty"`
	ThreadSubjectTotalMS float64 `json:"thread_subjects_total_ms,omitempty"`
	ThreadSubjectMaxMS   float64 `json:"thread_subjects_max_ms,omitempty"`
	ThreadPartsCount     int     `json:"thread_participants_count,omitempty"`
	ThreadPartsTotalMS  float64 `json:"thread_participants_total_ms,omitempty"`
	ThreadPartsMaxMS    float64 `json:"thread_participants_max_ms,omitempty"`
	ThreadAuthorCount   int     `json:"thread_authors_count,omitempty"`
	ThreadAuthorTotalMS float64 `json:"thread_authors_total_ms,omitempty"`
	ThreadAuthorMaxMS   float64 `json:"thread_authors_max_ms,omitempty"`
	OverviewReposMS     float64 `json:"overview_repositories_ms,omitempty"`
	OverviewDetailsMS   float64 `json:"overview_repo_details_total_ms,omitempty"`
	OverviewAgentsMS    float64 `json:"overview_agents_ms,omitempty"`
	OverviewHumanMS     float64 `json:"overview_human_unread_ms,omitempty"`
}

type opMeasurement struct {
	Operation     string               `json:"operation"`
	CacheMode     string               `json:"cache_mode"`
	OpenWallMS    float64              `json:"open_wall_ms,omitempty"`
	OpenCPUMS     float64              `json:"open_cpu_ms,omitempty"`
	WallMS        float64              `json:"wall_ms"`
	CPUMS         rusageDelta          `json:"rusage"`
	MuWaitMS      float64              `json:"mu_wait_ms"`
	MuHeldMS      float64              `json:"mu_held_ms"`
	Error         string               `json:"error,omitempty"`
	ResultSummary map[string]any       `json:"result_summary,omitempty"`
	Breakdown     opBreakdown          `json:"breakdown"`
	Statements    []sqlStatementRecord `json:"statements"`
}

type queryPlanStep struct {
	ID     int    `json:"id"`
	Parent int    `json:"parent"`
	Detail string `json:"detail"`
}

type queryPlanReport struct {
	Name                 string          `json:"name"`
	SQL                  string          `json:"sql"`
	Steps                []queryPlanStep `json:"steps"`
	Scans                []string        `json:"scans"`
	Searches             []string        `json:"searches"`
	IndexesUsed          []string        `json:"indexes_used"`
	CorrelatedSubqueries []string        `json:"correlated_subqueries"`
	TempBTrees           []string        `json:"temp_btrees"`
}

type databaseSummary struct {
	Path         string         `json:"path"`
	FileBytes    int64          `json:"file_bytes"`
	WALBytes     int64          `json:"wal_bytes"`
	SHMBytes     int64          `json:"shm_bytes"`
	PageSize     int64          `json:"page_size"`
	PageCount    int64          `json:"page_count"`
	PragmaBytes  int64          `json:"pragma_bytes"`
	Pragmas      map[string]any `json:"pragmas"`
	Counts       map[string]int `json:"counts"`
	GenerationMS float64        `json:"generation_ms"`
}

type overview14kReport struct {
	Database     databaseSummary `json:"database"`
	Cold         opMeasurement   `json:"cold"`
	Warm         opMeasurement   `json:"warm"`
	OverviewPlan queryPlanReport `json:"overview_agents_plan"`
}

type consoleStallReport struct {
	Environment      map[string]string          `json:"environment"`
	CacheNotes       []string                   `json:"cache_notes"`
	Config           consoleMeasureConfig       `json:"config"`
	Database         databaseSummary            `json:"database"`
	Operations       []opMeasurement            `json:"operations"`
	WarmupOperations []opMeasurement            `json:"warmup_operations,omitempty"`
	ExplainPlans     map[string]queryPlanReport `json:"explain_query_plan"`
	Overview14kPoint *overview14kReport         `json:"overview_14k_point,omitempty"`
	LeaseConsequence *leaseConsequenceResult    `json:"lease_consequence,omitempty"`
	ReadNonBlocking  *readNonBlockingResult     `json:"read_non_blocking,omitempty"`
}

type syntheticSeedInfo struct {
	ActiveExecID string
	Summary      databaseSummary
}

func measureEnvironment(t *testing.T, dir string, cfg consoleMeasureConfig) map[string]string {
	t.Helper()
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	fsKind := fmt.Sprintf("0x%x", fs.Type)
	switch fs.Type {
	case 0xef53:
		fsKind = "ext4 (0xef53)"
	case 0x01021994:
		fsKind = "tmpfs (0x01021994)"
	}
	return map[string]string{
		"kernel":         strings.TrimSpace(string(release)),
		"filesystem":     fsKind,
		"cpus":           strconv.Itoa(runtime.NumCPU()),
		"gomaxprocs":     strconv.Itoa(runtime.GOMAXPROCS(0)),
		"go":             runtime.Version(),
		"dir":            dir,
		"cache_mode":     cfg.CacheMode,
		"size_target_mb": strconv.Itoa(cfg.SizeMB),
		"timestamp_utc":  time.Now().UTC().Format(time.RFC3339Nano),
	}
}

var synthVocab = []string{
	"sqlite", "wal", "checkpoint", "transaction", "mutex", "heartbeat", "lease", "expiry",
	"reconcile", "snapshot", "worktree", "repository", "delivery", "acknowledge", "thread",
	"participant", "operator", "console", "overview", "latency", "pagecache", "subquery",
	"correlated", "index", "covering", "trigger", "fts5", "execution", "generation",
	"claim", "fence", "prefix", "exact", "conflict", "release", "waiter", "stream",
	"cursor", "outbox", "session", "hook", "shim", "watch", "guard", "doctor", "token",
	"mailbox", "envelope", "receipt", "inflight", "pending", "handled", "unreachable",
}

func synthText(seed, targetBytes int) string {
	var b strings.Builder
	b.Grow(targetBytes + 64)
	state := uint64(seed)*6364136223846793005 + 1442695040888963407
	for b.Len() < targetBytes {
		state = state*6364136223846793005 + 1
		w1 := synthVocab[(state>>32)%uint64(len(synthVocab))]
		state = state*6364136223846793005 + 1
		w2 := synthVocab[(state>>32)%uint64(len(synthVocab))]
		fmt.Fprintf(&b, "%s/%s#%d ", w1, w2, (state>>16)&0xffff)
	}
	s := b.String()
	if len(s) > targetBytes {
		s = s[:targetBytes]
	}
	return s
}

func generateSyntheticConsoleDB(t *testing.T, dbPath string, cfg consoleMeasureConfig) syntheticSeedInfo {
	t.Helper()
	start := time.Now()
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	_ = os.Remove(dbPath + ".lock")

	s, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("open synthetic db: %v", err)
	}
	defer func() { _ = s.Close() }()

	now := time.Now().UnixMilli()
	var epoch int64
	if err := s.db.QueryRow("SELECT boot_epoch FROM Meta WHERE id=1").Scan(&epoch); err != nil {
		t.Fatal(err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	repos := make([]string, cfg.Repos)
	humans := make([]string, cfg.Repos)
	humanExecs := make([]string, cfg.Repos)
	for r := range cfg.Repos {
		repoID := fmt.Sprintf("rp_synth_%02d", r)
		repos[r] = repoID
		if _, err := tx.Exec(`INSERT INTO Repository(id,case_policy,root_commit,conflict_action,name) VALUES(?,'folded',?,'deny',?)`, repoID, fmt.Sprintf("root-%02d", r), fmt.Sprintf("project-%02d", r)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO RepositoryAlias(alias,repository_id) VALUES(?,?)`, fmt.Sprintf("remote:example.invalid/org/project-%02d", r), repoID); err != nil {
			t.Fatal(err)
		}
		humanID := "human_" + repoID
		humanExec := "operator_" + repoID
		humans[r] = humanID
		humanExecs[r] = humanExec
		if _, err := tx.Exec(`INSERT INTO Agent(id,name,repository_id,host,client,worktree,slot) VALUES(?,?,?,'operator','human','',1)`, humanID, humanID, repoID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session,last_seen_ms) VALUES(?,?,?,'operator','human',?,1,'','',?,0,'ended',0,0,0,?)`, humanExec, humanID, repoID, humanExec, epoch, now); err != nil {
			t.Fatal(err)
		}
	}

	clients := []string{"claude", "codex", "cursor", "copilot", "antigravity", "grok", "kimi"}
	hosts := []string{"wsl-dev", "mac-work", "linux-box"}
	type agentInfo struct {
		id         string
		repoIdx    int
		activeExec string
		endedExec  string
		hasActive  bool
	}
	agents := make([]agentInfo, cfg.Agents)
	for a := range cfg.Agents {
		repoIdx := a % cfg.Repos
		repoID := repos[repoIdx]
		client := clients[a%len(clients)]
		host := hosts[a%len(hosts)]
		agentID := fmt.Sprintf("ag_synth_%03d", a)
		agentName := fmt.Sprintf("%s-%s-wt%02d", client, host, a)
		worktree := fmt.Sprintf("/work/project-%02d/wt-%02d", repoIdx, a)
		if _, err := tx.Exec(`INSERT INTO Agent(id,name,repository_id,host,client,worktree,slot) VALUES(?,?,?,?,?,?,1)`, agentID, agentName, repoID, host, client, worktree); err != nil {
			t.Fatal(err)
		}
		endedID := fmt.Sprintf("ex_synth_old_%03d", a)
		if _, err := tx.Exec(`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session,last_seen_ms,hook_seen_ms) VALUES(?,?,?,?,?,?,1,?,'main',?,0,'ended',1000,1000,1,?,?)`,
			endedID, agentID, repoID, host, client, fmt.Sprintf("sess-old-%03d", a), worktree, epoch, now-3600_000, now-3600_000); err != nil {
			t.Fatal(err)
		}
		// Leave ~18% of agents offline/ended so ui_deliveries has realistic "unreachable" problems too.
		hasActive := a == 0 || (a%6 != 5)
		activeID := endedID
		if hasActive {
			activeID = fmt.Sprintf("ex_synth_live_%03d", a)
			if _, err := tx.Exec(`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session,heartbeat_mode,last_seen_ms,hook_seen_ms,watch_seen_ms) VALUES(?,?,?,?,?,?,2,?,'main',?,?, 'active',2000,2000,1,1,?,?,?)`,
				activeID, agentID, repoID, host, client, fmt.Sprintf("sess-live-%03d", a), worktree, epoch, 900_000, now, now, now); err != nil {
				t.Fatal(err)
			}
		}
		agents[a] = agentInfo{id: agentID, repoIdx: repoIdx, activeExec: activeID, endedExec: endedID, hasActive: hasActive}
	}

	// Host reports for ui_doctor_get
	const reportJSON = `{"checked_at":"2026-11-22T12:00:00Z","clients":[{"client":"claude","version":"2.1.319","status":"ok","last_hook_at":"2026-11-22T11:59:00Z"},{"client":"codex","version":"0.167.0","status":"ok"},{"client":"cursor","version":"2026.11","status":"ok"},{"client":"copilot","version":"1.0.109","status":"ok"},{"client":"antigravity","version":"1.5.9","status":"ok"},{"client":"grok","version":"1.0.77","status":"silent","detail":"synthetic log entry"},{"client":"kimi","version":"2.6.1","status":"not_configured"}]}`
	for _, host := range hosts {
		if _, err := tx.Exec(`INSERT INTO HostReport(host,report) VALUES(?,?)`, host, reportJSON); err != nil {
			t.Fatal(err)
		}
	}

	// Claims: 12 active and 200 historical revoked claims
	for c := range 212 {
		ag := agents[c%len(agents)]
		execID := ag.endedExec
		var revoked any = now - int64(c+1)*60_000
		if c < 12 && ag.hasActive {
			execID = ag.activeExec
			revoked = nil
		}
		path := fmt.Sprintf("internal/pkg%02d/file_%03d.go", c%10, c)
		if _, err := tx.Exec(`INSERT INTO Claim(execution_id,repository_id,kind,path,origin,boot_epoch,expires_ms,revoked_ms,path_key) VALUES(?,?,'exact',?,'auto',?,?,?,?)`,
			execID, repos[ag.repoIdx], path, epoch, now+600_000, revoked, path); err != nil {
			t.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	targetBytes := int64(cfg.SizeMB) * (1 << 20)
	historySpanMS := int64((60 * 24 * time.Hour) / time.Millisecond)
	recentWindowMS := int64((7 * 24 * time.Hour) / time.Millisecond)
	waitAgeMS := int64((20 * time.Minute) / time.Millisecond)

	msgSeq := 0
	dlvSeq := 0
	threadIdx := 0

	insertThreadBatch := func(batchThreads int) {
		batchTx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = batchTx.Rollback() }()

		stmtMsg, err := batchTx.Prepare(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id,reply_to) VALUES(?,?,?,?,?,?,?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stmtMsg.Close() }()

		stmtDlv, err := batchTx.Prepare(`INSERT INTO Delivery(id,message_id,recipient_agent_id,execution_id,attempt,attention) VALUES(?,?,?,?,?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stmtDlv.Close() }()

		stmtAck, err := batchTx.Prepare(`INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms,handled_ms) VALUES(?,?,?,?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stmtAck.Close() }()

		stmtEvt, err := batchTx.Prepare(`INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) VALUES('inbox_changed',?,NULL,?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stmtEvt.Close() }()

		for range batchThreads {
			th := threadIdx
			threadIdx++
			repoIdx := th % cfg.Repos
			repoID := repos[repoIdx]
			humanInvolved := ((th * 37) % 100) < cfg.HumanThreadPct
			// Put ~35% of threads in the last 7 days so recent window & unacked mail are realistic.
			isRecent := (th % 3) == 0
			baseAge := recentWindowMS + int64((th*7919)%int(historySpanMS-recentWindowMS))
			if isRecent {
				baseAge = waitAgeMS + int64((th*104729)%int(recentWindowMS-waitAgeMS))
			}
			threadStartMS := now - baseAge
			threadID := ""
			prevMsgID := ""

			// Pick 2 agents in repo (and occasionally a cross-repo agent).
			var repoAgents []agentInfo
			for _, ag := range agents {
				if ag.repoIdx == repoIdx {
					repoAgents = append(repoAgents, ag)
				}
			}
			a1 := repoAgents[th%len(repoAgents)]
			a2 := agents[(th+1)%len(agents)]
			if a2.id == a1.id {
				a2 = agents[(th+2)%len(agents)]
			}

			for m := range cfg.MessagesPerThread {
				msgSeq++
				msgID := fmt.Sprintf("ms_synth_%07d", msgSeq)
				if m == 0 {
					threadID = msgID
				}
				createdMS := threadStartMS + int64(m)*45_000
				if createdMS > now-60_000 {
					createdMS = now - 60_000 - int64(cfg.MessagesPerThread-m)*1000
				}

				humanSends := humanInvolved && (m%3 == 0)
				senderExec := a1.endedExec
				if m%2 == 1 {
					senderExec = a2.endedExec
				}
				if humanSends {
					senderExec = humanExecs[repoIdx]
				}
				subject := fmt.Sprintf("[%s] Thread %d task discussion #%d", repoID, th, m)
				// Vary body size around cfg.BodyBytes within <= 16 KiB CHECK constraint.
				bodyLen := min(16000, max(256, cfg.BodyBytes+(msgSeq%5-2)*(cfg.BodyBytes/4)))
				body := synthText(msgSeq, bodyLen)
				var replyTo any
				if prevMsgID != "" {
					replyTo = prevMsgID
				}
				if _, err := stmtMsg.Exec(msgID, senderExec, subject, body, createdMS, repoID, threadID, replyTo); err != nil {
					t.Fatal(err)
				}
				prevMsgID = msgID

				// Choose 1 or 2 recipients distinct from sender.
				type recChoice struct {
					agentID string
					execID  string
					isHuman bool
				}
				var recs []recChoice
				if humanSends {
					recs = append(recs, recChoice{a1.id, a1.endedExec, false})
					if m%4 == 0 {
						recs = append(recs, recChoice{a2.id, a2.endedExec, false})
					}
				} else {
					other := a2
					if m%2 == 1 {
						other = a1
					}
					recs = append(recs, recChoice{other.id, other.endedExec, false})
					if humanInvolved && m%2 == 1 {
						recs = append(recs, recChoice{humans[repoIdx], humanExecs[repoIdx], true})
					}
				}

				for _, rec := range recs {
					dlvSeq++
					dlvID := fmt.Sprintf("dl_synth_%08d", dlvSeq)
					// Minority unacked only in the last 7 days; older deliveries are acked.
					acked := !isRecent || ((dlvSeq % 100) < cfg.AckedPercent)
					if acked {
						if _, err := stmtDlv.Exec(dlvID, msgID, rec.agentID, rec.execID, 1, 0); err != nil {
							t.Fatal(err)
						}
						if _, err := stmtAck.Exec(dlvID, rec.execID, 1, createdMS+5_000, createdMS+10_000); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := stmtDlv.Exec(dlvID, msgID, rec.agentID, nil, 0, 1); err != nil {
							t.Fatal(err)
						}
						if _, err := stmtEvt.Exec(rec.agentID, dlvID, createdMS); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
		if err := batchTx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// Ensure recipient agents[0] always has a small batch of unacked pending mail for inbox().
	ensureInboxMail := func() {
		btx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = btx.Rollback() }()
		for k := range 5 {
			msgSeq++
			dlvSeq++
			msgID := fmt.Sprintf("ms_inbox_%07d", msgSeq)
			dlvID := fmt.Sprintf("dl_inbox_%08d", dlvSeq)
			created := now - int64(k+2)*60_000
			if _, err := btx.Exec(`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id,reply_to) VALUES(?,?,?,?,?,?,?,NULL)`,
				msgID, humanExecs[0], fmt.Sprintf("Pending inbox item %d", k), synthText(msgSeq, min(1024, cfg.BodyBytes)), created, repos[0], msgID); err != nil {
				t.Fatal(err)
			}
			if _, err := btx.Exec(`INSERT INTO Delivery(id,message_id,recipient_agent_id,execution_id,attempt,attention) VALUES(?,?,?,NULL,0,1)`,
				dlvID, msgID, agents[0].id); err != nil {
				t.Fatal(err)
			}
			if _, err := btx.Exec(`INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) VALUES('inbox_changed',?,NULL,?,?)`,
				agents[0].id, dlvID, created); err != nil {
				t.Fatal(err)
			}
		}
		if err := btx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// First populate the configured baseline threads, then top up in batches until targetBytes is reached.
	batchSize := 50
	for threadIdx < cfg.Threads {
		n := min(batchSize, cfg.Threads-threadIdx)
		insertThreadBatch(n)
	}
	ensureInboxMail()

	dbBytesNow := func() int64 {
		var pages, pageSize int64
		if err := s.db.QueryRow("SELECT page_count,page_size FROM pragma_page_count(),pragma_page_size()").Scan(&pages, &pageSize); err != nil {
			t.Fatal(err)
		}
		return pages * pageSize
	}
	for dbBytesNow() < targetBytes {
		insertThreadBatch(batchSize)
	}
	var busy, logFrames, moved int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &moved); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	rows, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() || rows.Err() != nil {
		_ = rows.Close()
		t.Fatalf("foreign_key_check failed: %v", rows.Err())
	}
	_ = rows.Close()

	counts := collectDatabaseCounts(t, s.db, now-recentWindowMS)
	genMS := durationMS(time.Since(start))
	summary := collectDatabaseSummary(t, dbPath, s.db, counts, genMS)
	return syntheticSeedInfo{
		ActiveExecID: agents[0].activeExec,
		Summary:      summary,
	}
}

func collectDatabaseCounts(t *testing.T, db *sql.DB, recentCutoffMS int64) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, item := range []struct {
		key string
		sql string
	}{
		{"Repository", "SELECT COUNT(*) FROM Repository"},
		{"Agent", "SELECT COUNT(*) FROM Agent"},
		{"NonHumanAgent", "SELECT COUNT(*) FROM Agent WHERE client<>'human'"},
		{"HumanAgent", "SELECT COUNT(*) FROM Agent WHERE client='human'"},
		{"Execution", "SELECT COUNT(*) FROM Execution"},
		{"ActiveExecution", "SELECT COUNT(*) FROM Execution WHERE state='active'"},
		{"Claim", "SELECT COUNT(*) FROM Claim"},
		{"ActiveClaim", "SELECT COUNT(*) FROM Claim WHERE revoked_ms IS NULL"},
		{"Message", "SELECT COUNT(*) FROM Message"},
		{"Threads", "SELECT COUNT(DISTINCT thread_id) FROM Message"},
		{"MessageSearch", "SELECT COUNT(*) FROM MessageSearch"},
		{"Delivery", "SELECT COUNT(*) FROM Delivery"},
		{"Ack", "SELECT COUNT(*) FROM Ack"},
		{"UnackedDelivery", "SELECT COUNT(*) FROM Delivery WHERE id NOT IN (SELECT delivery_id FROM Ack)"},
		{"HostReport", "SELECT COUNT(*) FROM HostReport"},
		{"Event", "SELECT COUNT(*) FROM Event"},
	} {
		var n int
		if err := db.QueryRow(item.sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", item.key, err)
		}
		counts[item.key] = n
	}
	var unackedRecent int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Delivery d JOIN Message m ON m.id=d.message_id WHERE d.id NOT IN (SELECT delivery_id FROM Ack) AND m.created_ms>=?`, recentCutoffMS).Scan(&unackedRecent); err != nil {
		t.Fatal(err)
	}
	counts["UnackedRecent7d"] = unackedRecent
	var humanThreads int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT m.thread_id) FROM Message m WHERE m.thread_id IN (SELECT mm.thread_id FROM Message mm JOIN Execution e ON e.id=mm.sender_execution_id JOIN Agent a ON a.id=e.agent_id WHERE a.client='human' UNION SELECT mm.thread_id FROM Message mm JOIN Delivery d ON d.message_id=mm.id JOIN Agent a ON a.id=d.recipient_agent_id WHERE a.client='human')`).Scan(&humanThreads); err != nil {
		t.Fatal(err)
	}
	counts["HumanThreads"] = humanThreads
	return counts
}

func collectDatabaseSummary(t *testing.T, dbPath string, db *sql.DB, counts map[string]int, genMS float64) databaseSummary {
	t.Helper()
	statSize := func(p string) int64 {
		info, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	var pageSize, pageCount int64
	if err := db.QueryRow("SELECT page_size,page_count FROM pragma_page_size(),pragma_page_count()").Scan(&pageSize, &pageCount); err != nil {
		t.Fatal(err)
	}
	pragmas := map[string]any{}
	for _, p := range []string{"cache_size", "journal_mode", "synchronous", "temp_store", "foreign_keys", "busy_timeout"} {
		var val any
		if err := db.QueryRow("PRAGMA " + p).Scan(&val); err != nil {
			t.Fatal(err)
		}
		pragmas[p] = val
	}
	return databaseSummary{
		Path:         dbPath,
		FileBytes:    statSize(dbPath),
		WALBytes:     statSize(dbPath + "-wal"),
		SHMBytes:     statSize(dbPath + "-shm"),
		PageSize:     pageSize,
		PageCount:    pageCount,
		PragmaBytes:  pageSize * pageCount,
		Pragmas:      pragmas,
		Counts:       counts,
		GenerationMS: genMS,
	}
}

func copyDBCold(t *testing.T, src, dst string) (fadviseNote string) {
	t.Helper()
	_ = os.Remove(dst)
	_ = os.Remove(dst + "-wal")
	_ = os.Remove(dst + "-shm")
	_ = os.Remove(dst + ".lock")

	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := unix.Fadvise(int(out.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		_ = out.Close()
		return fmt.Sprintf("fadvise(FADV_DONTNEED) failed: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return "fadvise(FADV_DONTNEED) succeeded on fresh copy"
}

func summarizeStatements(stmts []sqlStatementRecord, muHeldMS float64) opBreakdown {
	var b opBreakdown
	var sqlTotal float64
	for _, st := range stmts {
		sqlTotal += st.WallMS
		switch st.Label {
		case "begin_immediate", "begin_deferred":
			b.BeginMS += st.WallMS
		case "commit":
			b.CommitMS += st.WallMS
		case "rollback":
			b.RollbackMS += st.WallMS
		case "expire_executions", "expire_claims", "expire_missed_receipts", "expire_console_sessions":
			b.ExpireMS += st.WallMS
		default:
			b.QueryMS += st.WallMS
			switch st.Label {
			case "threads_main_query":
				b.ThreadsMainMS += st.WallMS
			case "threads_unread_query":
				b.ThreadsUnreadMS += st.WallMS
			case "thread_subject_query":
				b.ThreadSubjectCount++
				b.ThreadSubjectTotalMS += st.WallMS
				b.ThreadSubjectMaxMS = max(b.ThreadSubjectMaxMS, st.WallMS)
			case "thread_participants_query":
				b.ThreadPartsCount++
				b.ThreadPartsTotalMS += st.WallMS
				b.ThreadPartsMaxMS = max(b.ThreadPartsMaxMS, st.WallMS)
			case "thread_authors_query":
				b.ThreadAuthorCount++
				b.ThreadAuthorTotalMS += st.WallMS
				b.ThreadAuthorMaxMS = max(b.ThreadAuthorMaxMS, st.WallMS)
			case "overview_repositories":
				b.OverviewReposMS += st.WallMS
			case "overview_repo_access", "overview_repo_aliases", "overview_repo_claims":
				b.OverviewDetailsMS += st.WallMS
			case "overview_agents":
				b.OverviewAgentsMS += st.WallMS
			case "overview_human_unread":
				b.OverviewHumanMS += st.WallMS
			}
		}
	}
	b.NonSQLGoMS = max(0, muHeldMS-sqlTotal)
	return b
}

func summarizeResult(op string, out map[string]any) map[string]any {
	if out == nil {
		return nil
	}
	switch op {
	case "ui_threads":
		threads, _ := out["threads"].([]map[string]any)
		_, hasNext := out["next"]
		return map[string]any{"threads": len(threads), "has_next": hasNext}
	case "ui_overview":
		repos, _ := out["repositories"].([]map[string]any)
		agents, _ := out["agents"].([]map[string]any)
		claims, _ := out["claims"].([]map[string]any)
		return map[string]any{"repositories": len(repos), "agents": len(agents), "claims": len(claims), "human_unread": out["human_unread"]}
	case "ui_deliveries":
		msgs, _ := out["messages"].([]map[string]any)
		return map[string]any{"messages": len(msgs), "more": out["more"]}
	case "ui_doctor_get":
		hosts, _ := out["hosts"].([]map[string]any)
		return map[string]any{"hosts": len(hosts)}
	case "inbox":
		dlvs, _ := out["deliveries"].([]map[string]any)
		return map[string]any{"deliveries": len(dlvs), "more": out["more"]}
	case "heartbeat":
		return map[string]any{"lease_ms": out["lease_ms"], "generation": out["generation"]}
	default:
		return map[string]any{"keys": len(out)}
	}
}

func operationRequest(op, activeExecID string) Request {
	switch op {
	case "ui_threads":
		return Request{RepositoryID: "", Agent: "human", Limit: 50}
	case "ui_overview":
		return Request{}
	case "ui_deliveries":
		return Request{RepositoryID: ""}
	case "ui_doctor_get":
		return Request{}
	case "inbox":
		return Request{ExecutionID: activeExecID}
	case "heartbeat":
		return Request{ExecutionID: activeExecID, Generation: 2, Source: "shim"}
	case "expire":
		return Request{}
	default:
		return Request{}
	}
}

func measureStoreOperation(t *testing.T, s *Store, tr *sqlTracer, cacheMode, op, activeExecID string) (opMeasurement, []sqlStatementRecord) {
	t.Helper()
	req := operationRequest(op, activeExecID)
	var timing lockTiming
	ctx := withLockObserver(context.Background(), lockObserver{
		afterUnlock: func(wait, held time.Duration) {
			timing = lockTiming{Wait: wait, Held: held}
		},
	})
	tr.reset(true)
	ruBefore := readSelfRusage(t)
	start := time.Now()
	out, err := s.Call(ctx, op, req)
	wall := time.Since(start)
	ruAfter := readSelfRusage(t)
	stmts := tr.snapshot()
	tr.reset(false)

	heldOrWallMS := durationMS(timing.Held)
	if heldOrWallMS == 0 {
		heldOrWallMS = durationMS(wall)
	}
	m := opMeasurement{
		Operation:     op,
		CacheMode:     cacheMode,
		WallMS:        durationMS(wall),
		CPUMS:         diffRusage(ruBefore, ruAfter),
		MuWaitMS:      durationMS(timing.Wait),
		MuHeldMS:      durationMS(timing.Held),
		ResultSummary: summarizeResult(op, out),
		Breakdown:     summarizeStatements(stmts, heldOrWallMS),
		Statements:    stmts,
	}
	if err != nil {
		m.Error = err.Error()
	}
	return m, stmts
}

func analyzeQueryPlan(t *testing.T, db *sql.DB, name, query string, args []any) queryPlanReport {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %s: %v\nsql: %s", name, err, query)
	}
	defer func() { _ = rows.Close() }()

	report := queryPlanReport{
		Name:                 name,
		SQL:                  query,
		Steps:                []queryPlanStep{},
		Scans:                []string{},
		Searches:             []string{},
		IndexesUsed:          []string{},
		CorrelatedSubqueries: []string{},
		TempBTrees:           []string{},
	}
	seenIdx := map[string]bool{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		report.Steps = append(report.Steps, queryPlanStep{ID: id, Parent: parent, Detail: detail})
		if strings.HasPrefix(detail, "SCAN ") {
			report.Scans = append(report.Scans, detail)
		}
		if strings.HasPrefix(detail, "SEARCH ") {
			report.Searches = append(report.Searches, detail)
		}
		if strings.Contains(detail, "CORRELATED") {
			report.CorrelatedSubqueries = append(report.CorrelatedSubqueries, detail)
		}
		if strings.Contains(detail, "USE TEMP B-TREE") {
			report.TempBTrees = append(report.TempBTrees, detail)
		}
		for _, marker := range []string{"USING COVERING INDEX ", "USING INDEX ", "USING INTEGER PRIMARY KEY"} {
			if idx := strings.Index(detail, marker); idx >= 0 {
				token := detail[idx:]
				if !seenIdx[token] {
					seenIdx[token] = true
					report.IndexesUsed = append(report.IndexesUsed, token)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return report
}

func captureExplainPlans(t *testing.T, db *sql.DB, captured map[string]sqlStatementRecord) map[string]queryPlanReport {
	t.Helper()
	plans := map[string]queryPlanReport{}
	for _, spec := range []struct {
		planKey, stmtLabel string
	}{
		{"threads_main", "threads_main_query"},
		{"threads_unread", "threads_unread_query"},
		{"thread_subject", "thread_subject_query"},
		{"threads_participants", "thread_participants_query"},
		{"overview_agents", "overview_agents"},
		{"overview_human_unread", "overview_human_unread"},
		{"deliveries", "deliveries_query"},
	} {
		if rec, ok := captured[spec.stmtLabel]; ok {
			plans[spec.planKey] = analyzeQueryPlan(t, db, spec.planKey, rec.SQL, rec.Args)
		}
	}
	return plans
}

func runColdWorker(t *testing.T, cfg consoleMeasureConfig) {
	t.Helper()
	tr := &sqlTracer{}
	ruOpenBefore := readSelfRusage(t)
	openStart := time.Now()
	s, err := openDatabase(cfg.WorkerDB, nil, registerTraceDriver(tr))
	if err != nil {
		t.Fatalf("cold worker open: %v", err)
	}
	openWall := time.Since(openStart)
	openRU := diffRusage(ruOpenBefore, readSelfRusage(t))
	defer func() { _ = s.Close() }()

	// Reattach the single target Execution after restart only when measuring inbox or
	// steady-state heartbeat so Store.Open's recovery_deadline does not alter their path.
	if cfg.WorkerOp == "inbox" || cfg.WorkerOp == "heartbeat" {
		if _, err := s.Call(context.Background(), "heartbeat", Request{
			ExecutionID: cfg.WorkerExecID, Generation: 2, Source: "shim",
		}); err != nil {
			t.Fatalf("cold worker reattach for %s: %v", cfg.WorkerOp, err)
		}
	}

	m, stmts := measureStoreOperation(t, s, tr, "cold", cfg.WorkerOp, cfg.WorkerExecID)
	m.OpenWallMS = durationMS(openWall)
	m.OpenCPUMS = openRU.TotalMS
	if m.Error != "" {
		t.Fatalf("cold worker %s error: %s", cfg.WorkerOp, m.Error)
	}

	captured := map[string]sqlStatementRecord{}
	for _, st := range stmts {
		if _, exists := captured[st.Label]; !exists {
			captured[st.Label] = st
		}
	}
	payload := struct {
		Operation opMeasurement              `json:"operation"`
		Plans     map[string]queryPlanReport `json:"plans,omitempty"`
	}{
		Operation: m,
		Plans:     captureExplainPlans(t, s.db, captured),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.OutPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func measureOverview14kPoint(t *testing.T, workDir string) *overview14kReport {
	t.Helper()
	dbPath := filepath.Join(workDir, "overview-14k.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	_ = os.Remove(dbPath + ".lock")

	start := time.Now()
	s, err := Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	var epoch int64
	if err := s.db.QueryRow("SELECT boot_epoch FROM Meta WHERE id=1").Scan(&epoch); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	const (
		nRepos      = 4
		nAgents     = 226
		nDeliveries = 14000
		nMessages   = 7000
	)
	execSQL := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const series = `WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x+1<?) `
	execSQL(series+`INSERT INTO Repository(id,case_policy,root_commit,conflict_action,name) SELECT printf('ov_rp_%d',x),'folded','root','deny',printf('ov-repo-%d',x) FROM n`, nRepos)
	execSQL(series+`INSERT INTO Agent(id,name,repository_id,host,client,worktree,slot) SELECT printf('ov_human_%d',x),printf('human_ov_rp_%d',x),printf('ov_rp_%d',x),'operator','human','',1 FROM n`, nRepos)
	execSQL(series+`INSERT INTO Agent(id,name,repository_id,host,client,worktree,slot) SELECT printf('ov_ag_%d',x),printf('ov-agent-%03d',x),printf('ov_rp_%d',x%?), 'host','codex',printf('/ov/%d',x),1 FROM n`, nAgents, nRepos)
	execSQL(series+`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session,last_seen_ms) SELECT printf('ov_ex_%d',x),printf('ov_ag_%d',x),printf('ov_rp_%d',x%?),'host','codex',printf('ov-sess-%d',x),1,printf('/ov/%d',x),'main',?,900000,CASE WHEN x%5=0 THEN 'ended' ELSE 'active' END,0,0,1,? FROM n`, nAgents, nRepos, epoch, now)
	execSQL(series+`INSERT INTO Message(id,sender_execution_id,subject,body,created_ms,repository_id,thread_id) SELECT printf('ov_ms_%d',x),printf('ov_ex_%d',x%?),'Overview 14k','small synthetic body',?,printf('ov_rp_%d',(x%?)%?),printf('ov_ms_%d',x) FROM n`, nMessages, nAgents, now-600_000, nAgents, nRepos)
	// 14,000 deliveries across 226 agents (plus 10 unread human deliveries inside the 14,000); leave 250 unacked, ack 13,750.
	execSQL(series+`INSERT INTO Delivery(id,message_id,recipient_agent_id,execution_id,attempt,attention) SELECT printf('ov_dl_%d',x),printf('ov_ms_%d',x%?),CASE WHEN x<10 THEN printf('ov_human_%d',x%?) ELSE printf('ov_ag_%d',(x/2+1)%?) END,CASE WHEN x<250 THEN NULL ELSE printf('ov_ex_%d',(x/2+1)%?) END,CASE WHEN x<250 THEN 0 ELSE 1 END,CASE WHEN x<250 THEN 1 ELSE 0 END FROM n`, nDeliveries, nMessages, nRepos, nAgents, nAgents)
	execSQL(series+`INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms) SELECT printf('ov_dl_%d',x),printf('ov_ex_%d',(x/2+1)%?),1,? FROM n WHERE x>=250`, nDeliveries, nAgents, now-300_000)

	if err := tx.Commit(); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	var busy, logFrames, moved int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &moved); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	counts := collectDatabaseCounts(t, s.db, now-7*24*3600_000)
	genMS := durationMS(time.Since(start))
	dbSum := collectDatabaseSummary(t, dbPath, s.db, counts, genMS)
	_ = s.Close()

	coldPath := filepath.Join(workDir, "overview-14k-cold.db")
	copyDBCold(t, dbPath, coldPath)
	tr := &sqlTracer{}
	coldStore, err := openDatabase(coldPath, nil, registerTraceDriver(tr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = coldStore.Close()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		_ = os.Remove(dbPath + ".lock")
		_ = os.Remove(coldPath)
		_ = os.Remove(coldPath + "-wal")
		_ = os.Remove(coldPath + "-shm")
		_ = os.Remove(coldPath + ".lock")
	}()

	coldM, stmts := measureStoreOperation(t, coldStore, tr, "cold", "ui_overview", "")
	warmM, _ := measureStoreOperation(t, coldStore, tr, "warm", "ui_overview", "")

	var plan queryPlanReport
	for _, st := range stmts {
		if st.Label == "overview_agents" {
			plan = analyzeQueryPlan(t, coldStore.db, "overview_agents_14k", st.SQL, st.Args)
			break
		}
	}
	return &overview14kReport{
		Database:     dbSum,
		Cold:         coldM,
		Warm:         warmM,
		OverviewPlan: plan,
	}
}

// TestConsoleReadStallMeasurement reproduces and measures the console-read Store.mu stall
// on Linux/WSL. It is skipped unless IRIS_MEASURE_CONSOLE is set.
func TestConsoleReadStallMeasurement(t *testing.T) {
	cfg, ok := parseConsoleMeasureConfig(t)
	if !ok {
		t.Skip("set IRIS_MEASURE_CONSOLE=1 (with IRIS_MEASURE_CONSOLE_SIZE_MB, IRIS_MEASURE_CONSOLE_CACHE, IRIS_MEASURE_CONSOLE_OUT) to run console stall measurement")
	}
	if cfg.Worker {
		runColdWorker(t, cfg)
		return
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.OutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runDir, err := os.MkdirTemp(cfg.WorkDir, "run-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	cfg.WorkDir = runDir

	env := measureEnvironment(t, runDir, cfg)
	baseDB := filepath.Join(runDir, "synthetic-base.db")
	seed := generateSyntheticConsoleDB(t, baseDB, cfg)

	ops := []string{"ui_threads", "ui_overview", "ui_deliveries", "ui_doctor_get", "inbox", "heartbeat", "expire"}
	report := consoleStallReport{
		Environment:  env,
		Config:       cfg,
		Database:     seed.Summary,
		ExplainPlans: map[string]queryPlanReport{},
	}

	capturedSQL := map[string]sqlStatementRecord{}
	switch cfg.CacheMode {
	case "warm":
		warmDB := filepath.Join(cfg.WorkDir, "measure-warm.db")
		fadviseNote := copyDBCold(t, baseDB, warmDB)
		report.CacheNotes = []string{
			"warm mode: pass 1 warms SQLite (~2 MiB default cache_size=-2000) and OS page cache in the same process and Store; pass 2 is reported in operations.",
			"warm db setup: " + fadviseNote,
		}
		tr := &sqlTracer{}
		s, err := openDatabase(warmDB, nil, registerTraceDriver(tr))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = s.Close()
			_ = os.Remove(warmDB)
			_ = os.Remove(warmDB + "-wal")
			_ = os.Remove(warmDB + "-shm")
			_ = os.Remove(warmDB + ".lock")
		}()
		// Reattach activeExecID so inbox and heartbeat measure steady-state execution.
		if _, err := s.Call(context.Background(), "heartbeat", Request{
			ExecutionID: seed.ActiveExecID, Generation: 2, Source: "shim",
		}); err != nil {
			t.Fatalf("warm reattach: %v", err)
		}

		for _, op := range ops {
			m, stmts := measureStoreOperation(t, s, tr, "warmup", op, seed.ActiveExecID)
			report.WarmupOperations = append(report.WarmupOperations, m)
			for _, st := range stmts {
				if _, exists := capturedSQL[st.Label]; !exists {
					capturedSQL[st.Label] = st
				}
			}
		}
		for _, op := range ops {
			m, _ := measureStoreOperation(t, s, tr, "warm", op, seed.ActiveExecID)
			report.Operations = append(report.Operations, m)
		}
		report.ExplainPlans = captureExplainPlans(t, s.db, capturedSQL)

	case "cold":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		fadviseNotes := map[string]bool{}
		for _, op := range ops {
			coldDB := filepath.Join(cfg.WorkDir, "measure-cold-"+op+".db")
			opOut := filepath.Join(cfg.WorkDir, "worker-"+op+".json")
			note := copyDBCold(t, baseDB, coldDB)
			fadviseNotes[note] = true

			cmd := exec.Command(exe, "-test.run=^TestConsoleReadStallMeasurement$")
			cmd.Env = append(os.Environ(),
				"IRIS_MEASURE_CONSOLE=1",
				"IRIS_MEASURE_CONSOLE_WORKER=1",
				"IRIS_MEASURE_CONSOLE_OP="+op,
				"IRIS_MEASURE_CONSOLE_DB="+coldDB,
				"IRIS_MEASURE_CONSOLE_EXEC="+seed.ActiveExecID,
				"IRIS_MEASURE_CONSOLE_OUT="+opOut,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("cold worker %s failed: %v\n%s", op, err, out)
			}
			raw, err := os.ReadFile(opOut)
			if err != nil {
				t.Fatal(err)
			}
			var workerRes struct {
				Operation opMeasurement              `json:"operation"`
				Plans     map[string]queryPlanReport `json:"plans"`
			}
			if err := json.Unmarshal(raw, &workerRes); err != nil {
				t.Fatal(err)
			}
			report.Operations = append(report.Operations, workerRes.Operation)
			for k, p := range workerRes.Plans {
				report.ExplainPlans[k] = p
			}
			_ = os.Remove(opOut)
			_ = os.Remove(coldDB)
			_ = os.Remove(coldDB + "-wal")
			_ = os.Remove(coldDB + "-shm")
			_ = os.Remove(coldDB + ".lock")
		}
		for note := range fadviseNotes {
			report.CacheNotes = append(report.CacheNotes, "cold mode file preparation: "+note)
		}
		report.CacheNotes = append(report.CacheNotes,
			"cold mode runs each operation in a fresh child process on a fresh copy of the checkpointed DB, dropping SQLite's ~2 MiB page cache and calling posix_fadvise(FADV_DONTNEED) before opening.",
			"limitation without root: /proc/sys/vm/drop_caches was not written, tmpfs does not evict pages on FADV_DONTNEED (use an ext4 directory via IRIS_MEASURE_CONSOLE_DIR), and Windows-host WSL2 VHD page cache outside the guest cannot be dropped from inside Linux.",
		)
	}

	_ = os.Remove(baseDB)
	_ = os.Remove(baseDB + "-wal")
	_ = os.Remove(baseDB + "-shm")
	_ = os.Remove(baseDB + ".lock")

	report.Overview14kPoint = measureOverview14kPoint(t, cfg.WorkDir)
	leaseRes := runLeaseConsequenceScenario(t, filepath.Join(cfg.WorkDir, "lease-consequence-measure.db"))
	report.LeaseConsequence = &leaseRes
	readNonBlockingRes := runReadNonBlockingScenario(t, filepath.Join(cfg.WorkDir, "read-non-blocking-measure.db"))
	report.ReadNonBlocking = &readNonBlockingRes

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.OutPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Logf("report written: %s (db=%d bytes, fs=%s, mode=%s)", cfg.OutPath, report.Database.FileBytes, env["filesystem"], cfg.CacheMode)
	for _, op := range report.Operations {
		t.Logf("%-14s wall=%8.2f ms  cpu=%7.2f ms (u=%6.2f s=%6.2f)  mu_wait=%6.3f ms  mu_held=%8.2f ms  begin=%5.2f  expire=%5.2f  query=%8.2f  commit=%5.2f",
			op.Operation, op.WallMS, op.CPUMS.TotalMS, op.CPUMS.UserMS, op.CPUMS.SysMS, op.MuWaitMS, op.MuHeldMS,
			op.Breakdown.BeginMS, op.Breakdown.ExpireMS, op.Breakdown.QueryMS, op.Breakdown.CommitMS)
	}
}
