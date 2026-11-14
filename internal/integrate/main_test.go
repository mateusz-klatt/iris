package integrate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
)

func TestMain(m *testing.M) {
	// Started again by a test, this binary stands in for a live Kimi watcher until it is stopped.
	if os.Getenv(kimiWatcherEnv) == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "iris-test-home")
	if err != nil {
		panic(err)
	}
	restore := config.SetPinnedForTest(filepath.Join(dir, "pinned-iris"))
	code := m.Run()
	restore()
	CleanupFake()
	CleanupTestDir(dir, "iris-test-home")
	os.Exit(code)
}
