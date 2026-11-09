package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Windows honours the read-only file attribute even though directory mode bits
// cannot reproduce the permission failures covered by the Unix tests.
func TestAgyWriteReportsReadOnlySettingsWithoutChangingThem(t *testing.T) {
	l, path, repo := agyLayout(t)
	const before = "{}\n"
	seedSettings(t, path, before)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	if note := agyWrite(l, repo)(); !strings.Contains(note, "could not allow") || !strings.Contains(note, path) {
		t.Fatal(note)
	}
	if got := read(t, path); got != before {
		t.Fatalf("settings changed after a failed write: %q", got)
	}
}

func TestCodexTrustReportsReadOnlyRestoreWithoutLosingTheTrustEntry(t *testing.T) {
	l, path := codexLayout(t)
	repo := filepath.Join(t.TempDir(), "repo")
	writeConfig(t, path, userCodexConfig)
	undo := codexTrust(l, repo)
	during := userCodexConfig + "\n" + trustTable(repo)
	writeConfig(t, path, during)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	if note := undo(); !strings.Contains(note, "could not restore") || !strings.Contains(note, path) || !strings.Contains(note, repo) {
		t.Fatal(note)
	}
	if got := read(t, path); got != during {
		t.Fatalf("settings changed after a failed restore: %q", got)
	}
}
