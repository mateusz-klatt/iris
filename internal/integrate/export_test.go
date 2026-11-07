package integrate

import (
	"path/filepath"
	"testing"
)

func TestCodexConfigIsTheFileCodexTrustsDirectoriesIn(t *testing.T) {
	home := t.TempDir()
	got, err := CodexConfig(Layout{Home: home, Getenv: func(string) string { return "" }})
	if err != nil || got != filepath.Join(home, ".codex", "config.toml") {
		t.Fatalf("%q %v", got, err)
	}
	custom := t.TempDir()
	got, err = CodexConfig(Layout{Getenv: func(key string) string {
		if key == "CODEX_HOME" {
			return custom
		}
		return ""
	}})
	if err != nil || got != filepath.Join(custom, "config.toml") {
		t.Fatalf("%q %v", got, err)
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err = CodexConfig(Layout{Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("no home, yet a path")
	}
}

func TestTOMLStringEscapesAWindowsPath(t *testing.T) {
	if got := TOMLString(`C:\Users\me "x"`); got != `"C:\\Users\\me \"x\""` {
		t.Fatal(got)
	}
}
