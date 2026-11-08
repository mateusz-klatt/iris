package store

import (
	"context"
	"fmt"
	"time"
)

type principalKey struct{}

// WithPrincipal carries the credential the hub authenticated, for per-credential
// quotas. Only the hub sets it; nothing a client sends can.
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func principalOf(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

// crossLimit bounds cross-repository recipients per credential and minute (ADR 004).
// Mail wakes its recipient, and the general limit of 6000 requests a minute with up
// to 100 recipients each would let one credential flood every inbox on the hub.
const crossLimit = 120

type crossWindow struct {
	start time.Time
	count int
}

// allowCross records n cross-repository recipients for principal, or refuses the
// whole send when they would pass the limit. A refused send records nothing.
func (s *Store) allowCross(principal string, n int, now time.Time) bool {
	if s.cross == nil {
		s.cross = map[string]crossWindow{}
	}
	w := s.cross[principal]
	if !now.Before(w.start.Add(time.Minute)) {
		w = crossWindow{start: now}
	}
	if w.count+n > crossLimit {
		return false
	}
	w.count += n
	s.cross[principal] = w
	return true
}

func crossRefused() error {
	return fail("rate_limited", fmt.Sprintf("sends to other repositories are limited to %d recipients a minute per credential; retry later", crossLimit))
}
