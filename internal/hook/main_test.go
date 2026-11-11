package hook

import (
	"io"
	"os"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// TestMain points every user directory at a temporary one. These tests inject hook
// failures on purpose, and those must never reach the real hook-failures.log, where
// iris doctor would report them as genuine.
func TestMain(m *testing.M) {
	// A live process with Codex's argv lets the Linux identity regression use the
	// same /proc evidence as a real exec or app-server, without starting a model.
	if os.Getenv("IRIS_TEST_CODEX_PROCESS") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "iris-test-home")
	if err != nil {
		panic(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "AppData", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if err = os.Setenv(key, dir); err != nil {
			panic(err)
		}
	}
	// An exported credential must not point hook failures at the real home.
	for _, key := range []string{"IRIS_HUB_URL", "IRIS_TOKEN_FILE"} {
		if err = os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	integrate.CleanupTestDir(dir, "iris-test-home")
	os.Exit(code)
}
