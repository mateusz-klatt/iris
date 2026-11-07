package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

const userCodexConfig = `# my settings
model = "gpt-6.1-sol"

[projects."/home/me/work"]
trust_level = "trusted"

# keep this comment
[projects."/home/me/other"]
trust_level = "trusted"

[mcp_servers.other]
command = "other"
`

func codexLayout(t *testing.T) (integrate.Layout, string) {
	t.Helper()
	home := t.TempDir()
	return integrate.Layout{Home: home, Getenv: func(string) string { return "" }}, filepath.Join(home, ".codex", "config.toml")
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func trustTable(dir string) string {
	return "[projects." + integrate.TOMLString(dir) + "]\ntrust_level = \"trusted\"\n"
}

func TestCodexTrustEntryIsRemovedAndTheFileIsIdentical(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	for name, codexWrites := range map[string]string{
		// Appended after everything, as on a file without project tables.
		"appended": userCodexConfig + "\n" + trustTable(repo),
		// Inserted after the other project tables, before the next table.
		"among projects": strings.Replace(userCodexConfig, "[mcp_servers.other]", trustTable(repo)+"\n[mcp_servers.other]", 1),
		// Literal-string spelling of the same key.
		"literal": userCodexConfig + "\n[projects.'" + repo + "']\ntrust_level = \"trusted\"\n",
		// Nothing written at all.
		"unchanged": userCodexConfig,
	} {
		t.Run(name, func(t *testing.T) {
			layout, path := codexLayout(t)
			writeConfig(t, path, userCodexConfig)
			undo := codexTrust(layout, repo)
			writeConfig(t, path, codexWrites)
			if note := undo(); note != "" {
				t.Fatal(note)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != userCodexConfig {
				t.Fatalf("err %v\n%s", err, got)
			}
		})
	}
}

func TestCodexTrustRemovesAFileCodexCreated(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	layout, path := codexLayout(t)
	undo := codexTrust(layout, repo)
	writeConfig(t, path, trustTable(repo))
	if note := undo(); note != "" {
		t.Fatal(note)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config.toml left behind: %v", err)
	}
	// No file before and none after is no change at all.
	if note := codexTrust(layout, repo)(); note != "" {
		t.Fatal(note)
	}
}

func TestCodexTrustLeavesAFileThatChangedOtherwise(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	layout, path := codexLayout(t)
	writeConfig(t, path, userCodexConfig)
	undo := codexTrust(layout, repo)
	changed := strings.Replace(userCodexConfig, "gpt-6.1-sol", "gpt-6-luna", 1) + "\n" + trustTable(repo)
	writeConfig(t, path, changed)
	if note := undo(); !strings.Contains(note, "changed during the run in other ways") || !strings.Contains(note, repo) {
		t.Fatal(note)
	}
	if got, _ := os.ReadFile(path); string(got) != changed {
		t.Fatal("a file that changed otherwise was rewritten")
	}
	// A file that vanished during the run is not recreated.
	undo = codexTrust(layout, repo)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if note := undo(); !strings.Contains(note, "changed during the run in other ways") {
		t.Fatal(note)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a removed file was recreated")
	}
}

func TestCodexTrustSaysWhatItCouldNotDo(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	layout, path := codexLayout(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if note := codexTrust(layout, repo)(); !strings.Contains(note, "could not read") {
		t.Fatal(note)
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("home", "")
	if note := codexTrust(integrate.Layout{Getenv: func(string) string { return "" }}, repo)(); note != "" {
		t.Fatal(note)
	}
}

func TestCodexTrustReportsARestoreThatFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	layout, path := codexLayout(t)
	undo := codexTrust(layout, repo)
	writeConfig(t, path, trustTable(repo))
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Dir(path), 0o700) }()
	if note := undo(); !strings.Contains(note, "could not restore") {
		t.Fatal(note)
	}
}

func TestProjectHeadersFollowSymbolicLinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	headers := strings.Join(projectHeaders(link), "\n")
	if !strings.Contains(headers, "[projects."+integrate.TOMLString(link)+"]") || !strings.Contains(headers, "[projects."+integrate.TOMLString(resolved)+"]") {
		t.Fatal(headers)
	}
}

func TestWithoutProjectKeepsAFileWithoutTheTable(t *testing.T) {
	raw := []byte(userCodexConfig)
	if got := withoutProject(raw, []string{`[projects."/nowhere"]`}); string(got) != userCodexConfig {
		t.Fatal(string(got))
	}
	// The table as the first and only thing in the file.
	if got := withoutProject([]byte(trustTable("/x")), projectHeaders("/x")); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
