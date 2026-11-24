package hook

import (
	"path/filepath"
	"testing"
)

// A refused binding or renewal names the local file that vetoed it, so doctor can point the
// operator at the end marker or at the generation proof.
func TestObserveBindingRefusalNamesTheVetoingFile(t *testing.T) {
	t.Run("end marker", func(t *testing.T) {
		token := filepath.Join(t.TempDir(), "hub.token")
		s := NewSessions(token)
		s.ObserveBindingRefusal("claude", "s", true)
		marker := s.memory.name("claude", "s") + ".ended"
		assertRecallObservation(t, token, endedProof(marker).Error()) // gitleaks:allow (a test observation token and path, not a credential)
	})
	t.Run("generation proof", func(t *testing.T) {
		token := filepath.Join(t.TempDir(), "hub.token")
		s := NewSessions(token)
		s.ObserveBindingRefusal("claude", "s", false)
		path := s.memory.name("claude", "s")
		assertRecallObservation(t, token, BindingGenerationRefused+" at "+path) // gitleaks:allow (a test observation token and path, not a credential)
	})
}
