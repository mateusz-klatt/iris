package store

import (
	"crypto/sha256"
	"encoding/json"
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

// claimGroupV0113 is a v0.1.13 checkout group. Branch is not a field. The
// checkout path, id and common dir stay: they are the request's identity.
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

// legacyOperationHash is the digest v0.1.13 stored for this request. Branch on
// a group, and the fields that release did not have, are absent. The checkout
// path, id and common dir stay, which is what that release hashed.
func legacyOperationHash(route string, r Request) []byte {
	return hashV0113(route, requestV0113From(r))
}

// operationDigest is the retry hash. A grouped touch uses the v0.1.13 digest
// for both storage and lookup, so a display-only branch cannot miss the cache.
// Every other route hashes the request it was given.
func operationDigest(route string, request any) []byte {
	if route == "touch_files" {
		if r, ok := request.(Request); ok {
			return legacyOperationHash(route, r)
		}
	}
	raw, _ := json.Marshal(struct {
		Route   string
		Request any
	}{route, request})
	sum := sha256.Sum256(raw)
	return sum[:]
}
