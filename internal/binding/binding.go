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
	lost              bool
}

func New(h *hubclient.Client, client string) *Resolver {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	return &Resolver{Hub: h, Client: client, Host: host, CWD: cwd, Getenv: os.Getenv, Ancestor: proc.Nearest}
}
func Key() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read always fills b or crashes the program.
	return hex.EncodeToString(b[:])
}
func IsCode(err error, code string) bool {
	var e *hubclient.Error
	return errors.As(err, &e) && e.Code == code
}
func (r *Resolver) Resolve(ctx context.Context, handle string) (Bound, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lost {
		return Bound{}, &hubclient.Error{Code: "lost", Message: "Execution generation lost; restart the Iris MCP process after the session reattaches"}
	}
	if r.bound.ExecutionID != "" && (handle == "" || handle == r.bound.ExecutionID) {
		return r.bound, nil
	}
	req := map[string]any{"client": r.Client, "host": r.Host, "cwd": r.CWD}
	bind := func(rung int) (Bound, error) {
		var b Bound
		err := r.Hub.Do(ctx, "POST", "/v1/bind", req, &b)
		if err == nil {
			b.Rung = rung
			r.bound = b
		}
		return b, err
	}
	if ex := r.Getenv("IRIS_EXECUTION"); ex != "" {
		req["execution"] = ex
		return bind(1)
	}
	variable := map[string]string{"claude": "CLAUDE_CODE_SESSION_ID", "copilot": "COPILOT_AGENT_SESSION_ID", "grok": "GROK_SESSION_ID"}[r.Client]
	session := ""
	if variable != "" {
		session = r.Getenv(variable)
	}
	if session != "" {
		req["session_id"] = session
		b, err := bind(2)
		if err == nil {
			return b, nil
		}
		if !IsCode(err, "unbound") {
			return b, err
		}
		return r.register(ctx, session, proc.Ancestor{}, false, 2)
	}
	ancestor, _ := r.Ancestor()
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
		if IsCode(err, "ambiguous") {
			return b, err
		}
		if proc.SingleSession(ancestor, r.Client) {
			return r.register(ctx, "mcp-"+fmt.Sprint(ancestor.PID)+"-"+fmt.Sprint(ancestor.StartMS), ancestor, true, 3)
		}
	}
	if handle != "" {
		req["execution"] = handle
		return bind(4)
	}
	return Bound{}, &hubclient.Error{Code: "unbound", Message: "Pass the execution handle from SessionStart in the execution argument, or restart with IRIS_EXECUTION set. Iris cannot identify this session safely."}
}
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
	r.bound = b
	return b, nil
}

// Maintain stops before any renewal after parent loss. The process owner also cancels
// this context when the transport reaches EOF. A lost generation is never rebound.
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
		r.mu.Lock()
		b := r.bound
		lost := r.lost
		r.mu.Unlock()
		if lost {
			return
		}
		if b.ExecutionID == "" {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.Hub.Do(call, "POST", "/v1/executions/"+b.ExecutionID+"/heartbeat", map[string]any{"generation": b.Generation, "source": source}, nil)
		cancel()
		if IsCode(err, "lost") {
			r.mu.Lock()
			r.lost = true
			r.mu.Unlock()
			onStop()
			return
		}
	}
}
