package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokInstallWritesTheSkillAndLeavesTheWakeMarkerAlone(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Grok)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, err := c.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	skill := string(mustRead(t, files[2]))
	if !strings.Contains(skill, notificationPlain) || !strings.Contains(skill, "does not see the command") || !strings.Contains(skill, "ignores an updated command") {
		t.Fatalf("skill\n%s", skill)
	}
	home, err := grokClient{}.home(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, grokWakeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain install wrote the wake marker: %v", err)
	}
	enabled, err := GrokWake(l)
	if err != nil || !enabled {
		t.Fatalf("missing marker: enabled %v %v", enabled, err)
	}
	if drifted, err := l.Drift(c); err != nil || len(drifted) != 0 {
		t.Fatalf("fresh install drifted: %v %v", drifted, err)
	}
}

func TestGrokWakeMarkerIsExplicitAndSurvivesReinstall(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Grok)
	if err := InstallGrokWake(l, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := GrokWake(l)
	if err != nil || enabled {
		t.Fatalf("off marker: %v %v", enabled, err)
	}
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	enabled, err = GrokWake(l)
	if err != nil || enabled {
		t.Fatalf("reinstall cleared off: %v %v", enabled, err)
	}
	if err := InstallGrokWake(l, true); err != nil {
		t.Fatal(err)
	}
	enabled, err = GrokWake(l)
	if err != nil || !enabled {
		t.Fatalf("on marker: %v %v", enabled, err)
	}
	home, _ := grokClient{}.home(l)
	marker := filepath.Join(home, grokWakeFile)
	if got := string(mustRead(t, marker)); got != "on\n" {
		t.Fatalf("marker %q", got)
	}
	if err := os.WriteFile(marker, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, marker)); got != "keep\n" {
		t.Fatalf("removed a person's marker: %q", got)
	}
	files, _ := c.Files(l)
	if _, err := os.Stat(files[2]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact skill survived remove: %v", err)
	}
}

func TestGrokRemoveKeepsAReplacedSkillAndDeletesAnExactMarker(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Grok)
	if err := InstallGrokWake(l, false); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	if err := os.WriteFile(files[2], []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[2])); got != "# mine\n" {
		t.Fatalf("replaced skill: %q", got)
	}
	home, _ := grokClient{}.home(l)
	if _, err := os.Stat(filepath.Join(home, grokWakeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact off marker survived remove: %v", err)
	}
}

func TestGrokWakeReportsAnUnreadableHomeAndMarker(t *testing.T) {
	nowhere := homeless(t)
	if err := InstallGrokWake(nowhere, false); err == nil {
		t.Fatal("wake install without a home succeeded")
	}
	if _, err := GrokWake(nowhere); err == nil {
		t.Fatal("no home was a successful read")
	}
	l := testLayout(t)
	home, err := grokClient{}.home(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallGrokWake(l, true); err == nil {
		t.Fatal("a directory in place of config.toml was installed")
	}
	if _, err := os.Stat(filepath.Join(home, grokWakeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a broken config still wrote the marker: %v", err)
	}
	markerHome := testLayout(t)
	home, err = grokClient{}.home(markerHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, grokWakeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := GrokWake(markerHome); err == nil {
		t.Fatal("a directory was read as the marker")
	}
}
