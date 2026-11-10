//go:build !windows

package doctor

import (
	"fmt"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

// scopeReport is the shell scope each worktree's breaker suggests, read from the session memory
// beside the token file (ADR 018 decision 6).
func scopeReport(tokenFile string, now time.Time) []string {
	return scopeLines(hook.ScopeHints(tokenFile, now))
}

// scopeLines says, for each worktree with a breaker, which scope it suggests and why, and the
// last full status recorded as succeeding there; then what reduced scope and the hint cannot do.
// A reduced worktree is never shown as full: one whose cooldown is over says that its next
// command tries full scope again.
func scopeLines(hints []hook.ScopeHint) []string {
	if len(hints) == 0 {
		return nil
	}
	lines := make([]string, 0, len(hints)+1)
	for _, h := range hints {
		var line string
		switch {
		case h.Due:
			line = fmt.Sprintf("%s: the breaker suggests reduced scope until %s, because %s", h.Worktree, h.NextFull.Format(time.RFC3339), h.Reason)
		case h.Reduced:
			line = fmt.Sprintf("%s: the breaker tripped because %s, and the next shell command tries full scope again", h.Worktree, h.Reason)
		default:
			line = h.Worktree + ": the breaker suggests full scope"
		}
		last := "none"
		if !h.LastFullOK.IsZero() {
			last = h.LastFullOK.Format(time.RFC3339)
		}
		lines = append(lines, line+"; last recorded full status that succeeded: "+last)
	}
	return append(lines, scopeLimits)
}

// scopeLimits is what reduced scope and its hint cannot do.
const scopeLimits = "limits: reduced scope does not see tracked edits inside submodules, so a shell command's changes there are neither warned about nor claimed. " +
	"A hook still runs full scope where its hub does not take reduced snapshots, its environment points git at another repository or index, or the index is missing. " +
	"Once a hook has chosen its scope, a later change to the hint does not alter that choice"
