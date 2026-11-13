package integrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
)

func TestMain(m *testing.M) {
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
