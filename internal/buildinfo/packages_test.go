package buildinfo

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoDiscoveryExcludesNodeDependenciesAndIncludesNewPackages(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":            "module example.invalid/discovery\n\ngo 1.27.3\n",
		"cmd/first/main.go": "package main\nfunc main() {}\n",
		"newpackage/new.go": "package newpackage\n",
		"web/node_modules/dependency/pkg/dependency.go": "package dependency\n",
	}
	// Use the repository's actual boundary. Without it, the dependency package
	// below is discovered too, reproducing an npm install changing the Go targets.
	boundary, err := os.ReadFile(filepath.Join("..", "..", "web", "go.mod"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err == nil {
		files["web/go.mod"] = string(boundary)
	}
	for path, body := range files {
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, output)
	}
	want := "example.invalid/discovery/cmd/first\nexample.invalid/discovery/newpackage\n"
	if string(output) != want {
		t.Fatalf("Go packages:\n%s\nwant only first-party packages:\n%s", output, want)
	}
}

func TestWebModuleCannotHideFirstPartyGo(t *testing.T) {
	root := filepath.Join("..", "..", "web")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if strings.HasSuffix(entry.Name(), ".go") {
			t.Errorf("first-party Go file %s is behind the web module boundary; keep Go source outside web/", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
