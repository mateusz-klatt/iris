package hook

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/proc"
	"golang.org/x/sys/windows"
)

func TestExtendedWindowsPath(t *testing.T) {
	long := `C:\` + strings.Repeat("long", 70)
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "short absolute is cleaned", path: `C:/short/marker.ended`, want: `C:\short\marker.ended`},
		{name: "long absolute", path: long, want: `\\?\` + long},
		{name: "UNC", path: `\\server\share\dir\marker.ended`, want: `\\server\share\dir\marker.ended`},
		{name: "already extended", path: `\\?\C:\already\marker.ended`, want: `\\?\C:\already\marker.ended`},
		{name: "device", path: `\\.\C:\device\marker.ended`, want: `\\.\C:\device\marker.ended`},
		{name: "device with alternate separators", path: `//./C:/dir/x.ended`, want: `\\.\C:\dir\x.ended`},
		{name: "extended with alternate separators", path: `//?/C:/dir/x.ended`, want: `\\?\C:\dir\x.ended`},
		{name: "device with mixed separators", path: `\\.\C:/dir/x.ended`, want: `\\.\C:\dir\x.ended`},
		{name: "UNC with alternate separators", path: `//server/share/dir/x.ended`, want: `\\server\share\dir\x.ended`},
		{name: "NT extended", path: `\??\C:\already\marker.ended`, want: `\??\C:\already\marker.ended`},
		{name: "embedded NUL is preserved", path: "bad\x00path", want: "bad\x00path"},
		{name: "long embedded NUL is preserved", path: long + "\x00", want: long + "\x00"},
		{name: "relative", path: `relative\marker.ended`, want: `relative\marker.ended`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extendedWindowsPath(tc.path); got != tc.want {
				t.Fatalf("extendedWindowsPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestExtendedWindowsPathLongUNCAndBufferGrowth(t *testing.T) {
	input := `\\server\share\` + strings.Repeat("u", 248)
	encoded, err := windows.UTF16FromString(input)
	if err != nil {
		t.Fatal(err)
	}
	original := windowsFullPathName
	t.Cleanup(func() { windowsFullPathName = original })
	calls := 0
	windowsFullPathName = func(_ *uint16, n uint32, buf *uint16, _ **uint16) (uint32, error) {
		calls++
		if calls == 1 {
			return n + 100, nil
		}
		copy(unsafe.Slice(buf, len(encoded)), encoded)
		return uint32(len(encoded) - 1), nil
	}
	want := `\\?\UNC\server\share\` + strings.Repeat("u", 248)
	if got := extendedWindowsPath(input); got != want || calls != 2 {
		t.Fatalf("long UNC conversion = %q after %d calls, want %q after 2", got, calls, want)
	}
}

func TestExtendedWindowsPathPreservesInputWhenResolutionFails(t *testing.T) {
	original := windowsFullPathName
	t.Cleanup(func() { windowsFullPathName = original })
	windowsFullPathName = func(*uint16, uint32, *uint16, **uint16) (uint32, error) {
		return 0, errors.New("resolution failed")
	}
	input := strings.Repeat("r", 248) + "/marker.ended"
	if got := extendedWindowsPath(input); got != input {
		t.Fatalf("extendedWindowsPath(%q) = %q, want original input", input, got)
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
