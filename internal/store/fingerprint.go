package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
)

// requestV0113 is the Request shape v0.1.13 hashed. LastMessageID and
// ClaimAutoScope are absent: that release discarded them. Claim groups have no
// branch. Field order is the order encoding/json wrote then, so a stored digest
// still matches.
type requestV0113 struct {
	CommonDir        string            `json:"common_dir"`
	Status           map[string]string `json:"status"`
	Scope            *ShellScope       `json:"scope,omitempty"`
	ReplyTo          string            `json:"reply_to"`
	Q                string            `json:"q"`
	Agent            string            `json:"agent"`
	DryRun           bool              `json:"dry_run"`
	Before           string            `json:"before"`
	CheckedAt        string            `json:"checked_at"`
	Clients          []map[string]any  `json:"clients"`
	Client           string            `json:"client"`
	SessionID        string            `json:"session_id"`
	Host             string            `json:"host"`
	Cwd              string            `json:"cwd"`
	Worktree         string            `json:"worktree"`
	WorktreeID       string            `json:"worktree_id"`
	Receipt          bool              `json:"receipt"`
	Receipts         []Receipt         `json:"receipts"`
	RemoteURL        string            `json:"remote_url"`
	RootCommit       string            `json:"root_commit"`
	Branch           string            `json:"branch"`
	ParentSessionID  string            `json:"parent_session_id"`
	ClientPID        int64             `json:"client_pid"`
	ClientPIDStartMS int64             `json:"client_pid_start_ms"`
	SingleSession    bool              `json:"single_session"`
	Source           string            `json:"source"`
	Execution        string            `json:"execution"`
	Exclude          string            `json:"exclude"`
	HandOver         string            `json:"hand_over"`
	ExecutionID      string            `json:"execution_id"`
	Generation       int64             `json:"generation"`
	Key              string            `json:"key"`
	ToolCallID       string            `json:"tool_call_id"`
	Paths            []string          `json:"paths"`
	RenameFrom       string            `json:"rename_from"`
	RenameTo         string            `json:"rename_to"`
	Groups           []claimGroupV0113 `json:"groups,omitempty"`
	Checkout         *claimGroupV0113  `json:"checkout,omitempty"`
	Kind             string            `json:"kind"`
	Path             string            `json:"path"`
	RepoWide         bool              `json:"repo_wide"`
	TTLS             int64             `json:"ttl_s"`
	Fence            int64             `json:"fence"`
	All              bool              `json:"all"`
	Token            string            `json:"token"`
	To               []string          `json:"to"`
	Subject          string            `json:"subject"`
	Body             string            `json:"body"`
	Attempt          int64             `json:"attempt"`
	Handled          bool              `json:"handled"`
	RepositoryID     string            `json:"repository_id"`
	FromRepository   string            `json:"from_repository_id,omitempty"`
	IntoRepository   string            `json:"into_repository_id,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	After            int64             `json:"after"`
	Limit            int               `json:"limit"`
	ConsumerID       string            `json:"consumer_id"`
	Stream           string            `json:"stream"`
	Attention        bool              `json:"attention"`
	RetryGeneration  int64             `json:"retry_generation,omitempty"`
}

// claimGroupV0113 is a v0.1.13 checkout group. Branch is not a field.
type claimGroupV0113 struct {
	Worktree   string   `json:"worktree"`
	WorktreeID string   `json:"worktree_id,omitempty"`
	CommonDir  string   `json:"common_dir,omitempty"`
	RemoteURL  string   `json:"remote_url,omitempty"`
	RootCommit string   `json:"root_commit,omitempty"`
	Paths      []string `json:"paths"`
	Prefix     string   `json:"prefix,omitempty"`
}

func groupV0113(g ClaimGroup) claimGroupV0113 {
	return claimGroupV0113{Worktree: g.Worktree, WorktreeID: g.WorktreeID, CommonDir: g.CommonDir, RemoteURL: g.RemoteURL, RootCommit: g.RootCommit, Paths: g.Paths, Prefix: g.Prefix}
}

func requestV0113From(r Request) requestV0113 {
	out := requestV0113{
		CommonDir: r.CommonDir, Status: r.Status, Scope: r.Scope, ReplyTo: r.ReplyTo, Q: r.Q, Agent: r.Agent, DryRun: r.DryRun,
		Before: r.Before, CheckedAt: r.CheckedAt, Clients: r.Clients, Client: r.Client, SessionID: r.SessionID, Host: r.Host,
		Cwd: r.Cwd, Worktree: r.Worktree, WorktreeID: r.WorktreeID, Receipt: r.Receipt, Receipts: r.Receipts, RemoteURL: r.RemoteURL,
		RootCommit: r.RootCommit, Branch: r.Branch, ParentSessionID: r.ParentSessionID, ClientPID: r.ClientPID, ClientPIDStartMS: r.ClientPIDStartMS,
		SingleSession: r.SingleSession, Source: r.Source, Execution: r.Execution, Exclude: r.Exclude, HandOver: r.HandOver,
		ExecutionID: r.ExecutionID, Generation: r.Generation, Key: r.Key, ToolCallID: r.ToolCallID, Paths: r.Paths,
		RenameFrom: r.RenameFrom, RenameTo: r.RenameTo, Kind: r.Kind, Path: r.Path, RepoWide: r.RepoWide, TTLS: r.TTLS,
		Fence: r.Fence, All: r.All, Token: r.Token, To: r.To, Subject: r.Subject, Body: r.Body, Attempt: r.Attempt,
		Handled: r.Handled, RepositoryID: r.RepositoryID, FromRepository: r.FromRepository, IntoRepository: r.IntoRepository,
		Aliases: r.Aliases, After: r.After, Limit: r.Limit, ConsumerID: r.ConsumerID, Stream: r.Stream, Attention: r.Attention,
		RetryGeneration: r.RetryGeneration,
	}
	if len(r.Groups) > 0 {
		out.Groups = make([]claimGroupV0113, len(r.Groups))
		for i, g := range r.Groups {
			out.Groups[i] = groupV0113(g)
		}
	}
	if r.Checkout != nil {
		c := groupV0113(*r.Checkout)
		out.Checkout = &c
	}
	return out
}

func hashV0113(route string, r requestV0113) []byte {
	raw, _ := json.Marshal(struct {
		Route   string
		Request requestV0113
	}{route, r})
	sum := sha256.Sum256(raw)
	return sum[:]
}

// legacyOperationHash is the digest v0.1.13 stored for this request. Branch and
// the fields that release did not have are absent. The path and common dir stay,
// which is what that release hashed.
func legacyOperationHash(route string, r Request) []byte {
	return hashV0113(route, requestV0113From(r))
}

// operationHash is the digest stored for a new operation. An unidentified
// checkout keeps its path, because that path can choose the repository. A
// checkout this host has already recorded, with the same remote and root, is
// one checkout: another spelling of its path or common dir is not a new request.
func (t *transaction) operationHash(e execution, route string, r Request) ([]byte, error) {
	prepared := requestV0113From(r)
	for i := range prepared.Groups {
		if err := t.normalizeEstablished(&prepared.Groups[i], e); err != nil {
			return nil, err
		}
	}
	if prepared.Checkout != nil {
		c := *prepared.Checkout
		if err := t.normalizeEstablished(&c, e); err != nil {
			return nil, err
		}
		prepared.Checkout = &c
	}
	return hashV0113(route, prepared), nil
}

// normalizeEstablished rewrites a group to the path this host first named, and
// drops the common-dir spelling, only when the worktree id already names that
// checkout for the same remote and root. Without an id, or when the id now
// names another history, the path stays so two checkouts cannot share a hash.
func (t *transaction) normalizeEstablished(g *claimGroupV0113, e execution) error {
	if g.WorktreeID == "" || g.Worktree == "" {
		return nil
	}
	var first, remote, root string
	err := t.row("SELECT worktree,remote_url,root_commit FROM WorktreeAlias WHERE host_id=? AND worktree_id=?", e.Host, g.WorktreeID).Scan(&first, &remote, &root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if remote != g.RemoteURL || root != g.RootCommit {
		return nil
	}
	if sameWorktree(first, e.Worktree) {
		g.Worktree = first
		g.CommonDir = ""
		return nil
	}
	known, err := t.knownCheckout(e, first, g)
	if err != nil || !known {
		return err
	}
	g.Worktree = first
	g.CommonDir = ""
	return nil
}

// knownCheckout reports whether canonical is a checkout of a repository this hub
// can already name. An ambiguous or unknown checkout is not established, so its
// path and common dir stay part of the request.
func (t *transaction) knownCheckout(e execution, canonical string, g *claimGroupV0113) (bool, error) {
	r := Request{Host: e.Host, Cwd: canonical, Worktree: canonical, WorktreeID: g.WorktreeID, CommonDir: g.CommonDir, RemoteURL: g.RemoteURL, RootCommit: g.RootCommit}
	repo, ambiguous, err := t.resolveRepository(r)
	if err != nil {
		return false, err
	}
	return repo != "" && !ambiguous, nil
}
