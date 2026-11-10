//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
	"golang.org/x/sys/unix"
)

// TestReducedScopeMeasurement measures ADR 018 on this machine: real shell hooks in a
// superproject whose full status takes about a second, against a loopback hub. It runs only
// with IRIS_MEASURE_SCOPE=1 and IRIS_MEASURE_SCOPE_DIR naming a directory on the disk to
// measure; IRIS_MEASURE_SCOPE_BASELINE may name an iris binary without the hook's side, for the
// comparison. It writes report.json there. The test process is a child subreaper, so git
// processes a killed status leaves are reaped here and their CPU is counted with the hook's.
func TestReducedScopeMeasurement(t *testing.T) {
	if os.Getenv("IRIS_MEASURE_SCOPE") != "1" {
		t.Skip("set IRIS_MEASURE_SCOPE=1 and IRIS_MEASURE_SCOPE_DIR to measure reduced scope")
	}
	dir := os.Getenv("IRIS_MEASURE_SCOPE_DIR")
	if dir == "" {
		t.Fatal("IRIS_MEASURE_SCOPE_DIR names the directory to measure in")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	m := &measurement{t: t, dir: dir}
	m.report.Environment = environment(t, dir)
	m.binary = filepath.Join(dir, "bin", "iris")
	if out, err := exec.Command("go", "build", "-trimpath", "-o", m.binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	m.fixture()
	m.warm()
	m.startHub()

	m.measure("branch", m.binary, "measure", "attributed.txt", true)
	if baseline := os.Getenv("IRIS_MEASURE_SCOPE_BASELINE"); baseline != "" {
		m.measure("baseline", baseline, "baseline", "baseline.txt", false)
	}
	raw, _ := json.MarshalIndent(m.report, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range m.report.Phases {
		t.Logf("%-9s %-22s %s", p.Binary, p.Name, p.summary())
	}
	if len(m.report.Leftovers) > 0 {
		t.Fatalf("descendants alive 100 ms after a hook exited: %+v", m.report.Leftovers)
	}
	for _, p := range m.report.Phases {
		for _, h := range p.Hooks {
			if len(h.Earlier) > 0 {
				t.Fatalf("%s %s %d: processes alive before the hook started: %+v", p.Name, h.Event, h.Command, h.Earlier)
			}
		}
	}
}

type measurement struct {
	t      *testing.T
	dir    string
	binary string
	super  string
	hubURL string
	report scopeReport
}

type scopeReport struct {
	Environment map[string]string `json:"environment"`
	Warm        []gitRun          `json:"warm_status"`
	Phases      []*phase          `json:"phases"`
	Breakers    []breakerState    `json:"breakers"`
	Leftovers   []process         `json:"leftovers"`
}

type gitRun struct {
	Scope  string  `json:"scope"`
	WallMS float64 `json:"wall_ms"`
	CPUMS  float64 `json:"cpu_ms"`
}

type phase struct {
	Binary string    `json:"binary"`
	Name   string    `json:"name"`
	Hooks  []hookRun `json:"hooks"`
}

type hookRun struct {
	Command  int      `json:"command"`
	Event    string   `json:"event"`
	WallMS   float64  `json:"wall_ms"`
	CPUMS    float64  `json:"cpu_ms"`
	Exit     int      `json:"exit"`
	Warned   bool     `json:"warned"`
	Failures []string `json:"failures,omitempty"`
	// Earlier is what was below the harness before the hook started, after reaping: nothing,
	// if the measurement holds. Reaped is what outlived its parent during the hook and was
	// reparented here, with its pid and start time; the hook's own children are not seen.
	Earlier []process `json:"earlier,omitempty"`
	Reaped  []process `json:"reaped,omitempty"`
}

type breakerState struct {
	Binary     string  `json:"binary"`
	After      string  `json:"after"`
	Scope      string  `json:"scope"`
	CooldownS  float64 `json:"cooldown_s"`
	NextFullIn float64 `json:"next_full_in_s"`
	Reason     string  `json:"reason"`
}

type process struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start_ticks"`
	State string `json:"state"`
	Comm  string `json:"comm"`
}

func environment(t *testing.T, dir string) map[string]string {
	t.Helper()
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	version, _ := exec.Command("git", "--version").Output()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	kind := fmt.Sprintf("0x%x", fs.Type)
	if fs.Type == 0xef53 {
		kind = "ext4 (0xef53)"
	}
	return map[string]string{
		"kernel": strings.TrimSpace(string(release)), "git": strings.TrimSpace(string(version)),
		"filesystem": kind, "cpus": strconv.Itoa(runtime.NumCPU()), "go": runtime.Version(),
	}
}

func (m *measurement) git(dir string, args ...string) {
	m.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		m.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// fixture makes the superproject: seven submodules, one holding 448 MiB in four files that are
// rewritten with the same content after the checkout, so that every status looking inside it
// hashes them again, and six small ones. Nothing refreshes that index afterwards: the hooks and
// the warm-up pass --no-optional-locks.
func (m *measurement) fixture() {
	t := m.t
	root := filepath.Join(m.dir, "fixture")
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("2026-11-10T12:00:00.000Z,EURUSD,1.08420,1.08430,100\n"), 20200)[:1<<20]
	names := []string{"market", "small1", "small2", "small3", "small4", "small5", "small6"}
	for _, name := range names {
		repo := filepath.Join(root, name)
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		m.git(repo, "init", "-q")
		files := map[string]int{"readme.txt": 0}
		if name == "market" {
			files = map[string]int{"ticks0.csv": 112, "ticks1.csv": 112, "ticks2.csv": 112, "ticks3.csv": 112}
		}
		for file, mib := range files {
			f, err := os.Create(filepath.Join(repo, file))
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(name + "\n")
			for range mib {
				if err == nil {
					_, err = f.Write(block)
				}
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		m.git(repo, "add", "-A")
		m.git(repo, "commit", "-qm", name)
	}
	m.super = filepath.Join(root, "super")
	if err := os.MkdirAll(m.super, 0o700); err != nil {
		t.Fatal(err)
	}
	m.git(m.super, "init", "-q")
	for _, file := range []string{"top.txt", "attributed.txt", "baseline.txt"} {
		if err := os.WriteFile(filepath.Join(m.super, file), []byte(file+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.git(m.super, "add", "-A")
	for _, name := range names {
		m.git(m.super, "submodule", "-q", "add", filepath.Join(root, name), name)
	}
	m.git(m.super, "commit", "-qm", "submodules")
	m.git(m.super, "remote", "add", "origin", "https://example.invalid/iris-measure/scope.git")
	later := time.Now().Add(time.Minute)
	for i := range 4 {
		if err := os.Chtimes(filepath.Join(m.super, "market", fmt.Sprintf("ticks%d.csv", i)), later, later); err != nil {
			t.Fatal(err)
		}
	}
}

// warm runs the hook's two statuses directly, three times each, so the page cache holds the
// files, and records what they cost.
func (m *measurement) warm() {
	for _, scope := range []string{"untracked", "dirty", "untracked", "dirty", "untracked", "dirty"} {
		before := childrenCPU()
		start := time.Now()
		cmd := exec.Command("git", "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules="+scope)
		cmd.Dir = m.super
		if out, err := cmd.CombinedOutput(); err != nil {
			m.t.Fatalf("status: %v: %s", err, out)
		}
		m.report.Warm = append(m.report.Warm, gitRun{Scope: scope, WallMS: ms(time.Since(start)), CPUMS: ms(childrenCPU() - before)})
	}
}

func (m *measurement) startHub() {
	s, err := store.Open(filepath.Join(m.dir, "hub.db"), nil)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { _ = s.Close(); _ = os.Remove(filepath.Join(m.dir, "hub.db")) })
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = hub.New(s, "secret", []string{server.Listener.Addr().String()})
	server.Start()
	m.t.Cleanup(server.Close)
	m.hubURL = server.URL
}

// measure runs one binary's phases with a memory of its own. With reduced, the phases are the
// cold trip, 30 commands within the cooldown, and the full attempt once it is due; without, 30
// commands. One command in the middle edits a file another session holds.
func (m *measurement) measure(binary, path, session, edited string, reduced bool) {
	t := m.t
	tokens := filepath.Join(m.dir, "tokens-"+binary)
	if err := os.RemoveAll(tokens); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tokens, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(tokens, "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	holder := &hubclient.Client{Config: hubclient.EnvConfig{Getenv: func(key string) string {
		return map[string]string{"IRIS_HUB_URL": m.hubURL, "IRIS_TOKEN_FILE": token}[key]
	}}}
	ctx := context.Background()
	held, err := holder.RegisterSession(ctx, hubclient.SessionRequest{Client: "codex", SessionID: "holder-" + binary, Host: "elsewhere", CWD: "/elsewhere", Worktree: "/elsewhere", RemoteURL: "https://example.invalid/iris-measure/scope.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.Touch(ctx, hubclient.TouchRequest{ExecutionID: held.ExecutionID, Generation: held.Generation, ToolCallID: "held", Paths: []string{"top.txt", "attributed.txt", "baseline.txt"}}); err != nil {
		t.Fatal(err)
	}
	r := runner{m: m, binary: path, token: token, session: session}
	start := &phase{Binary: binary, Name: "session start"}
	start.Hooks = append(start.Hooks, r.hook("SessionStart", 0))
	m.report.Phases = append(m.report.Phases, start)
	if !reduced {
		steady := &phase{Binary: binary, Name: "30 commands"}
		r.commands(steady, 30, edited)
		m.report.Phases = append(m.report.Phases, steady)
		return
	}
	cold := &phase{Binary: binary, Name: "cold trip"}
	r.command(cold, 1, "top.txt")
	m.report.Phases = append(m.report.Phases, cold)
	tripped := m.breaker(binary, "cold trip", token)
	steady := &phase{Binary: binary, Name: "steady reduced"}
	r.commands(steady, 30, edited)
	m.report.Phases = append(m.report.Phases, steady)
	if time.Now().After(tripped) {
		t.Fatal("the steady phase outlasted the cooldown")
	}
	time.Sleep(time.Until(tripped) + 100*time.Millisecond)
	due := &phase{Binary: binary, Name: "due full attempt"}
	r.command(due, 1, "")
	m.report.Phases = append(m.report.Phases, due)
	m.breaker(binary, "due full attempt", token)
}

// breaker records the worktree's breaker and returns when its next full attempt is due.
func (m *measurement) breaker(binary, after, token string) time.Time {
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(token), "sessions", "breaker-*"))
	if len(files) != 1 {
		m.t.Fatalf("breakers %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		m.t.Fatal(err)
	}
	var b struct {
		Scope      string `json:"scope"`
		Reason     string `json:"reason"`
		NextFullMS int64  `json:"next_full_ms"`
		CooldownMS int64  `json:"cooldown_ms"`
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		m.t.Fatal(err)
	}
	next := time.UnixMilli(b.NextFullMS)
	m.report.Breakers = append(m.report.Breakers, breakerState{Binary: binary, After: after, Scope: b.Scope, CooldownS: float64(b.CooldownMS) / 1000, NextFullIn: time.Until(next).Seconds(), Reason: b.Reason})
	return next
}

type runner struct {
	m                      *measurement
	binary, token, session string
}

func (r runner) commands(p *phase, n int, edited string) {
	for i := 1; i <= n; i++ {
		file := ""
		if i == n/2 {
			file = edited
		}
		r.command(p, i, file)
	}
}

// command is one shell command: its pre-tool hook, the edit it makes, if any, and its post-tool
// hook.
func (r runner) command(p *phase, n int, edited string) {
	p.Hooks = append(p.Hooks, r.hook("PreToolUse", n))
	if edited != "" {
		if err := os.WriteFile(filepath.Join(r.m.super, edited), []byte(fmt.Sprintf("edited by command %d\n", n)), 0o600); err != nil {
			r.m.t.Fatal(err)
		}
	}
	p.Hooks = append(p.Hooks, r.hook("PostToolUse", n))
}

// hook runs one hook process. Its CPU is what the hook, its git processes and any orphan reaped
// here used. 100 ms after it exits, no process below this one may be alive.
func (r runner) hook(event string, n int) hookRun {
	t := r.m.t
	payload := map[string]any{"session_id": r.session, "cwd": r.m.super, "hook_event_name": event}
	if event == "SessionStart" {
		payload["source"] = "startup"
	} else {
		payload["tool_name"], payload["tool_use_id"], payload["tool_input"] = "Bash", fmt.Sprintf("t%d", n), map[string]string{"command": "make"}
	}
	raw, _ := json.Marshal(payload)
	failed, _ := hook.ReadRecentFailures(r.token, 1<<16)
	// What earlier work left, the fixture's git included, is reaped now, so it is not counted as
	// this hook's. Anything still alive here would be.
	reapAll()
	run := hookRun{Command: n, Event: event, Earlier: descendants(os.Getpid())}
	before := childrenCPU()
	cmd := exec.Command(r.binary, "hook", "--client", "claude", "--event", event)
	cmd.Env = append(os.Environ(), "IRIS_HUB_URL="+r.m.hubURL, "IRIS_TOKEN_FILE="+r.token, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = bytes.NewReader(raw)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start)
	run.WallMS, run.Warned = ms(wall), strings.Contains(stdout.String(), "Shell edit detected")
	if exit, ok := err.(*exec.ExitError); ok {
		run.Exit = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	run.Reaped = descendants(os.Getpid())
	time.Sleep(100 * time.Millisecond)
	for _, p := range descendants(os.Getpid()) {
		if p.State != "Z" {
			r.m.report.Leftovers = append(r.m.report.Leftovers, p)
			_ = syscall.Kill(p.PID, syscall.SIGKILL)
		}
	}
	reapAll()
	run.CPUMS = ms(childrenCPU() - before)
	now, _ := hook.ReadRecentFailures(r.token, 1<<16)
	for _, f := range now[min(len(failed), len(now)):] {
		run.Failures = append(run.Failures, f.Stage+": "+f.Error)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("%s %d: invalid output %q", event, n, stdout.String())
	}
	return run
}

func childrenCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// reapAll collects every exited child, orphans reparented here included, so their CPU counts.
func reapAll() {
	for {
		var status syscall.WaitStatus
		if pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil); pid <= 0 || err != nil {
			return
		}
	}
}

// descendants lists the processes below pid by /proc's parent links, with their start times.
func descendants(root int) []process {
	entries, _ := os.ReadDir("/proc")
	parent := map[int]int{}
	info := map[int]process{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(raw)
		open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || end < open {
			continue
		}
		fields := strings.Fields(s[end+1:])
		if len(fields) < 20 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		start, _ := strconv.ParseUint(fields[19], 10, 64)
		parent[pid] = ppid
		info[pid] = process{PID: pid, Start: start, State: fields[0], Comm: s[open+1 : end]}
	}
	var out []process
	for pid, p := range info {
		for up := parent[pid]; up > 1; up = parent[up] {
			if up == root {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (p *phase) summary() string {
	var b strings.Builder
	for _, event := range []string{"SessionStart", "PreToolUse", "PostToolUse"} {
		var wall, cpu []float64
		over, failures, warned := 0, 0, 0
		for _, h := range p.Hooks {
			if h.Event != event {
				continue
			}
			wall, cpu = append(wall, h.WallMS), append(cpu, h.CPUMS)
			if h.WallMS > 300 {
				over++
			}
			failures += len(h.Failures)
			if h.Warned {
				warned++
			}
		}
		if len(wall) == 0 {
			continue
		}
		slices.Sort(wall)
		slices.Sort(cpu)
		fmt.Fprintf(&b, "%s n=%d wall p50 %.1f p95 %.1f max %.1f ms; cpu p50 %.1f max %.1f ms; over 300 ms %d; failures %d; warned %d. ",
			event, len(wall), wall[len(wall)/2], wall[(len(wall)*95)/100], wall[len(wall)-1], cpu[len(cpu)/2], cpu[len(cpu)-1], over, failures, warned)
	}
	return b.String()
}
