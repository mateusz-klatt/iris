package binding

import (
	"os"
	"path/filepath"
	"testing"

	irisconfig "github.com/mateusz-klatt/iris/internal/config"
)

// testDirKey names the run's temporary directory for the helper processes that
// TestRealOrphanExits starts from this binary.
const testDirKey = "IRIS_BINDING_TEST_DIR"

// TestMain points the pinned binary record at a temporary directory. The helpers
// that TestRealOrphanExits starts exit inside that test, before m.Run returns, so
// each of them would leave a directory of its own behind. They inherit the
// directory of the run that started them instead, and only that run removes it.
func TestMain(m *testing.M) {
	if os.Getenv("IRIS_ORPHAN_TEST") != "" {
		restore := irisconfig.SetPinnedForTest(filepath.Join(os.Getenv(testDirKey), "pinned-iris"))
		code := m.Run()
		restore()
		os.Exit(code)
	}
	dir, err := os.MkdirTemp("", "iris-binding-test")
	if err != nil {
		panic(err)
	}
	if err = os.Setenv(testDirKey, dir); err != nil {
		panic(err)
	}
	restore := irisconfig.SetPinnedForTest(filepath.Join(dir, "pinned-iris"))
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
