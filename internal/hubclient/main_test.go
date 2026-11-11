package hubclient

import (
	"os"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// TestMain points every user directory at a temporary one. These tests inject hook
// failures on purpose, and those must never reach the real hook-failures.log, where
// iris doctor would report them as genuine.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iris-test-home")
	if err != nil {
		panic(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "AppData", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if err = os.Setenv(key, dir); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	integrate.CleanupTestDir(dir, "iris-test-home")
	os.Exit(code)
}
