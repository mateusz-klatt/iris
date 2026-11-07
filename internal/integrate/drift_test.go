package integrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Before integrate, every client's configuration differs from what it installs. After it,
// nothing does, and Drift itself writes nothing: no file, no manifest, no plugin command.
func TestDriftIsEmptyOnlyAfterIntegrateAndWritesNothing(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			before, err := l.Drift(c)
			if err != nil || len(before) == 0 {
				t.Fatalf("before integrate: %v %v", before, err)
			}
			for _, path := range before {
				if _, err := os.Stat(path); err == nil {
					t.Fatalf("Drift wrote %s", path)
				}
			}
			if _, err := os.Stat(manifestPath(l.StateDir)); err == nil {
				t.Fatal("Drift wrote the manifest")
			}
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			if after, err := l.Drift(c); err != nil || len(after) != 0 {
				t.Fatalf("after integrate: %v %v", after, err)
			}
		})
	}
}

// A hook event removed by hand, which a partial integration looks like, is drift in the file
// that lost it, and only there.
func TestARemovedHookIsDriftInItsFile(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["hooks"].(map[string]any), "preToolUse")
	edited, _ := json.Marshal(doc)
	if err := os.WriteFile(files[0], edited, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := l.Drift(c)
	if err != nil || len(got) != 1 || got[0] != files[0] {
		t.Fatalf("drift %v %v, want only %s", got, err, files[0])
	}
}

// A client that rewrites its own JSON with other spacing and key order has not drifted.
func TestReformattedJSONIsNotDrift(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		compact, _ := json.Marshal(doc)
		if err := os.WriteFile(path, compact, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := l.Drift(c); err != nil || len(got) != 0 {
		t.Fatalf("reformatting counted as drift: %v %v", got, err)
	}
}

// Hooks that call an iris binary which has since moved run nothing: that is drift too.
func TestAMovedIrisBinaryIsDrift(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Codex)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(l.Home, "elsewhere", "iris")
	l.LookPath = func(name string) (string, error) { return moved, nil }
	got, err := l.Drift(c)
	if err != nil || len(got) != 2 {
		t.Fatalf("drift %v %v, want both codex files", got, err)
	}
	if !strings.HasSuffix(got[0], "hooks.json") || !strings.HasSuffix(got[1], "config.toml") {
		t.Fatal(got)
	}
}

// A broken file is not drift: Drift reports the error, which doctor shows as it is.
func TestDriftReportsABrokenFile(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	files, _ := c.Files(l)
	if err := os.MkdirAll(filepath.Dir(files[0]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Drift(c); err == nil {
		t.Fatalf("a broken file gave drift %v and no error", got)
	}
}

// The user's own handlers beside Iris's, which reintegration keeps (#108), are not drift.
func TestTheUsersOwnHandlersAreNotDrift(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	raw, _ := os.ReadFile(files[0])
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	hooks := doc["hooks"].(map[string]any)
	hooks["preToolUse"] = append(hooks["preToolUse"].([]any), map[string]any{"command": "my-own-linter"})
	hooks["stop"] = []any{map[string]any{"command": "my-own-notifier"}}
	edited, _ := json.Marshal(doc)
	if err := os.WriteFile(files[0], edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Drift(c); err != nil || len(got) != 0 {
		t.Fatalf("the user's handlers counted as drift: %v %v", got, err)
	}
}

// Drift never writes, which a read-only configuration proves: a write there would fail, and
// Drift still answers.
func TestDriftNeedsNoWriteAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			// A moved binary makes every file differ, so Drift renders each one.
			l.LookPath = func(string) (string, error) { return filepath.Join(l.Home, "moved", "iris"), nil }
			var dirs []string
			_ = filepath.WalkDir(l.Home, func(path string, d os.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					dirs = append(dirs, path)
				}
				return nil
			})
			for _, dir := range dirs {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, dir := range dirs {
					_ = os.Chmod(dir, 0o700)
				}
			})
			if got, err := l.Drift(c); err != nil || len(got) == 0 {
				t.Fatalf("read-only drift: %v %v", got, err)
			}
		})
	}
}
