// Package binding attributes long-lived clients using the positive D6 evidence chain.
package binding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

type Bound struct {
	ExecutionID  string          `json:"execution_id"`
	Generation   int             `json:"generation"`
	RepositoryID string          `json:"repository_id"`
	Agent        hubclient.Agent `json:"agent"`
	Rung         int             `json:"binding_rung"`
}
type Resolver struct {
	Hub               *hubclient.Client
	Client, Host, CWD string
	Getenv            func(string) string
	Ancestor          func() (proc.Ancestor, error)
	mu                sync.Mutex
	bound             Bound
	// lost is set when a heartbeat found the bound generation gone, until a fresh bind
	// succeeds. gone holds every Execution found lost: none is ever bound again, so a
	// rebind never speaks for a stale identity.
	lost bool
	gone map[string]bool
	// Grace is how long the first bind keeps looking for the session's hook's Execution
	// before this process registers one of its own. The MCP often starts before SessionStart
	// has registered the session, so registering at once would split it. New sets 15 s.
	Grace time.Duration
	// since is when the first bind began; own says the binding is an Execution this process
	// registered, which it gives up for the hook's once one appears.
	since time.Time
	own   bool
}

func New(h *hubclient.Client, client string) *Resolver {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	return &Resolver{Hub: h, Client: client, Host: host, CWD: cwd, Getenv: os.Getenv, Ancestor: proc.Nearest, Grace: 15 * time.Second}
}
func Key() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read always fills b or crashes the program.
	return hex.EncodeToString(b[:])
}

// Lost reports whether a heartbeat found this binding's generation gone at the hub and no
// fresh bind has replaced it yet. Only a heartbeat confirms a loss: a lost answer to any
// other call can be a session still recovering after a hub restart, which its next
// heartbeat reattaches.
func (r *Resolver) Lost() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lost
}

func IsCode(err error, code string) bool {
	var e *hubclient.Error
	return errors.As(err, &e) && e.Code == code
}

// errLost answers a call while the binding is lost and no fresh bind has succeeded.
var errLost = &hubclient.Error{Code: "lost", Message: "This session's Execution is gone at the hub. Iris binds afresh once the session has reattached: send any prompt, then retry."}

// Resolve returns the binding, making one when there is none. After a loss it binds afresh
// through the usual rungs, but never to an Execution found lost, however that comes back:
// IRIS_EXECUTION, a handle argument, or the hub's answer.
func (r *Resolver) Resolve(ctx context.Context, handle string) (Bound, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound.ExecutionID != "" && (handle == "" || handle == r.bound.ExecutionID) {
		return r.bound, nil
	}
	if r.gone[handle] {
		handle = ""
	}
	b, err := r.resolve(ctx, handle, "")
	b, err = r.orRegister(ctx, handle, b, err)
	if err == nil && r.gone[b.ExecutionID] {
		r.bound, b, err = Bound{}, Bound{}, errLost
	}
	if err != nil && r.lost {
		return Bound{}, errLost
	}
	if err == nil {
		r.lost = false
	}
	return b, err
}

// orRegister is the last resort when no rung finds the session: a session whose hooks never
// ran, as in an untrusted folder, still gets messaging. First it keeps looking, with backoff,
// for the grace period, because the hook usually registers within it; and never after a loss,
// when the hook is what reattaches. Only a process that could name its session registers: by
// its session id, or as a single-session client process (D6).
func (r *Resolver) orRegister(ctx context.Context, handle string, b Bound, err error) (Bound, error) {
	if !IsCode(err, "unbound") || len(r.gone) > 0 {
		return b, err
	}
	if r.since.IsZero() {
		r.since = time.Now()
	}
	for delay := 100 * time.Millisecond; time.Since(r.since) < r.Grace; delay = min(2*delay, 2*time.Second) {
		select {
		case <-ctx.Done():
			return b, err
		case <-time.After(min(delay, r.Grace-time.Since(r.since))):
		}
		if b, err = r.resolve(ctx, handle, ""); !IsCode(err, "unbound") {
			return b, err
		}
	}
	session := ""
	if variable := sessionVariable[r.Client]; variable != "" {
		session = r.Getenv(variable)
	}
	ancestor := r.ancestor()
	switch {
	case session != "":
		return r.register(ctx, session, ancestor, false, 2)
	case ancestor.PID > 0 && ancestor.StartMS > 0 && proc.SingleSession(ancestor, r.Client):
		return r.register(ctx, "mcp-"+fmt.Sprint(ancestor.PID)+"-"+fmt.Sprint(ancestor.StartMS), ancestor, true, 3)
	}
	return b, err
}

// register makes an Execution of this process's own, which it gives up as soon as the hook's
// appears (see adopt).
func (r *Resolver) register(ctx context.Context, session string, a proc.Ancestor, single bool, rung int) (Bound, error) {
	req := hubclient.SessionRequest{Client: r.Client, SessionID: session, Host: r.Host, CWD: r.CWD, Source: "mcp", ClientPID: a.PID, ClientPIDStartMS: a.StartMS, SingleSession: single}
	if repo, err := gitinfo.Find(r.CWD); err == nil {
		req.CommonDir = repo.CommonDir
		req.Worktree = repo.Worktree
		req.RemoteURL = repo.RemoteURL
		req.RootCommit = repo.RootCommit
		req.Branch = repo.Branch
	}
	response, err := r.Hub.RegisterSession(ctx, req)
	if err != nil {
		return Bound{}, err
	}
	b := Bound{ExecutionID: response.ExecutionID, Generation: response.Generation, RepositoryID: response.RepositoryID, Agent: response.Agent, Rung: rung}
	r.bound, r.own = b, true
	return b, nil
}

// adopt gives up this process's own Execution for the session's hook's, once that exists:
// a hook slower than the grace period must not leave the session split either.
func (r *Resolver) adopt(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.own {
		return
	}
	mine := r.bound
	b, err := r.resolve(ctx, "", mine.ExecutionID)
	if err != nil {
		r.bound = mine
		return
	}
	_ = r.Hub.EndExecution(ctx, mine.ExecutionID, mine.Generation)
	r.bound, r.own = b, false
}

// ancestor is the nearest client process, or none when there is no way to look.
func (r *Resolver) ancestor() proc.Ancestor {
	if r.Ancestor == nil {
		return proc.Ancestor{}
	}
	a, _ := r.Ancestor()
	return a
}

var sessionVariable = map[string]string{"claude": "CLAUDE_CODE_SESSION_ID", "copilot": "COPILOT_AGENT_SESSION_ID", "grok": "GROK_SESSION_ID"}

// resolve tries the rungs in order and only binds. exclude names an Execution to skip.
func (r *Resolver) resolve(ctx context.Context, handle, exclude string) (Bound, error) {
	req := map[string]any{"client": r.Client, "host": r.Host, "cwd": r.CWD}
	if exclude != "" {
		req["exclude"] = exclude
	}
	bind := func(rung int) (Bound, error) {
		var b Bound
		err := r.Hub.Do(ctx, "POST", "/v1/bind", req, &b)
		if err == nil {
			b.Rung = rung
			r.bound = b
		}
		return b, err
	}
	if ex := r.Getenv("IRIS_EXECUTION"); ex != "" && !r.gone[ex] {
		req["execution"] = ex
		return bind(1)
	}
	variable := sessionVariable[r.Client]
	session := ""
	if variable != "" {
		session = r.Getenv(variable)
	}
	// The session's hook is its registrar. On resume Claude Code gives an MCP server another
	// CLAUDE_CODE_SESSION_ID than its hooks, so a session id with nothing bound to it falls
	// through to the client process, which the hook's Execution records.
	if session != "" {
		req["session_id"] = session
		b, err := bind(2)
		if err == nil || !IsCode(err, "unbound") {
			return b, err
		}
		delete(req, "session_id")
	}
	ancestor := r.ancestor()
	if ancestor.PID > 0 && ancestor.StartMS > 0 {
		req["client_pid"] = ancestor.PID
		req["client_pid_start_ms"] = ancestor.StartMS
		b, err := bind(3)
		if err == nil {
			return b, nil
		}
		if !IsCode(err, "unbound") && !IsCode(err, "ambiguous") {
			return b, err
		}
		if handle != "" {
			req["execution"] = handle
			return bind(4)
		}
		return b, err
	}
	if handle != "" {
		req["execution"] = handle
		return bind(4)
	}
	return Bound{}, &hubclient.Error{Code: "unbound", Message: "Pass the execution handle from SessionStart in the execution argument, or restart with IRIS_EXECUTION set. Iris cannot identify this session safely."}
}

// Maintain stops before any renewal after parent loss. The process owner also cancels
// this context when the transport reaches EOF. A generation the heartbeat finds lost is
// dropped, never renewed, and the process keeps running: the next Resolve binds afresh.
func (r *Resolver) Maintain(ctx context.Context, source string, interval time.Duration, parentAlive func() bool, onStop func()) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !parentAlive() {
			onStop()
			return
		}
		r.adopt(ctx)
		r.mu.Lock()
		b := r.bound
		r.mu.Unlock()
		if b.ExecutionID == "" {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.Hub.Do(call, "POST", "/v1/executions/"+b.ExecutionID+"/heartbeat", map[string]any{"generation": b.Generation, "source": source}, nil)
		cancel()
		if IsCode(err, "lost") {
			r.mu.Lock()
			if r.gone == nil {
				r.gone = map[string]bool{}
			}
			r.gone[b.ExecutionID] = true
			if r.bound.ExecutionID == b.ExecutionID {
				r.bound, r.lost = Bound{}, true
			}
			r.mu.Unlock()
		}
	}
}
