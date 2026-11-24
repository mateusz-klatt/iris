package doctor

import (
	"errors"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hook"
)

func TestReadEndMarkerGenerationRejectsInvalidWindowsPath(t *testing.T) {
	if generation, err := readEndMarkerGeneration("bad\x00path.ended"); generation != 0 || !errors.Is(err, hook.ErrInvalidGeneration) {
		t.Fatalf("invalid Windows path = G%d, %v", generation, err)
	}
}
