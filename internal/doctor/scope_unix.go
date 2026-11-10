//go:build !windows

package doctor

import (
	"fmt"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

// scopeReport is the shell scope of every worktree whose breaker tripped, read from the session
// memory beside the token file (ADR 018 decision 6).
func scopeReport(tokenFile string, now time.Time) []string {
	return scopeLines(hook.ScopeHints(tokenFile, now))
}

// scopeLines says, for each worktree, which scope its next shell reconcile runs and why, and
// when a full status last finished there after a trip; then what reduced scope and the hint
// cannot do. A reduced worktree is never shown as full: one whose cooldown is over says that its
// next command tries full scope again.
func scopeLines(hints []hook.ScopeHint) []string {
	if len(hints) == 0 {
		return nil
	}
	lines := make([]string, 0, len(hints)+1)
	for _, h := range hints {
		var line string
		switch {
		case h.Due:
			line = fmt.Sprintf("%s: reduced until %s, because %s", h.Worktree, h.NextFull.Format(time.RFC3339), h.Reason)
		case h.Reduced:
			line = fmt.Sprintf("%s: reduced, and its next shell command tries full scope again; it tripped because %s", h.Worktree, h.Reason)
		default:
			line = h.Worktree + ": full"
		}
		last := "none since it tripped"
		if !h.LastFullOK.IsZero() {
			last = h.LastFullOK.Format(time.RFC3339)
		}
		lines = append(lines, line+"; a full status last finished: "+last)
	}
	return append(lines, scopeLimits)
}

// scopeLimits is what reduced scope and its hint cannot do.
const scopeLimits = "limits: reduced scope does not see tracked edits inside submodules, so a shell command's changes there are neither warned about nor claimed. " +
	"Each hook reads this hint when it starts, so a change to it does not alter a hook already running"
