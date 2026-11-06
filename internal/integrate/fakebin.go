package integrate

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// PlaceFake copies a small Go stub into dir under name. On Windows the name
// gains a .exe suffix when it has none. The copy is built once per process.
// Behaviour is IRIS_FAKE_STDOUT, IRIS_FAKE_EXIT and IRIS_FAKE_REQUIRE_CLOSED_STDIN.
func PlaceFake(dir, name string) (string, error) {
	exe, err := fakeBinary()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, exeName(runtime.GOOS, name))
	if err := copyFile(exe, dest); err != nil {
		return "", err
	}
	return dest, nil
}

var (
	fakeOnce sync.Once
	fakeExe  string
	fakeErr  error
)

func fakeBinary() (string, error) {
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "iris-fakebin-")
		if err != nil {
			fakeErr = err
			return
		}
		out := filepath.Join(dir, exeName(runtime.GOOS, "fakebin"))
		cmd := exec.Command("go", "build", "-o", out, "github.com/mateusz-klatt/iris/internal/integrate/internal/fakebin")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		msg, err := cmd.CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("build fakebin: %w: %s", err, msg)
			return
		}
		fakeExe = out
	})
	return fakeExe, fakeErr
}

func exeName(goos, name string) string {
	if goos == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}
	return name
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
