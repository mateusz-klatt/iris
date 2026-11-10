package gitinfo

import "testing"

// Windows stops neither git's descendants at the deadline nor reads the index's identity, so a
// reduced snapshot is never taken there (ADR 018).
func TestWindowsTakesNoReducedSnapshot(t *testing.T) {
	if ReducedSupported {
		t.Fatal("reduced scope is on")
	}
	if _, err := ReadIndexID("index"); err == nil {
		t.Fatal("the index was identified")
	}
}
