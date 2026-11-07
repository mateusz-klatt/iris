package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/store"
)

// LivePlan is one billable client run, shown before anything is spent.
type LivePlan struct {
	Client string
	Binary string
	Model  string
	// Requires is the step a person must have done for the hook to run. It is
	// set only when the user named a client that doctor cannot see is ready.
	Requires string
}

// LiveResult is what one live run proved. Each field is a separate claim, so a
// client whose hook works but whose MCP server does not reads as exactly that.
type LiveResult struct {
	LivePlan
	// HookFired means the client's own hook registered the scratch session.
	HookFired bool
	// ClaimReached means the pre-edit hook asked the hub for scratch.txt and was allowed.
	ClaimReached bool
	// ToolsAnswered means the model's reply carries the execution id that only whoami returns.
	ToolsAnswered bool
	// FileEdited means the scratch file holds the line the prompt asked to add.
	FileEdited bool
	// Marker is the scratch repository's origin, by which stray records can be found.
	Marker string
	Detail string
}

// OK is true when every part of the client answered.
func (r LiveResult) OK() bool {
	return r.HookFired && r.ClaimReached && r.ToolsAnswered && r.FileEdited && r.Detail == ""
}

// LiveOptions configures live runs. Run replaces process execution in tests.
type LiveOptions struct {
	Layout integrate.Layout
	// Timeout limits one client. Zero means the default.
	Timeout time.Duration
	Run     func(ctx context.Context, dir string, env []string, name string, args []string) ([]byte, error)
}

const liveTimeout = 3 * time.Minute

// scratchFile is the one file a live run edits.
const scratchFile = "scratch.txt"

// liveClient is how one client runs headless. Models are the cheapest ones
// named in the client reports; an empty model leaves the client's default.
// No flag bypasses workspace trust: a client that refuses an untrusted
// directory says so, which is what the user needs to know.
type liveClient struct {
	model string
	args  func(model, prompt string) []string
	// guard, when set, runs before the client and returns the clean-up that
	// runs after it, which reports what it could not undo.
	guard func(l integrate.Layout, repo string) func() string
}

var liveClients = map[string]liveClient{
	// The allow list names only the tools the prompt needs. The prompt comes
	// first because --allowedTools takes every later word as a tool name.
	integrate.Claude: {model: "haiku", args: func(model, prompt string) []string {
		return []string{"-p", prompt, "--model", model, "--allowedTools", "Read,Edit,Write,mcp__plugin_iris_iris__whoami"}
	}},
	// workspace-write lets the model edit only inside the scratch repository,
	// and approval_policy never keeps it from waiting for a person. The Iris
	// tools act only on the hub, so they are approved for this run.
	// Not the cheapest model: gpt-6-luna wrote the file through Code Mode's js
	// tool, which no pre-edit hook sees, in every measured run. gpt-6.1-terra
	// used apply_patch in 8 of 8 runs (measured on macOS by claude-laptop-iris).
	integrate.Codex: {model: "gpt-6.1-terra", guard: codexTrust, args: func(model, prompt string) []string {
		return []string{"exec", "-m", model, "-c", `sandbox_mode="workspace-write"`, "-c", `approval_policy="never"`, "-c", `mcp_servers.iris.default_tools_approval_mode="approve"`, prompt}
	}},
	integrate.Copilot: {model: "claude-haiku-4.5", args: func(model, prompt string) []string {
		return []string{"-p", prompt, "--model", model}
	}},
	// --trust trusts only the scratch repository doctor itself created under
	// the temporary directory, never one of the user's. Without it a headless
	// cursor-agent prints "Workspace Trust Required" and exits 0 having run
	// nothing (measured on holzera), so no hook could fire.
	integrate.Cursor: {model: "auto", args: func(model, prompt string) []string {
		return []string{"-p", "--trust", "--model", model, prompt}
	}},
	integrate.Antigravity: {model: "gemini-3.8-flash-low", guard: agyWrite, args: func(model, prompt string) []string {
		return []string{"--print=" + prompt, "--model", model}
	}},
	// A headless Grok cannot ask for approval, so the tools the prompt needs
	// are allowed by name. Measured on macOS by claude-laptop-iris.
	integrate.Grok: {model: "grok-4.7-build-fast", args: func(model, prompt string) []string {
		return []string{"-p", prompt, "-m", model, "--allow", "iris__whoami", "--allow", "Edit", "--allow", "Write"}
	}},
	// Kimi model names are aliases from the user's config.toml; this one is in
	// Kimi Code's own catalog, and an unknown alias fails before any spending.
	integrate.Kimi: {model: "moonshot-ai/kimi-k2.7-code-highspeed", args: func(model, prompt string) []string {
		return []string{"-p", prompt, "-m", model}
	}},
}

// LivePlans lists the clients a live run would call: those whose hook passed
// the offline canary, that need nothing from a person, and whose binary is on
// the path. A client that still needs a person, such as Codex before its
// hooks are reviewed, is skipped with the reason: its hook would not run, and
// spending on it proves nothing, unless only names it. A non-empty only keeps
// that client alone.
func LivePlans(layout integrate.Layout, rows []ClientReport, only string) (plans []LivePlan, skipped []string) {
	for _, row := range rows {
		if only != "" && row.Client != only {
			continue
		}
		requires := ""
		switch {
		case row.Status == StatusNeedsHuman && only == row.Client && row.Detail != "client binary not found":
			// Naming the client is the user's statement that the step is done.
			requires = row.Detail
		case row.Status == StatusNeedsHuman:
			skipped = append(skipped, row.Client+": "+row.Detail)
			continue
		case row.Status != StatusOK && row.Status != StatusSilent:
			// A silent row may come from an old failure in the log, and a live
			// run is how to prove the client works now, so silent ones run too.
			continue
		}
		bin, err := layout.Binary(row.Client)
		if err != nil {
			skipped = append(skipped, row.Client+": client binary not found")
			continue
		}
		plans = append(plans, LivePlan{Client: row.Client, Binary: bin, Model: liveClients[row.Client].model, Requires: requires})
	}
	return plans, skipped
}

// RunLive runs one client headless in a scratch repository against a hub
// that exists only for this run, so nothing is written to the configured hub
// unless the client hides the environment from its hook.
func RunLive(ctx context.Context, opt LiveOptions, plan LivePlan) (res LiveResult) {
	res = LiveResult{LivePlan: plan}
	spec, ok := liveClients[plan.Client]
	if !ok {
		res.Detail = "no live invocation for " + plan.Client
		return res
	}
	dir, err := os.MkdirTemp("", "iris-doctor-live-")
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	defer func() { _ = os.RemoveAll(dir) }()
	id, nonce := randomHex(), randomHex()
	res.Marker = "https://example.invalid/iris-doctor-live/" + id + ".git"
	repo, h, err := setupScratch(dir, res.Marker)
	if err != nil {
		res.Detail = "scratch setup: " + err.Error()
		return res
	}
	defer h.close()
	if spec.guard != nil {
		undo := spec.guard(opt.Layout, repo)
		defer func() {
			if note := undo(); note != "" {
				res.Detail = strings.TrimPrefix(res.Detail+"; "+note, "; ")
			}
		}()
	}
	env := liveEnv(os.Environ(), h.url, h.tokenFile)
	model := spec.model
	args := spec.args(model, livePrompt(nonce))
	if opt.Timeout == 0 {
		opt.Timeout = liveTimeout
	}
	run := opt.Run
	if run == nil {
		run = execLive
	}
	cctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	out, runErr := run(cctx, repo, env, plan.Binary, args)
	timedOut := cctx.Err() != nil
	cancel()
	seen := h.rec.snapshot(res.Marker, plan.Client)
	res.HookFired = len(seen.executions) > 0
	res.ClaimReached = seen.claimed
	for _, e := range seen.executions {
		if bytes.Contains(out, []byte(e)) {
			res.ToolsAnswered = true
		}
	}
	got, _ := os.ReadFile(filepath.Join(repo, scratchFile))
	// The prompt asks for a line to be added: asked to replace the whole
	// file, Codex wrote an invalid patch every time (measured on macOS).
	for _, line := range strings.Split(string(got), "\n") {
		if strings.TrimSpace(line) == nonce {
			res.FileEdited = true
		}
	}
	var notes []string
	switch {
	case timedOut:
		notes = append(notes, fmt.Sprintf("timed out after %s", opt.Timeout))
	case runErr != nil:
		notes = append(notes, "client failed: "+runErr.Error()+": "+clip(out))
	}
	if !res.HookFired && plan.Requires != "" {
		notes = append(notes, "the hook did not fire; it runs only after this step: "+plan.Requires)
	}
	if !res.HookFired {
		notes = append(notes, "the scratch hub saw no session from this client's hook. Either the client stopped before running it, or the hook used the configured hub, under the repository with origin "+res.Marker+", which the hub's owner can remove with `iris admin forget`")
	}
	// Any fact that failed is explained only by what the client did, such as
	// Cursor stopping with exit 0 in an untrusted workspace, or a model writing
	// the file with a tool no pre-edit hook sees. A failed run already shows it.
	failed := !res.HookFired || !res.ClaimReached || !res.ToolsAnswered || !res.FileEdited
	if failed && runErr == nil && !timedOut {
		notes = append(notes, "client output: "+clip(out))
	}
	res.Detail = strings.Join(notes, "; ")
	return res
}

func livePrompt(nonce string) string {
	return "This is an Iris doctor check in a scratch repository. Do exactly two things and nothing else. " +
		"First, call the Iris MCP tool named whoami and copy the execution_id value it returns into your reply. " +
		"Second, using your file editing tool, append one line containing exactly " + nonce + " to the end of the file " + scratchFile + " in the current directory, keeping what is there. " +
		"Do not run shell commands or code to write the file. End your reply with the execution_id."
}

// inherited are dropped from the client's environment. The hub variables are
// set again below; the others would bind the scratch session to the session
// doctor was started from, when that is itself a client.
var inherited = []string{"IRIS_HUB_URL", "IRIS_TOKEN_FILE", "IRIS_EXECUTION", "CLAUDE_CODE_SESSION_ID", "COPILOT_AGENT_SESSION_ID", "GROK_SESSION_ID"}

// liveEnv points the client, its hook and its MCP server at the scratch hub.
// Both variables win over client.json, so a client that passes its
// environment on never reaches the configured hub.
func liveEnv(base []string, url, tokenFile string) []string {
	env := make([]string, 0, len(base)+2)
next:
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		for _, drop := range inherited {
			if strings.EqualFold(key, drop) {
				continue next
			}
		}
		env = append(env, kv)
	}
	return append(env, "IRIS_HUB_URL="+url, "IRIS_TOKEN_FILE="+tokenFile)
}

// setupScratch makes the repository and the hub, both inside dir.
func setupScratch(dir, marker string) (string, *scratchHub, error) {
	repo := filepath.Join(dir, "repo")
	if err := scratchRepo(repo, marker); err != nil {
		return "", nil, err
	}
	h, err := startScratchHub(dir)
	return repo, h, err
}

// scratchRepo writes the smallest repository git accepts. It has no commit,
// so it shares no root commit with anything, and an origin that cannot resolve.
func scratchRepo(root, origin string) error {
	gitDir := filepath.Join(root, ".git")
	for _, d := range []string{filepath.Join(gitDir, "objects"), filepath.Join(gitDir, "refs", "heads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	files := map[string]string{
		filepath.Join(gitDir, "HEAD"):    "ref: refs/heads/main\n",
		filepath.Join(gitDir, "config"):  "[core]\n\trepositoryformatversion = 0\n\tbare = false\n[remote \"origin\"]\n\turl = " + origin + "\n",
		filepath.Join(root, scratchFile): "replace this line\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

type scratchHub struct {
	url, tokenFile string
	rec            *recorder
	store          *store.Store
	server         *http.Server
}

// listen is replaced by a test that needs the port to be refused.
var listen = net.Listen

func startScratchHub(dir string) (*scratchHub, error) {
	token := randomHex()
	tokenFile := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		return nil, err
	}
	s, err := store.Open(filepath.Join(dir, "hub.db"), nil)
	if err != nil {
		return nil, err
	}
	listener, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	addr := listener.Addr().String()
	handler := hub.New(s, token, []string{addr})
	// The scratch hub's log lines are noise beside the report.
	handler.Log = log.New(io.Discard, "", 0)
	rec := &recorder{next: handler}
	server := &http.Server{Handler: rec, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return &scratchHub{url: (&url.URL{Scheme: "http", Host: addr}).String(), tokenFile: tokenFile, rec: rec, store: s, server: server}, nil
}

func (h *scratchHub) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.server.Shutdown(ctx)
	_ = h.store.Close()
}

// recorder notes the two hook calls a live run proves: the session the hook
// registered and the claim it took before the edit.
type recorder struct {
	next     http.Handler
	mu       sync.Mutex
	sessions []recordedSession
	claims   []string
}

type recordedSession struct{ client, remote, execution string }

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := r.Method + " " + r.URL.Path
	if route != "POST /v1/sessions" && route != "POST /v1/claims/touch" {
		rec.next.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))
	cw := &responseCapture{ResponseWriter: w, status: http.StatusOK}
	rec.next.ServeHTTP(cw, r)
	if cw.status/100 != 2 {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if route == "POST /v1/sessions" {
		var in hubclient.SessionRequest
		var out hubclient.SessionResponse
		_ = json.Unmarshal(body, &in)
		_ = json.Unmarshal(cw.body.Bytes(), &out)
		rec.sessions = append(rec.sessions, recordedSession{client: in.Client, remote: in.RemoteURL, execution: out.ExecutionID})
		return
	}
	var in hubclient.TouchRequest
	var out hubclient.TouchResponse
	_ = json.Unmarshal(body, &in)
	_ = json.Unmarshal(cw.body.Bytes(), &out)
	if out.Action != "allow" {
		return
	}
	for _, p := range in.Paths {
		rec.claims = append(rec.claims, in.ExecutionID+" "+p)
	}
}

type observed struct {
	executions []string
	claimed    bool
}

// snapshot keeps only what the named client's hook did in the scratch
// repository. Some clients also load other clients' hooks (D5), and those
// must not count for it.
func (rec *recorder) snapshot(marker, client string) observed {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var s observed
	for _, sess := range rec.sessions {
		if sess.remote == marker && sess.client == client && sess.execution != "" {
			s.executions = append(s.executions, sess.execution)
		}
	}
	for _, c := range rec.claims {
		for _, e := range s.executions {
			if c == e+" "+scratchFile {
				s.claimed = true
			}
		}
	}
	return s
}

type responseCapture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *responseCapture) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *responseCapture) Write(p []byte) (int, error) {
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

// execLive runs the client with stdin closed: codex exec waits while stdin is open.
func execLive(ctx context.Context, dir string, env []string, name string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = closedStdin()
	return cmd.CombinedOutput()
}

func clip(out []byte) string {
	msg := strings.TrimSpace(string(out))
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return msg
}

func randomHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// FormatLive is the text a person reads, one block per client binary.
func FormatLive(results []LiveResult) string {
	var b strings.Builder
	mark := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "no"
	}
	for _, r := range results {
		state := "ok"
		if !r.OK() {
			state = "failed"
		}
		fmt.Fprintf(&b, "%s (%s, model %s): %s\n", r.Client, r.Binary, r.Model, state)
		fmt.Fprintf(&b, "  hook fired: %s\n  claim reached the hub: %s\n  MCP tools answered: %s\n  scratch file edited: %s\n",
			mark(r.HookFired), mark(r.ClaimReached), mark(r.ToolsAnswered), mark(r.FileEdited))
		if r.Detail != "" {
			fmt.Fprintf(&b, "  %s\n", r.Detail)
		}
	}
	return b.String()
}
