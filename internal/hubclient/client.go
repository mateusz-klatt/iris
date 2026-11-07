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
	"strings"
)

// SessionRequest is the body of POST /v1/sessions.
type SessionRequest struct {
	CommonDir        string `json:"common_dir"`
	Generation       int    `json:"generation"`
	Client           string `json:"client"`
	SessionID        string `json:"session_id"`
	Host             string `json:"host"`
	CWD              string `json:"cwd"`
	Worktree         string `json:"worktree"`
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
	ExecutionID  string  `json:"execution_id"`
	Generation   int     `json:"generation"`
	Handle       string  `json:"handle"`
	RepositoryID string  `json:"repository_id"`
	Agent        Agent   `json:"agent"`
	LeaseMS      int64   `json:"lease_ms"`
	Peers        []Peer  `json:"peers"`
	Claims       []Claim `json:"claims"`
	Unread       int     `json:"unread"`
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
	Action  string `json:"action"`
	Message string `json:"message"`
}

// Client calls the hub. A non-nil HTTP client is used as-is; otherwise
// http.DefaultClient is used and the request context carries the deadline.
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
		return SessionResponse{}, fmt.Errorf("hub returned no execution id")
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
		return TouchResponse{}, fmt.Errorf("hub returned no action")
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
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	slurp, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var envelope struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(slurp, &envelope) == nil && envelope.Error != nil {
			return envelope.Error
		}
		return fmt.Errorf("hub %s: %s", path, res.Status)
	}
	if dest == nil || len(bytes.TrimSpace(slurp)) == 0 {
		return nil
	}
	if err := json.Unmarshal(slurp, dest); err != nil {
		return fmt.Errorf("hub %s: %w", path, err)
	}
	return nil
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
	RemoteURL  string   `json:"remote_url"`
	RootCommit string   `json:"root_commit"`
	Paths      []string `json:"paths"`
}

type CheckResponse struct {
	Message   string  `json:"message"`
	Conflicts []Claim `json:"conflicts"`
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
	err := c.post(ctx, "/v1/shell/reconcile", req, &out)
	return out, err
}

// IsCode reports whether err is a hub error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
