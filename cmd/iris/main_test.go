package main

import (
	"os"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// TestMain points every user directory at a temporary one. These tests inject hook
// failures on purpose, and those must never reach the real hook-failures.log, where
// iris doctor would report them as genuine.
func TestMain(m *testing.M) {
	// An exported credential must not point hook failures at the real home.
	for _, key := range []string{"IRIS_HUB_URL", "IRIS_TOKEN_FILE"} {
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	// These children inherit the parent's temporary home and can exit or be
	// killed inside a test, before m.Run returns. Only the parent owns cleanup.
	if os.Getenv("IRIS_TEST_MCP_CHILD") == "1" || os.Getenv("IRIS_TEST_HOOK_CHILD") == "1" {
		os.Exit(m.Run())
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
	code := m.Run()
	integrate.CleanupFake()
	integrate.CleanupTestDir(dir, "iris-test-home")
	os.Exit(code)
}
