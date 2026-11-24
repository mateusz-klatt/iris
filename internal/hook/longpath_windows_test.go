package hook

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestExtendedWindowsPath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	long := `C:\` + strings.Repeat("long", 70)
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "short absolute is cleaned", path: `C:/short/marker.ended`, want: `\\?\C:\short\marker.ended`},
		{name: "long absolute", path: long, want: `\\?\` + long},
		{name: "UNC", path: `\\server\share\dir\marker.ended`, want: `\\?\UNC\server\share\dir\marker.ended`},
		{name: "already extended", path: `\\?\C:\already\marker.ended`, want: `\\?\C:\already\marker.ended`},
		{name: "device", path: `\\.\C:\device\marker.ended`, want: `\\.\C:\device\marker.ended`},
		{name: "NT extended", path: `\??\C:\already\marker.ended`, want: `\??\C:\already\marker.ended`},
		{name: "embedded NUL is preserved", path: "bad\x00path", want: "bad\x00path"},
		{name: "relative", path: `relative\marker.ended`, want: `\\?\` + filepath.Join(cwd, `relative\marker.ended`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extendedWindowsPath(tc.path); got != tc.want {
				t.Fatalf("extendedWindowsPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestSessionEndPreservesLongMarkerPath(t *testing.T) {
	root := t.TempDir()
	m := newFileMemory(t)
	m.dir = filepath.Join(root, strings.Repeat("d", 235), "sessions")
	if err := os.MkdirAll(`\\?\`+m.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m.restrict = func(string) error { return nil }
	m.Remember("claude", "s", 2)
	marker := m.name("claude", "s") + ".ended"
	if len(marker) <= 260 {
		t.Fatalf("test marker path has only %d characters", len(marker))
	}
	if err := m.replace(marker, "1"); err != nil {
		t.Fatal(err)
	}
	held, err := OpenGenerationFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if generation, err := readGeneration(marker); generation != 1 || err != nil {
		t.Fatalf("long marker read = G%d, %v; want G1", generation, err)
	}

	token := filepath.Join(root, "hub.token")
	if err := os.WriteFile(token, []byte("test-only-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	hub := &sessionEndReplaceHub{}
	payload := `{"session_id":"s","cwd":` + quote(root) + `}`
	out := (Runner{
		ClientName: "claude", EventName: "SessionEnd", Stdin: strings.NewReader(payload), Getenv: getenv,
		Find: func(string) (gitinfo.Repo, error) { return gitinfo.Repo{Worktree: root}, nil },
		Host: func() string { return "host" }, Ancestor: func() (proc.Ancestor, error) { return proc.Ancestor{}, nil },
		Hub: hub, Memory: m, after: never, logAfter: never,
	}).Run()
	assertAllow(t, ClientClaude, out)
	if generation, err := readGeneration(marker); generation != 2 || err != nil {
		t.Fatalf("long-path replacement = G%d, %v; want G2", generation, err)
	}
	old, err := io.ReadAll(held)
	if err != nil || string(old) != "1\n" {
		t.Fatalf("open long-path reader lost the old marker: %q, %v", old, err)
	}
	observed, err := os.ReadFile(filepath.Join(root, "hook-observed.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(observed), DamagedGenerationProof) {
		t.Fatalf("long-path SessionEnd recorded damaged proof: %s", observed)
	}
}
