package gitinfo

import (
	"errors"
	"testing"
)

func TestFindRefusesAPathWithANUL(t *testing.T) {
	if _, err := Find("invalid\x00path"); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("invalid Windows path: %v", err)
	}
}
