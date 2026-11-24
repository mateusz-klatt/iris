package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generationAliasFixture(t *testing.T, long bool) (realMarker, aliasMarker string) {
	t.Helper()
	root := t.TempDir()
	var realDir, aliasDir string
	if long {
		component := strings.Repeat("d", 235)
		realDir = filepath.Join(root, component, "sessions")
		aliasDir = root + `\` + component + `.\child\..\sessions`
	} else {
		realDir = filepath.Join(root, "iris", "sessions")
		aliasDir = root + `\iris.\sessions`
	}
	if err := os.MkdirAll(`\\?\`+realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realMarker = filepath.Join(realDir, "x.ended")
	if err := os.WriteFile(`\\?\`+realMarker, []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasMarker = aliasDir + `\x.ended`
	if long && len(aliasMarker) <= 260 {
		t.Fatalf("alias path has only %d characters", len(aliasMarker))
	}
	return realMarker, aliasMarker
}

func TestGenerationReadPreservesWin32Aliases(t *testing.T) {
	for _, long := range []bool{false, true} {
		name := "short trailing dot"
		if long {
			name = "long trailing dot and dot-dot"
		}
		t.Run(name, func(t *testing.T) {
			_, alias := generationAliasFixture(t, long)
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("os.Stat rejected the Win32 alias: %v", err)
			}
			raw, err := os.ReadFile(alias)
			if err != nil || string(raw) != "7\n" {
				t.Fatalf("os.ReadFile alias = %q, %v", raw, err)
			}
			f, err := OpenGenerationFile(alias)
			if err != nil {
				t.Fatalf("OpenGenerationFile alias: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if generation, err := readGeneration(alias); generation != 7 || err != nil {
				t.Fatalf("readGeneration alias = G%d, %v; want G7", generation, err)
			}
		})
	}
}

func TestOpenGenerationFileMatchesOSOpen(t *testing.T) {
	_, short := generationAliasFixture(t, false)
	_, long := generationAliasFixture(t, true)
	for _, tc := range []struct {
		name, path string
	}{
		{name: "short alias", path: short},
		{name: "short missing", path: short + ".missing"},
		{name: "long alias", path: long},
		{name: "long missing", path: long + ".missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			osFile, osErr := os.Open(tc.path)
			if osErr == nil {
				_ = osFile.Close()
			}
			nativeFile, nativeErr := OpenGenerationFile(tc.path)
			if nativeErr == nil {
				_ = nativeFile.Close()
			}
			if (osErr == nil) != (nativeErr == nil) {
				t.Fatalf("os.Open error = %v, OpenGenerationFile error = %v", osErr, nativeErr)
			}
		})
	}
}
