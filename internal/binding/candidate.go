package binding

import (
	"cmp"
	"strings"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

var errInvalidCandidate = &hubclient.Error{Code: "lost", Message: "the hub returned an unusable session binding"}

// boundFromCandidate checks the advertised classification before joining the hub's
// answer with the session and rung that identified it locally.
func boundFromCandidate(candidate hubclient.Candidate, session string, rung int) (Bound, error) {
	class, err := candidate.Classify()
	if err != nil || candidate.ExecutionID == "" || candidate.Generation <= 0 {
		return Bound{}, errInvalidCandidate
	}
	if candidate.SessionID == "" {
		candidate.SessionID = session
	}
	if session != "" && candidate.SessionID != session {
		return Bound{}, errInvalidCandidate
	}
	b := Bound{
		ExecutionID:  candidate.ExecutionID,
		Generation:   candidate.Generation,
		RepositoryID: candidate.RepositoryID,
		Agent:        candidate.Agent,
		SessionID:    candidate.SessionID,
		Rung:         rung,
		Worktree:     candidate.Worktree,
		Capabilities: Capabilities(strings.Join(candidate.Capabilities, "\n")),
		Outside:      candidate.OutsideGit,
		HookSeen:     class.HookSeen,
	}
	if class.Kind != hubclient.CandidateUnclassified {
		b.Class, b.Classified = class, true
	}
	return b, nil
}

// mergeCandidate uses the binding targeted by a heartbeat as the implicit pair for
// a legacy lease-only reply. A reply that names a pair must identify the same one.
func mergeCandidate(b Bound, candidate hubclient.Candidate) (Bound, error) {
	if b.SessionID != "" && candidate.SessionID != "" && candidate.SessionID != b.SessionID {
		return Bound{}, errInvalidCandidate
	}
	if candidate.IsLeaseOnly() {
		b.Outside = false // Older replies omit the outside-git flag.
		return b, nil
	}
	if candidate.ExecutionID != b.ExecutionID || candidate.Generation != b.Generation {
		return Bound{}, errInvalidCandidate
	}
	fresh, err := boundFromCandidate(candidate, b.SessionID, b.Rung)
	if err != nil {
		return Bound{}, err
	}
	if !fresh.Classified {
		fresh.Class, fresh.Classified = b.Class, b.Classified
		fresh.RepositoryID = cmp.Or(fresh.RepositoryID, b.RepositoryID)
		fresh.Agent.ID = cmp.Or(fresh.Agent.ID, b.Agent.ID)
		fresh.Agent.Name = cmp.Or(fresh.Agent.Name, b.Agent.Name)
		fresh.Worktree = cmp.Or(fresh.Worktree, b.Worktree)
		if candidate.Capabilities == nil {
			fresh.Capabilities = b.Capabilities
		}
		if b.Classified {
			fresh.HookSeen = b.HookSeen
		} else {
			fresh.HookSeen = fresh.HookSeen || b.HookSeen
		}
	}
	return fresh, nil
}
