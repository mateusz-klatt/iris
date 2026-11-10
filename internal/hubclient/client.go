package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// SessionRequest is the body of POST /v1/sessions.
type SessionRequest struct {
	CommonDir  string `json:"common_dir"`
	Generation int    `json:"generation"`
	// RetryGeneration reports discarded proof for hub diagnostics, never authority.
	RetryGeneration  int    `json:"retry_generation,omitempty"`
	Client           string `json:"client"`
	SessionID        string `json:"session_id"`
	Host             string `json:"host"`
	CWD              string `json:"cwd"`
	Worktree         string `json:"worktree"`
	WorktreeID       string `json:"worktree_id"` // device and inode of the root; see gitinfo.Repo
	RemoteURL        string `json:"remote_url"`
	RootCommit       string `json:"root_commit"`
	Branch           string `json:"branch"`
	ParentSessionID  string `json:"parent_session_id"`
	ClientPID        int    `json:"client_pid"`
	ClientPIDStartMS int64  `json:"client_pid_start_ms"`
	SingleSession    bool   `json:"single_session"`
	Source           string `json:"source"`
}

// SessionResponse is the registration result.
type SessionResponse struct {
	ExecutionID  string    `json:"execution_id"`
	Generation   int       `json:"generation"`
	Handle       string    `json:"handle"`
	RepositoryID string    `json:"repository_id"`
	Agent        Agent     `json:"agent"`
	LeaseMS      int64     `json:"lease_ms"`
	Peers        []Peer    `json:"peers"`
	Claims       []Claim   `json:"claims"`
	Unread       int       `json:"unread"`
	Attention    Attention `json:"attention"`
	// Capabilities names what the hub accepts beyond version one; see ShellScopeCapable.
	Capabilities []string `json:"capabilities"`
}

// Attention is what the session has not been told yet: unacknowledged deliveries and paths
// it was refused that are now free. Monitored means its iris watch beat recently, so the
// monitor, not a hook, tells it.
type Attention struct {
	Unread    int  `json:"unread"`
	Available int  `json:"available"`
	Monitored bool `json:"monitored"`
}

// Agent is the mailbox named in a session response.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Peer is another agent on the same repository.
type Peer struct {
	Name     string `json:"name"`
	Client   string `json:"client"`
	State    string `json:"state"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
}

// Claim is a live claim returned with the session.
type Claim struct {
	Fence     int64  `json:"fence"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Holder    string `json:"holder"`
	Origin    string `json:"origin"`
	ExpiresAt string `json:"expires_at"`
}

// TouchRequest is the body of POST /v1/claims/touch.
type TouchRequest struct {
	ExecutionID string   `json:"execution_id"`
	Generation  int      `json:"generation"`
	ToolCallID  string   `json:"tool_call_id"`
	Paths       []string `json:"paths"`
	RenameFrom  string   `json:"rename_from"`
	RenameTo    string   `json:"rename_to"`
}

// TouchResponse is the pre-edit result. Action is allow, deny, ask, or warn.
type TouchResponse struct {
	Action    string    `json:"action"`
	Message   string    `json:"message"`
	Attention Attention `json:"attention"`
}

// Client calls the hub. A non-nil HTTP client is used as-is; otherwise defaultClient, whose
// transport drops dead pooled connections, is used and the request context carries the
// deadline.
type Client struct {
	Config Config
	HTTP   *http.Client
}

// RegisterSession calls POST /v1/sessions.
func (c *Client) RegisterSession(ctx context.Context, req SessionRequest) (SessionResponse, error) {
	var resp SessionResponse
	if err := c.post(ctx, "/v1/sessions", req, &resp); err != nil {
		return SessionResponse{}, err
	}
	if resp.ExecutionID == "" {
		return SessionResponse{}, ErrNoExecution
	}
	return resp, nil
}

// Touch calls POST /v1/claims/touch.
func (c *Client) Touch(ctx context.Context, req TouchRequest) (TouchResponse, error) {
	var resp TouchResponse
	if err := c.post(ctx, "/v1/claims/touch", req, &resp); err != nil {
		return TouchResponse{}, err
	}
	if resp.Action == "" {
		return TouchResponse{}, ErrNoAction
	}
	return resp, nil
}

// EndExecution calls POST /v1/executions/{id}/end.
func (c *Client) EndExecution(ctx context.Context, id string, generation int) error {
	if id == "" {
		return fmt.Errorf("missing execution id")
	}
	path := "/v1/executions/" + url.PathEscape(id) + "/end"
	return c.post(ctx, path, map[string]int{"generation": generation}, nil)
}

// HandOverExecution ends id and hands its unacknowledged mail to the live Execution to, of the
// same host and client: the session's hook's, when an MCP gives up one it had to register.
func (c *Client) HandOverExecution(ctx context.Context, id string, generation int, to string) error {
	path := "/v1/executions/" + url.PathEscape(id) + "/end"
	return c.post(ctx, path, map[string]any{"generation": generation, "hand_over": to}, nil)
}

func (c *Client) post(ctx context.Context, path string, body, dest any) error {
	return c.Do(ctx, http.MethodPost, path, body, dest)
}

// Error preserves the hub's typed failure for binding and liveness decisions.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Do issues an authenticated JSON request. Callers keep retry keys stable across retries.
func (c *Client) Do(ctx context.Context, method, path string, body, dest any) error {
	if c == nil || c.Config == nil {
		return fmt.Errorf("hub client is not configured")
	}
	base := strings.TrimRight(c.Config.HubURL(), "/")
	if err := ValidateURL(base); err != nil {
		return err
	}
	token, err := c.Config.Token()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := NoRedirects(c.HTTP).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	// One byte past the limit is read, so a response that does not fit is told apart from one
	// that only ends exactly there.
	slurp, err := io.ReadAll(io.LimitReader(res.Body, responseLimit+1))
	if err != nil {
		return err
	}
	if len(slurp) > responseLimit {
		return fmt.Errorf("hub %s: %w", path, ErrResponseTooLarge)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var envelope struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(slurp, &envelope) == nil && envelope.Error != nil {
			return envelope.Error
		}
		return &StatusError{Path: path, Code: res.StatusCode, Status: res.Status}
	}
	if dest == nil || len(bytes.TrimSpace(slurp)) == 0 {
		return nil
	}
	if err := json.Unmarshal(slurp, dest); err != nil {
		return fmt.Errorf("hub %s: %w", path, err)
	}
	return nil
}

// responseLimit is the most of a hub response a client reads. The hub keeps its pages under it.
const responseLimit = 1 << 20

// ErrResponseTooLarge is a hub response longer than responseLimit. It is refused whole: a
// response cut short would only fail later as broken JSON, without saying why.
var ErrResponseTooLarge = errors.New("the hub's response is larger than the 1 MiB a client reads")

// StatusError is an error status from the hub, or from a proxy in front of it, that carries no
// hub error envelope: a 502 while the hub restarts, for example. It is typed so a hook can log
// the status code alone.
type StatusError struct {
	Path   string
	Code   int
	Status string
}

func (e *StatusError) Error() string { return "hub " + e.Path + ": " + e.Status }

// ErrNoExecution is a session answer that named no Execution.
var ErrNoExecution = errors.New("hub returned no execution id")

// ErrNoAction is a touch answer that named no action.
var ErrNoAction = errors.New("hub returned no action")

// ErrRedirect is a hub answer that tried to send the request elsewhere.
var ErrRedirect = errors.New("the hub answered with a redirect, which Iris does not follow: set the hub URL to the final address")

// NoRedirects returns a copy of hc, or of the default client, that refuses every
// redirect. Every hub request carries the bearer token, and Go keeps that header
// across a redirect to the same host, including one from https to http, so only
// the first URL is ever validated (ValidateURL). The hub never redirects an API
// call, so refusing all of them costs nothing.
func NoRedirects(hc *http.Client) *http.Client {
	if hc == nil {
		hc = defaultClient
	}
	refusing := *hc
	refusing.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrRedirect }
	return &refusing
}

// ValidateURL accepts https, and http only for the literal hosts 127.0.0.1 and ::1.
// localhost is refused: it is not literally loopback.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("hub url: %w", err)
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("hub url has no host")
		}
		return nil
	case "http":
		switch u.Hostname() {
		case "127.0.0.1", "::1":
			return nil
		default:
			return fmt.Errorf("refusing cleartext hub host %q", u.Hostname())
		}
	default:
		return fmt.Errorf("unsupported hub scheme %q", u.Scheme)
	}
}

// CheckRequest identifies a worktree without creating an Execution or taking claims.
type CheckRequest struct {
	CommonDir  string   `json:"common_dir"`
	Host       string   `json:"host"`
	Worktree   string   `json:"worktree"`
	WorktreeID string   `json:"worktree_id"`
	RemoteURL  string   `json:"remote_url"`
	RootCommit string   `json:"root_commit"`
	Paths      []string `json:"paths"`
}

type CheckResponse struct {
	Message   string  `json:"message"`
	Conflicts []Claim `json:"conflicts"`
	// CasePolicy is the repository's case policy, folded or sensitive (ADR 003).
	CasePolicy string `json:"case_policy,omitempty"`
}

func (c *Client) CheckPaths(ctx context.Context, req CheckRequest) (CheckResponse, error) {
	var out CheckResponse
	err := c.post(ctx, "/v1/guard/check", req, &out)
	return out, err
}

// ReconcileRequest carries an observed porcelain snapshot, never shell instructions.
type ReconcileRequest struct {
	ExecutionID string            `json:"execution_id"`
	Generation  int               `json:"generation"`
	Kind        string            `json:"kind"` // before records a baseline; after compares and records it.
	Status      map[string]string `json:"status"`
}

func (c *Client) Reconcile(ctx context.Context, req ReconcileRequest) (CheckResponse, error) {
	var out CheckResponse
	err := c.post(ctx, reconcilePath, req, &out)
	return out, err
}

// ShellSessionRequest names the session instead of an Execution. The hub registers it, with
// the remembered generation as its recovery proof, and reconciles in the same transaction, so
// a shell hook makes one round trip, not two. A hub from before this answers it with an error.
type ShellSessionRequest struct {
	SessionRequest
	Kind   string            `json:"kind"`
	Status map[string]string `json:"status"`
}

// ShellResponse is a reconcile result with the Execution the session registered as, which the
// hook remembers as its next proof.
type ShellResponse struct {
	CheckResponse
	Session struct {
		ExecutionID  string   `json:"execution_id"`
		Generation   int      `json:"generation"`
		Capabilities []string `json:"capabilities"`
	} `json:"session"`
}

func (c *Client) ReconcileSession(ctx context.Context, req ShellSessionRequest) (ShellResponse, error) {
	var out ShellResponse
	err := c.post(ctx, reconcilePath, req, &out)
	return out, err
}

// ShellScopeCapable is whether a registration answer names shell_scope: that hub takes reduced
// snapshots on POST /v1/shell/scoped (ADR 018).
func ShellScopeCapable(capabilities []string) bool {
	return slices.Contains(capabilities, "shell_scope")
}

// Scope is what a reduced snapshot did not look into: its status ran with
// --ignore-submodules=dirty, and Submodules are the gitlink paths of the index it read, in byte
// order. The hub takes at most 500, and refuses a request past its body limit as too_large, so a
// worktree with very many long submodule paths gets no reduced snapshot.
type Scope struct {
	Mode       string   `json:"mode"`
	Submodules []string `json:"submodules"`
}

// ScopedRequest is a reduced snapshot for an Execution.
type ScopedRequest struct {
	ReconcileRequest
	Scope Scope `json:"scope"`
}

// ScopedSessionRequest is a reduced snapshot that names its session, as ShellSessionRequest does.
type ScopedSessionRequest struct {
	ShellSessionRequest
	Scope Scope `json:"scope"`
}

// ReconcileScoped sends a reduced snapshot for an Execution. A hub from before the route answers
// not_found, so it never reads the snapshot as a full one.
func (c *Client) ReconcileScoped(ctx context.Context, req ScopedRequest) (CheckResponse, error) {
	var out CheckResponse
	err := c.post(ctx, scopedPath, req, &out)
	return out, err
}

// ReconcileScopedSession is ReconcileScoped in the one-call form.
func (c *Client) ReconcileScopedSession(ctx context.Context, req ScopedSessionRequest) (ShellResponse, error) {
	var out ShellResponse
	err := c.post(ctx, scopedPath, req, &out)
	return out, err
}

// IsCode reports whether err is a hub error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
