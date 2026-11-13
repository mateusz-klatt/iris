package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iris-watch-test")
	if err != nil {
		panic(err)
	}
	restore := config.SetPinnedForTest(filepath.Join(dir, "pinned-iris"))
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
