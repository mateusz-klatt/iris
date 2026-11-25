package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
)

// SuccessionProofCapability names a stage 1b hub's support for classified binding candidates,
// compatibility opt-in, and POST /v1/sessions/succeed/exact (ADR 039).
const SuccessionProofCapability = "succession_proof"

// SuccessionProofCapable is whether a hub reply names succession_proof in capabilities.
func SuccessionProofCapable(capabilities []string) bool {
	return slices.Contains(capabilities, SuccessionProofCapability)
}

// NullableString decodes an explicit JSON string or JSON null while distinguishing omission.
// All fields are comparable so decoded classifications remain usable inside comparable binding keys.
type NullableString struct {
	Known bool
	Valid bool
	Value string
}

// UnmarshalJSON records whether a string field was omitted, set to null, or populated.
func (n *NullableString) UnmarshalJSON(data []byte) error {
	n.Known = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		n.Valid = false
		n.Value = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	n.Valid = true
	n.Value = s
	return nil
}

// OptionalBool decodes a JSON boolean while rejecting explicit null values and tracking omission.
type OptionalBool struct {
	Known bool
	Value bool
}

// UnmarshalJSON records whether a boolean field was present and rejects JSON null.
func (b *OptionalBool) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("hubclient: boolean field cannot be null")
	}
	var v bool
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	b.Known = true
	b.Value = v
	return nil
}

// Provenance holds the hub-emitted creation lineage and hook visibility metadata (ADR 039).
type Provenance struct {
	PriorExecutionID NullableString `json:"prior_execution_id"`
	CreationSource   NullableString `json:"creation_source"`
	CreationProof    NullableString `json:"creation_proof"`
	HookSeen         OptionalBool   `json:"hook_seen"`
}

// BindRequest is the body of POST /v1/bind.
type BindRequest struct {
	Client           string `json:"client"`
	Host             string `json:"host"`
	SessionID        string `json:"session_id,omitempty"`
	Execution        string `json:"execution,omitempty"`
	Exclude          string `json:"exclude,omitempty"`
	ClientPID        int64  `json:"client_pid,omitempty"`
	ClientPIDStartMS int64  `json:"client_pid_start_ms,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	SuccessionProof  bool   `json:"succession_proof,omitempty"`
}

// HeartbeatRequest is the body of POST /v1/executions/{id}/heartbeat.
type HeartbeatRequest struct {
	Generation      int    `json:"generation"`
	Source          string `json:"source"`
	SuccessionProof bool   `json:"succession_proof,omitempty"`
}

// SucceedRequest is the body of POST /v1/sessions/succeed/exact.
type SucceedRequest struct {
	Client           string `json:"client"`
	Host             string `json:"host"`
	SessionID        string `json:"session_id"`
	Source           string `json:"source"`
	PriorGeneration  int    `json:"prior_generation"`
	PriorExecutionID string `json:"prior_execution_id"`
	EndedThrough     int    `json:"ended_through,omitempty"`
	ClientPID        int64  `json:"client_pid,omitempty"`
	ClientPIDStartMS int64  `json:"client_pid_start_ms,omitempty"`
	SingleSession    bool   `json:"single_session,omitempty"`
}

// Candidate decodes a session, bind, succeed, or heartbeat answer together with its
// stage 1b classification metadata.
type Candidate struct {
	ExecutionID  string   `json:"execution_id"`
	Generation   int      `json:"generation"`
	Handle       string   `json:"handle,omitempty"`
	RepositoryID string   `json:"repository_id,omitempty"`
	Agent        Agent    `json:"agent"`
	SessionID    string   `json:"session_id,omitempty"`
	Worktree     string   `json:"worktree,omitempty"`
	OutsideGit   bool     `json:"outside_git,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Match        string   `json:"match,omitempty"`
	LeaseMS      int64    `json:"lease_ms,omitempty"`
	Provenance
}

// CandidateKind classifies an Execution returned by the hub.
type CandidateKind string

const (
	// CandidateUnclassified comes from a hub that does not advertise succession_proof.
	CandidateUnclassified CandidateKind = "unclassified"
	// CandidateLegacy identifies a pre-migration-029 row whose provenance columns are SQL NULL.
	CandidateLegacy CandidateKind = "legacy"
	// CandidateOrdinary identifies a non-shim-succession row with validated provenance.
	CandidateOrdinary CandidateKind = "ordinary"
	// CandidateShimSuccession identifies a child created by POST /v1/sessions/succeed/exact.
	CandidateShimSuccession CandidateKind = "shim_succession"
)

// Classification summarizes the hub's validated provenance classification for a Candidate.
type Classification struct {
	Kind             CandidateKind
	PriorExecutionID string
	PriorGeneration  int
	CreationSource   string
	CreationProof    string
	HookSeen         bool
}

// IsHooklessSuccession reports whether c classifies a shim-created succession child that has
// not yet been registered by a genuine hook.
func (c Classification) IsHooklessSuccession() bool {
	return c.Kind == CandidateShimSuccession && !c.HookSeen
}

// HookCreatedFrom reports whether c classifies a hook-registered Execution whose immediate
// predecessor is (priorExecutionID, priorGeneration).
func (c Classification) HookCreatedFrom(priorExecutionID string, priorGeneration int) bool {
	return c.Kind == CandidateOrdinary &&
		c.HookSeen &&
		c.CreationSource == "hook" &&
		priorExecutionID != "" &&
		c.PriorExecutionID == priorExecutionID &&
		priorGeneration > 0 &&
		c.PriorGeneration == priorGeneration
}

// IsLeaseOnly reports whether c is a legacy heartbeat reply carrying only lease renewal fields
// without capabilities, identity, or stage 1b classification metadata.
func (c Candidate) IsLeaseOnly() bool {
	return !SuccessionProofCapable(c.Capabilities) &&
		c.ExecutionID == "" &&
		c.Generation == 0 &&
		!c.PriorExecutionID.Known &&
		!c.CreationSource.Known &&
		!c.CreationProof.Known &&
		!c.HookSeen.Known
}

// Classify validates c's provenance fields when the hub advertises succession_proof. Replies from
// hubs without succession_proof return CandidateUnclassified with any observed hook_seen value.
func (c Candidate) Classify() (Classification, error) {
	if !SuccessionProofCapable(c.Capabilities) {
		return Classification{
			Kind:     CandidateUnclassified,
			HookSeen: c.HookSeen.Value,
		}, nil
	}
	if c.ExecutionID == "" || c.Generation <= 0 {
		return Classification{}, errors.New("hubclient: classified candidate requires execution_id and positive generation")
	}
	if !c.HookSeen.Known || !c.PriorExecutionID.Known || !c.CreationSource.Known || !c.CreationProof.Known {
		return Classification{}, errors.New("hubclient: capable hub reply omitted classification fields")
	}
	if c.CreationSource.Valid != c.CreationProof.Valid || (!c.CreationSource.Valid && c.PriorExecutionID.Valid) {
		return Classification{}, errors.New("hubclient: inconsistent null classification provenance")
	}
	if !c.CreationSource.Valid {
		return Classification{
			Kind:     CandidateLegacy,
			HookSeen: c.HookSeen.Value,
		}, nil
	}
	switch c.CreationSource.Value {
	case "hook", "shim", "watch", "mcp", "legacy":
	default:
		return Classification{}, fmt.Errorf("hubclient: unknown creation_source %q", c.CreationSource.Value)
	}
	switch c.CreationProof.Value {
	case "initial", "absent", "stale_retry", "shim_succession":
	default:
		return Classification{}, fmt.Errorf("hubclient: unknown creation_proof %q", c.CreationProof.Value)
	}
	var priorGen int
	if !c.PriorExecutionID.Valid {
		if c.CreationProof.Value != "initial" {
			return Classification{}, errors.New("hubclient: candidate without prior_execution_id must have initial creation_proof")
		}
	} else {
		if c.PriorExecutionID.Value == "" || c.PriorExecutionID.Value == c.ExecutionID {
			return Classification{}, errors.New("hubclient: invalid prior_execution_id")
		}
		if c.Generation < 2 {
			return Classification{}, errors.New("hubclient: candidate with prior_execution_id requires generation >= 2")
		}
		if c.CreationProof.Value == "initial" {
			return Classification{}, errors.New("hubclient: candidate with prior_execution_id cannot have initial creation_proof")
		}
		priorGen = c.Generation - 1
	}
	kind := CandidateOrdinary
	if c.CreationProof.Value == "shim_succession" {
		if c.CreationSource.Value != "shim" {
			return Classification{}, errors.New("hubclient: shim_succession creation_proof requires shim creation_source")
		}
		kind = CandidateShimSuccession
	}
	return Classification{
		Kind:             kind,
		PriorExecutionID: c.PriorExecutionID.Value,
		PriorGeneration:  priorGen,
		CreationSource:   c.CreationSource.Value,
		CreationProof:    c.CreationProof.Value,
		HookSeen:         c.HookSeen.Value,
	}, nil
}

// Candidate projects a registration or succession SessionResponse into a Candidate for
// uniform classification.
func (r SessionResponse) Candidate() Candidate {
	return Candidate{
		ExecutionID:  r.ExecutionID,
		Generation:   r.Generation,
		Handle:       r.Handle,
		RepositoryID: r.RepositoryID,
		Agent:        r.Agent,
		Worktree:     r.Worktree,
		OutsideGit:   r.OutsideGit,
		Capabilities: slices.Clone(r.Capabilities),
		LeaseMS:      r.LeaseMS,
		Provenance:   r.Provenance,
	}
}

// Bind resolves a usable Execution candidate via POST /v1/bind.
func (c *Client) Bind(ctx context.Context, req BindRequest) (Candidate, error) {
	var out Candidate
	if err := c.post(ctx, "/v1/bind", req, &out); err != nil {
		return Candidate{}, err
	}
	return out, nil
}

// Heartbeat renews or resumes an Execution via POST /v1/executions/{id}/heartbeat.
func (c *Client) Heartbeat(ctx context.Context, executionID string, req HeartbeatRequest) (Candidate, error) {
	if executionID == "" {
		return Candidate{}, errors.New("hubclient: execution id is required")
	}
	var out Candidate
	if err := c.post(ctx, "/v1/executions/"+url.PathEscape(executionID)+"/heartbeat", req, &out); err != nil {
		return Candidate{}, err
	}
	return out, nil
}

// Succeed invokes the stage 1b exact-proof succession route POST /v1/sessions/succeed/exact.
func (c *Client) Succeed(ctx context.Context, req SucceedRequest) (SessionResponse, error) {
	var out SessionResponse
	if err := c.post(ctx, "/v1/sessions/succeed/exact", req, &out); err != nil {
		return SessionResponse{}, err
	}
	return out, nil
}
