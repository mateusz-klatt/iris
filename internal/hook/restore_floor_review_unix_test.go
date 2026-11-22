//go:build !windows

package hook

import (
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

func TestReducedShellWithMalformedProofUsesNoRestoredAuthority(t *testing.T) {
	checkDamagedReviewProof(t, "/v1/shell/scoped", false, false, func(h *restoredHook) {
		repo, err := gitinfo.Find(h.root)
		if err != nil {
			t.Fatal(err)
		}
		m := sessionsFor(h.getenv)
		m.SetCapable(h.server.URL, true)
		m.SetBreaker(worktreeKey(repo), breaker{V: 1, Scope: scopeReduced, NextFullMS: time.Now().Add(time.Minute).UnixMilli()})
	})
}
