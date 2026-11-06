package hook

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
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
	ClientName string
	EventName  string
	Stdin      io.Reader
	Getenv     func(string) string
	Hub        Hub
	Find       func(dir string) (gitinfo.Repo, error)
	Ancestor   func() (proc.Ancestor, error)
	Host       func() string
	Deadline   time.Duration
}

// Run catches every failure, including panics, and always returns valid output
// for the named client. A deadline ends the call even if a dependency is stuck.
func (r Runner) Run() Output {
	allow := FallbackAllow(Canonical(r.ClientName))
	deadline := r.Deadline
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	ch := make(chan Output, 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- allow
			}
		}()
		ch <- r.execute(ctx)
	}()
	select {
	case out := <-ch:
		return out
	case <-ctx.Done():
		return allow
	}
}

func (r Runner) execute(ctx context.Context) Output {
	client := Canonical(r.ClientName)
	allow := FallbackAllow(client)
	payload, err := readPayload(r.Stdin)
	if err != nil || client == "" {
		return allow
	}
	ev, err := Decode(client, r.EventName, payload)
	if err != nil {
		if runner, cross := r.crossLoad(peekSessionID(payload), client); cross {
			return FallbackAllow(runner)
		}
		return allow
	}
	if runner, cross := r.crossLoad(ev.SessionID, client); cross {
		return FallbackAllow(runner)
	}
	repo, ok := r.repoFor(ev)
	if !ok {
		return r.finish(ev, Encode(ev, Decision{Action: ActionAllow}))
	}
	if !needsNetwork(ev) {
		return r.finish(ev, Encode(ev, Decision{Action: ActionAllow}))
	}
	if r.Hub == nil {
		return allow
	}
	switch ev.Kind {
	case KindSessionStart, KindSubagentStart:
		sess, err := r.Hub.RegisterSession(ctx, r.sessionReq(ev, repo))
		if err != nil {
			return allow
		}
		return r.finish(ev, Encode(ev, Decision{Action: ActionAllow, Context: SessionText(sess)}))
	case KindSessionEnd:
		sess, err := r.Hub.RegisterSession(ctx, r.sessionReq(ev, repo))
		if err != nil {
			return allow
		}
		// Session end is a best-effort speed-up. A missed call still expires the lease.
		_ = r.Hub.EndExecution(ctx, sess.ExecutionID, sess.Generation)
		return r.finish(ev, Encode(ev, Decision{Action: ActionAllow}))
	case KindPreTool:
		paths, from, to := r.claimPaths(repo, ev)
		if len(paths) == 0 {
			return r.finish(ev, Encode(ev, Decision{Action: ActionAllow}))
		}
		// Touch needs an execution id. The hook only has the client session id,
		// so it reattaches through POST /v1/sessions, which is defined as idempotent.
		sess, err := r.Hub.RegisterSession(ctx, r.sessionReq(ev, repo))
		if err != nil {
			return allow
		}
		touch, err := r.Hub.Touch(ctx, hubclient.TouchRequest{
			ExecutionID: sess.ExecutionID,
			Generation:  sess.Generation,
			ToolCallID:  ev.ToolCallID,
			Paths:       paths,
			RenameFrom:  from,
			RenameTo:    to,
		})
		if err != nil {
			return allow
		}
		return r.finish(ev, Encode(ev, decisionFromTouch(touch)))
	default:
		return r.finish(ev, Encode(ev, Decision{Action: ActionAllow}))
	}
}

func needsNetwork(ev Event) bool {
	switch ev.Kind {
	case KindSessionStart, KindSubagentStart, KindSessionEnd:
		return true
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

func (r Runner) finish(ev Event, out Output) Output {
	if ev.UnknownTool && ev.ToolName != "" {
		name := strings.ReplaceAll(strings.ReplaceAll(ev.ToolName, "\n", " "), "\r", " ")
		out.Stderr = append(out.Stderr, []byte("iris: unknown tool "+name+"\n")...)
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

func (r Runner) repoFor(ev Event) (gitinfo.Repo, bool) {
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
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		repo, err := find(dir)
		if err == nil {
			return repo, true
		}
	}
	return gitinfo.Repo{}, false
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
		Client:           string(ev.Client),
		SessionID:        ev.SessionID,
		Host:             host,
		CWD:              ev.CWD,
		Worktree:         repo.Worktree,
		RemoteURL:        repo.RemoteURL,
		RootCommit:       repo.RootCommit,
		Branch:           repo.Branch,
		ParentSessionID:  ev.ParentSessionID,
		ClientPID:        anc.PID,
		ClientPIDStartMS: anc.StartMS,
		SingleSession:    singleSession(ev.Client),
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

// singleSession is true only where one process hosts one session.
// Codex app-server, Claude and Grok were observed multiplexing, so they stay false.
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
