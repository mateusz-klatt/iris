package binding

import (
	"os"
	"path/filepath"
	"testing"

	irisconfig "github.com/mateusz-klatt/iris/internal/config"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iris-binding-test")
	if err != nil {
		panic(err)
	}
	restore := irisconfig.SetPinnedForTest(filepath.Join(dir, "pinned-iris"))
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
