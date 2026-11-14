package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/guard"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// DefaultDeadline is the internal limit from D3. Client timeouts are set longer.
const DefaultDeadline = 1500 * time.Millisecond

// Hub is the subset of the hub API a hook calls.
type Hub interface {
	RegisterSession(ctx context.Context, req hubclient.SessionRequest) (hubclient.SessionResponse, error)
	Touch(ctx context.Context, req hubclient.TouchRequest) (hubclient.TouchResponse, error)
	EndExecution(ctx context.Context, id string, generation int) error
}

// Runner executes one hook invocation.
type Runner struct {
	// Status, Gitlinks and IndexID stand in for gitinfo's in tests; nil means git itself.
	Status          func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error)
	Gitlinks        func(context.Context, string) ([]string, error)
	IndexID         func(string) (gitinfo.IndexID, error)
	Now             func() time.Time
	ClientName      string
	EventName       string
	Stdin           io.Reader
	Getenv          func(string) string
	Hub             Hub
	Find            func(dir string) (gitinfo.Repo, error)
	Ancestor        func() (proc.Ancestor, error)
	Host            func() string
	GOOS            string
	IrisPath        func() string
	PinnedConfigDir func() (string, error)
	Deadline        time.Duration
	// Memory keeps each session's generation between hook processes. Nil means a file
	// beside the token.
	Memory Memory
	// ShellDeadline bounds a shell reconcile. Zero means ReconcileDeadline. Tests that are
	// not about the budget give it room, because a real git status is slow under load.
	ShellDeadline time.Duration
	// after starts the deadline. Tests replace it so a deadline fires at a known
	// stage rather than whenever a slow machine gets there.
	after func(time.Duration) <-chan time.Time
	// shellTimeout starts a shell reconcile's budget, as after starts the hook's deadline. Nil
	// means context.WithTimeout. Tests replace it so the budget runs out inside the stand-in
	// that is slow on purpose, rather than wherever a loaded machine happens to be.
	shellTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	// logAfter starts the failure log budget, as after starts the deadline. Tests that read the
	// log pass one that never fires, so Run returns only once the entry is written.
	logAfter func(time.Duration) <-chan time.Time
	// logged runs once the failure write has finished, even after Run has returned.
	logged func()
	// arming runs as a Kimi prompt's arming decision starts (ADR 030). Tests hold it there to pass
	// the deadline at that stage.
	arming func()
	// settling runs inside a shell reconcile's settle: with true once the first outcome has taken
	// the slot, before it is recorded, and with false before a later outcome waits for that
	// record. Tests hold the first outcome there to pin the order a preemption could produce.
	settling func(first bool)
}

// Run catches every failure, including panics, and always returns valid output
// for the named client. A deadline ends the call even if a dependency is stuck.
func (r Runner) Run() Output {
	allow := FallbackAllow(Canonical(r.ClientName))
	state := &runState{stage: "read"}
	var failure error
	var failureStage string
	defer func() {
		if failure != nil {
			r.recordFailure(failureStage, failure, state.payloadShape())
		}
	}()
	deadline := r.Deadline
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	after := r.after
	if after == nil {
		after = time.After
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Output, 1)
	go func() {
		defer func() {
			if recover() != nil {
				state.fail(failPanic, allow)
				ch <- allow
			}
		}()
		ch <- r.execute(ctx, state)
	}()
	select {
	case out := <-ch:
		failureStage, failure = state.snapshot()
		out.Stderr = append(out.Stderr, state.receiptLine()...)
		if seen := state.observation(); seen != "" {
			r.record(stageObserved, seen, nil)
		}
		return out
	case <-after(deadline):
		cancel()
		failureStage, _ = state.snapshot()
		failure = context.DeadlineExceeded
		return allow
	}
}

func (r Runner) execute(ctx context.Context, state *runState) Output {
	client := Canonical(r.ClientName)
	allow := FallbackAllow(client)
	payload, err := readPayload(r.Stdin)
	if err != nil {
		return state.fail(err, allow)
	}
	if client == "" {
		state.at("client")
		return state.fail(failUnknownClient, allow)
	}
	state.at("decode")
	ev, err := Decode(client, r.EventName, payload)
	if err != nil {
		if runner, cross := r.crossLoad(peekSessionID(payload), client); cross {
			state.observe(crossLoadText(runner))
			return FallbackAllow(runner)
		}
		state.shaped(payloadShape(payload))
		return state.fail(err, allow)
	}
	if runner, cross := r.crossLoad(ev.SessionID, client); cross {
		state.observe(crossLoadText(runner))
		return FallbackAllow(runner)
	}
	state.received(r.canaryReceipt(ev))
	if out, ok := r.antigravityHook(ev, state.observe); ok {
		return r.finish(state, ev, out)
	}
	if ev.Client == ClientCodex && ev.Kind == KindPrompt {
		r.consumeWake(ev)
	}
	// Some clients supply only a timestamp, not a tool call ID. Hash the raw
	// bytes so a retry stays idempotent while distinct timestamped calls differ.
	if ev.ToolCallID == "" {
		hash := sha256.Sum256(payload)
		ev.ToolCallID = "payload:" + hex.EncodeToString(hash[:])
	}
	// Stop asks for a monitor before the repository check. A session outside a git checkout
	// still needs the wake, and the ask does not call the hub (ADR 025).
	if out, ok := r.grokMonitorAsk(ev, state.observe); ok {
		return r.finish(state, ev, out)
	}
	if shellEvent(ev) {
		return r.finish(state, ev, r.reconcile(ctx, ev, state))
	}
	state.at("repository")
	repo, err := r.repoFor(ev)
	if err != nil && !errors.Is(err, gitinfo.ErrNotRepo) {
		return state.fail(err, allow)
	}
	if err != nil {
		return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow}))
	}
	if !needsNetwork(ev) {
		return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow}))
	}
	if r.Hub == nil {
		state.at("hub")
		return state.fail(failNoHub, allow)
	}
	switch ev.Kind {
	case KindSessionStart, KindSubagentStart:
		state.at("session")
		// What the session was told is read before the hub answers; see attentionNote.
		rev := r.toldRevision(ev)
		sess, err := r.register(ctx, ev, repo)
		if err != nil {
			return state.fail(err, r.failOpen(state, ev, err, allow))
		}
		if ev.Kind == KindSubagentStart && ev.ParentSessionID == "" {
			// A sub-agent that is its root session to Iris, as Claude Code's and Codex's are (ADR
			// 017), reads this context with a model of its own. The root session's model never sees
			// it, so it counts as told for no one. A sub-agent with a session of its own, as Grok's
			// has, keeps its own told state below.
			return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow, Context: SessionText(sess)}))
		}
		var notes []string
		// Once per session, not per subagent: the hint is about the repository.
		if ev.Kind == KindSessionStart && guard.NeedsHint(repo) {
			notes = append(notes, guard.Hint)
		}
		// The session context states the unread count and the freed paths, so those counts count
		// as said.
		r.attentionNote(ctx, ev, sess.Attention, true, rev)
		return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow, Context: SessionText(sess, notes...)}))
	case KindPrompt, KindPreInvocation:
		// One registration per user turn: idempotent, hedged, and within the deadline.
		state.at("session")
		rev := r.toldRevision(ev)
		sess, err := r.register(ctx, ev, repo)
		if err != nil {
			return state.fail(err, r.failOpen(state, ev, err, allow))
		}
		// Kimi's one-shot watcher is asked for at a user prompt whose session the hub accepted
		// (ADR 030): Kimi 2.6.1 never runs Stop hooks. The request is decided before the note,
		// which records what the session was told. Were the deadline to pass while arming waits
		// for its lock, after the note, the note would be lost with its attention recorded as told.
		ask := r.kimiArm(ev)
		note := r.attentionNote(ctx, ev, sess.Attention, false, rev)
		if ask != "" {
			note = strings.TrimPrefix(note+"\n"+ask, "\n")
		}
		return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow, Context: note}))
	case KindStop:
		return r.cursorStop(ctx, state, ev, repo)
	case KindSessionEnd:
		state.at("session")
		sess, err := r.register(ctx, ev, repo)
		if err != nil {
			return state.fail(err, allow)
		}
		// Session end is a best-effort speed-up. A missed call still expires the lease.
		state.at("end")
		if err := r.Hub.EndExecution(ctx, sess.ExecutionID, sess.Generation); err != nil {
			return state.fail(err, allow)
		}
		r.forgetGrokSessionFile(ev)
		if m := r.memory(); m != nil {
			m.Forget(string(ev.Client), ev.SessionID)
		}
		// What the session was told goes with it; its wake file stays (ADR 017, decision 3).
		if am, ok := r.memory().(attentionMemory); ok {
			if err := am.forgetAttention(string(ev.Client), ev.SessionID); err != nil {
				r.recordFailure(stageSessionFiles, err, nil)
			}
		}
		return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow}))
	default: // needsNetwork leaves only an edit that is about to run.
		state.at("paths")
		if len(ev.Paths) == 0 {
			return state.fail(failNoPaths, allow)
		}
		paths, from, to := r.claimPaths(repo, ev)
		if len(paths) == 0 {
			return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow}))
		}
		// Touch needs an execution id. The hook only has the client session id,
		// so it reattaches through POST /v1/sessions, which is defined as idempotent.
		state.at("session")
		sess, err := r.register(ctx, ev, repo)
		if hubclient.IsCode(err, "lost") {
			// Only a recovering Execution refuses a request that carries no generation. Its
			// claims still block others, so allowing the edit would leave it unclaimed.
			return r.finish(state, ev, Encode(ev, Decision{Action: ActionDeny, Reason: recovering}))
		}
		if err != nil {
			return state.fail(err, r.failOpen(state, ev, err, allow))
		}
		state.at("touch")
		touch, err := r.Hub.Touch(ctx, hubclient.TouchRequest{
			ExecutionID: sess.ExecutionID,
			Generation:  sess.Generation,
			ToolCallID:  ev.ToolCallID,
			Paths:       paths,
			RenameFrom:  from,
			RenameTo:    to,
		})
		if err != nil {
			return state.fail(err, r.failOpen(state, ev, err, allow))
		}
		switch strings.ToLower(touch.Action) {
		case "allow", "deny", "ask", "warn":
		default:
			return state.fail(failBadTouch, allow)
		}
		d := decisionFromTouch(touch)
		if d.Action == ActionAllow && carriesToolContext(ev.Client) {
			// The touch already brought the counts, so this costs no request.
			if note := r.attentionNote(ctx, ev, touch.Attention, false, fromReplay); note != "" {
				d.Context = strings.TrimSpace(d.Context + "\n" + note)
			}
		}
		return r.finish(state, ev, Encode(ev, d))
	}
}

// rejected tells the model that Iris is not protecting it. D3 still allows the edit, and
// without this the session saw nothing at all when its token was revoked.
const rejected = "Iris: the hub rejected this host's token, so this session's edits are allowed but not claimed, and other agents cannot see them. Tell the user to run `iris doctor`."

// failOpen is D3's allow. A rejected token also tells the model, because a silent allow
// looks exactly like protection.
func (r Runner) failOpen(state *runState, ev Event, err error, allow Output) Output {
	if hubclient.IsCode(err, "unauthorized") {
		return r.finish(state, ev, Encode(ev, Decision{Action: ActionAllow, Context: rejected}))
	}
	return allow
}

// recovering is the refusal while a session cannot yet prove itself after a hub restart.
const recovering = "Iris: the hub restarted and this session cannot prove it is the same one yet, so the edit is refused rather than left unclaimed. Retry shortly: the session reattaches when Iris hears from it with proof, or starts afresh within two minutes of the restart."

// register attaches a hook to its Execution. A remembered generation is the proof a
// recovering Execution asks for. A stale one is dropped and the call made once without it,
// which is what the hook sent before it had a memory. SessionEnd must keep its proof:
// retrying without it could end a newer, healthy generation of the session.
func (r Runner) register(ctx context.Context, ev Event, repo gitinfo.Repo) (hubclient.SessionResponse, error) {
	m := r.memory()
	req := r.sessionReq(ev, repo)
	if m != nil {
		req.Generation = m.Recall(string(ev.Client), ev.SessionID)
	}
	sess, err := r.Hub.RegisterSession(ctx, req)
	if req.Generation != 0 && ev.Kind != KindSessionEnd && hubclient.IsCode(err, "lost") {
		m.Forget(string(ev.Client), ev.SessionID)
		req.RetryGeneration = req.Generation
		req.Generation = 0
		sess, err = r.Hub.RegisterSession(ctx, req)
	}
	if err == nil {
		r.learnScope(sess.Capabilities)
	}
	if err == nil && m != nil {
		m.Remember(string(ev.Client), ev.SessionID, sess.Generation)
		if ev.Kind == KindSessionStart {
			if f, ok := m.(fileMemory); ok {
				if err := f.prune(); err != nil {
					r.recordFailure(stageSessionFiles, err, nil)
				}
			}
		}
	}
	// The pid file and the Kimi session map follow a hub acceptance. Neither is part of the
	// sessions directory, so a failed write is not a session-files failure and must not mark the
	// run skipped.
	if err == nil && ev.Kind == KindSessionStart {
		r.recordGrokSession(ev)
		r.recordKimiSession(ev)
	}
	return sess, err
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func needsNetwork(ev Event) bool {
	switch ev.Kind {
	case KindSessionStart, KindSubagentStart, KindSessionEnd:
		return true
	case KindPrompt, KindPreInvocation:
		return promptEvent(ev) && carriesPromptContext(ev.Client)
	case KindStop:
		return ev.Client == ClientCursor
	case KindPreTool:
		return ev.ToolClass == ClassEdit
	default:
		return false
	}
}

func decisionFromTouch(t hubclient.TouchResponse) Decision {
	switch strings.ToLower(t.Action) {
	case "deny":
		return Decision{Action: ActionDeny, Reason: t.Message}
	case "ask":
		return Decision{Action: ActionAsk, Reason: t.Message}
	case "warn":
		return Decision{Action: ActionWarn, Reason: t.Message}
	default:
		if t.Message != "" {
			return Decision{Action: ActionAllow, Context: t.Message}
		}
		return Decision{Action: ActionAllow}
	}
}

func (r Runner) finish(state *runState, ev Event, out Output) Output {
	if ev.UnknownTool && ev.ToolName != "" {
		name := strings.ReplaceAll(strings.ReplaceAll(ev.ToolName, "\n", " "), "\r", " ")
		out.Stderr = append(out.Stderr, []byte("iris: unknown tool "+name+"\n")...)
		// stderr reaches nobody who keeps it, so doctor gets it from the log too (C4).
		state.observe("unknown tool " + toolLabel(ev.ToolName))
	}
	return out
}

// sessionEnv is how a hook confirms the real runner (D5).
// CLAUDE_CODE_SESSION_ID is deliberately absent: nested clients inherit it.
var sessionEnv = map[Client]string{
	ClientGrok:        "GROK_SESSION_ID",
	ClientCopilot:     "COPILOT_AGENT_SESSION_ID",
	ClientCursor:      "CURSOR_CONVERSATION_ID",
	ClientAntigravity: "ANTIGRAVITY_CONVERSATION_ID",
}

func (r Runner) crossLoad(sessionID string, requested Client) (Client, bool) {
	getenv := r.Getenv
	if getenv == nil || sessionID == "" {
		return "", false
	}
	var found []Client
	for _, c := range []Client{ClientGrok, ClientCopilot, ClientCursor, ClientAntigravity} {
		if getenv(sessionEnv[c]) == sessionID {
			found = append(found, c)
		}
	}
	if len(found) == 0 {
		return "", false
	}
	for _, c := range found {
		if c == requested {
			return "", false
		}
	}
	return found[0], true
}

func (r Runner) repoFor(ev Event) (gitinfo.Repo, error) {
	find := r.Find
	if find == nil {
		find = gitinfo.Find
	}
	var dirs []string
	if ev.CWD != "" {
		dirs = append(dirs, ev.CWD)
	}
	dirs = append(dirs, ev.Workspaces...)
	seen := map[string]bool{}
	var failure error
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		repo, err := find(dir)
		if err == nil {
			return repo, nil
		}
		if !errors.Is(err, gitinfo.ErrNotRepo) {
			failure = err
		}
	}
	if failure != nil {
		return gitinfo.Repo{}, failure
	}
	return gitinfo.Repo{}, gitinfo.ErrNotRepo
}

func (r Runner) sessionReq(ev Event, repo gitinfo.Repo) hubclient.SessionRequest {
	anc := r.ancestor()
	host := ""
	if r.Host != nil {
		host = r.Host()
	} else {
		host, _ = os.Hostname()
	}
	return hubclient.SessionRequest{
		CommonDir:        repo.CommonDir,
		Client:           string(ev.Client),
		SessionID:        ev.SessionID,
		Host:             host,
		CWD:              ev.CWD,
		Worktree:         repo.Worktree,
		WorktreeID:       repo.WorktreeID,
		RemoteURL:        repo.RemoteURL,
		RootCommit:       repo.RootCommit,
		Branch:           repo.Branch,
		ParentSessionID:  ev.ParentSessionID,
		ClientPID:        anc.PID,
		ClientPIDStartMS: anc.StartMS,
		SingleSession:    singleSession(ev.Client) || proc.SingleSession(anc, string(ev.Client)),
		Source:           "hook",
	}
}

func (r Runner) ancestor() proc.Ancestor {
	fn := r.Ancestor
	if fn == nil {
		fn = proc.Nearest
	}
	a, err := fn()
	if err != nil {
		return proc.Ancestor{}
	}
	return a
}

// singleSession names clients whose launchers need no command-line check.
// Codex exec is checked separately; app-server, Claude and Grok can multiplex.
func singleSession(c Client) bool {
	switch c {
	case ClientCursor, ClientAntigravity, ClientKimi:
		return true
	default:
		return false
	}
}

func (r Runner) claimPaths(repo gitinfo.Repo, ev Event) (paths []string, from, to string) {
	paths = repoPaths(repo.Worktree, ev.CWD, ev.Paths)
	if ev.RenameFrom != "" {
		if rel, err := relOne(repo.Worktree, ev.CWD, ev.RenameFrom); err == nil {
			from = rel
		}
	}
	if ev.RenameTo != "" {
		if rel, err := relOne(repo.Worktree, ev.CWD, ev.RenameTo); err == nil {
			to = rel
		}
	}
	if from == "" || to == "" {
		from, to = "", ""
	}
	return paths, from, to
}

func repoPaths(root, cwd string, paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		rel, err := relOne(root, cwd, p)
		if err != nil || rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	return out
}

func relOne(root, cwd, p string) (string, error) {
	if !isAbsPath(p) && cwd != "" {
		p = joinPath(cwd, p)
	}
	return gitinfo.Relative(root, p)
}

func isAbsPath(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && p[2] == '/'
}

func joinPath(base, rel string) string {
	base = strings.TrimRight(strings.ReplaceAll(base, "\\", "/"), "/")
	rel = strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	return base + "/" + rel
}

func readPayload(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, io.EOF
	}
	return io.ReadAll(io.LimitReader(r, 8<<20))
}

// CanarySession is the session id iris doctor's canary payload carries, and CanaryNonce the
// variable it sets in the hook command's environment.
const (
	CanarySession = "iris-canary"
	CanaryNonce   = "IRIS_DOCTOR_NONCE"
)

var canaryNonce = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// canaryReceipt is the line that proves to iris doctor that its canary reached Iris, not a
// wrapper that answers for it (gap review C3). It goes to stderr, never to the client's stdout,
// and only for the canary's own session with doctor's nonce, which is hex, so nothing else can
// ride on it.
func (r Runner) canaryReceipt(ev Event) []byte {
	nonce := ""
	if r.Getenv != nil {
		nonce = r.Getenv(CanaryNonce)
	}
	if ev.SessionID != CanarySession || !canaryNonce.MatchString(nonce) {
		return nil
	}
	return []byte("iris: canary " + nonce + "\n")
}

// crossLoadText is the observation for a hook of this client that ran in another client's
// session, which loaded this client's configuration (D5). The hook answered as that client.
func crossLoadText(runner Client) string {
	return "cross-load: ran in a session of " + string(runner)
}

// toolLabel keeps a tool name to what is safe in a log line doctor shows: a client's tool
// name is payload data, so anything outside a plain identifier becomes ?, within 64 bytes.
func toolLabel(name string) string {
	b := []byte(name)
	if len(b) > 64 {
		b = b[:64]
	}
	for i, c := range b {
		if !labelByte(c) {
			b[i] = '?'
		}
	}
	return string(b)
}

func labelByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_-.:/", c) >= 0
}
