package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// allowed is permissions.allow of a settings file, decoded, so a Windows path compares as written.
func allowed(t *testing.T, body string) []string {
	t.Helper()
	var doc struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return doc.Permissions.Allow
}

func agyLayout(t *testing.T) (integrate.Layout, string, string) {
	t.Helper()
	home := t.TempDir()
	l := integrate.Layout{Home: home, Getenv: func(string) string { return "" }}
	path, err := integrate.AgySettings(l)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	return l, path, repo
}

func seedSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The run allows only the scratch file, and the user's settings come back byte for byte.
func TestAgyWriteAllowsOnlyTheScratchFileForTheRun(t *testing.T) {
	l, path, repo := agyLayout(t)
	mine := "{\n    \"trustedWorkspaces\": [\"/work\"],\n    \"permissions\": {\"allow\": [\"mcp(iris/*)\"]}\n}\n"
	seedSettings(t, path, mine)
	undo := agyWrite(l, repo)
	during := read(t, path)
	rule := agyWriteRule(repo)
	allow := allowed(t, during)
	if !slices.Contains(allow, rule) || !slices.Contains(allow, "mcp(iris/*)") || !strings.HasSuffix(rule, filepath.Join("repo", scratchFile)+")") {
		t.Fatalf("during the run\n%s", during)
	}
	if note := undo(); note != "" {
		t.Fatal(note)
	}
	if got := read(t, path); got != mine {
		t.Fatalf("not restored\n got %q\nwant %q", got, mine)
	}
}

func TestAgyWriteCreatesAndRemovesTheSettings(t *testing.T) {
	l, path, repo := agyLayout(t)
	undo := agyWrite(l, repo)
	if !slices.Contains(allowed(t, read(t, path)), agyWriteRule(repo)) {
		t.Fatal("rule not written")
	}
	if note := undo(); note != "" {
		t.Fatal(note)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created settings left behind: %v", err)
	}
}

func TestAgyWriteKeepsARuleTheUserAlreadyHas(t *testing.T) {
	l, path, repo := agyLayout(t)
	raw, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": []string{agyWriteRule(repo)}}})
	mine := string(raw) // the rule holds backslashes on Windows, so it is encoded, not pasted
	seedSettings(t, path, mine)
	undo := agyWrite(l, repo)
	if read(t, path) != mine {
		t.Fatal("touched during the run")
	}
	if note := undo(); note != "" || read(t, path) != mine {
		t.Fatalf("%q %q", note, read(t, path))
	}
}

// Anything else that changed meanwhile stays, and the report names the entry to remove.
func TestAgyWriteLeavesAFileChangedDuringTheRun(t *testing.T) {
	l, path, repo := agyLayout(t)
	seedSettings(t, path, "{}\n")
	undo := agyWrite(l, repo)
	changed := strings.Replace(read(t, path), "{", `{"theirs": true,`, 1)
	seedSettings(t, path, changed)
	note := undo()
	if !strings.Contains(note, "left alone") || !strings.Contains(note, agyWriteRule(repo)) || read(t, path) != changed {
		t.Fatalf("%q\n%s", note, read(t, path))
	}
}

func TestAgyWriteRefusesFilesItCannotEditSafely(t *testing.T) {
	cases := map[string]string{"not valid JSON": "{", "cannot read": `{"permissions": "x"}`}
	for want, body := range cases {
		l, path, repo := agyLayout(t)
		seedSettings(t, path, body)
		if note := agyWrite(l, repo)(); !strings.Contains(note, want) || read(t, path) != body {
			t.Fatalf("%s: %q", want, note)
		}
	}
	l, path, repo := agyLayout(t)
	if err := os.MkdirAll(path, 0o700); err != nil { // a directory where the file should be
		t.Fatal(err)
	}
	if note := agyWrite(l, repo)(); !strings.Contains(note, "could not read") {
		t.Fatal(note)
	}
}

func TestAgyWriteReportsWhatItCouldNotDo(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory permission bits do not refuse this account")
	}
	// The settings directory refuses the new file.
	l, path, repo := agyLayout(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
	if note := agyWrite(l, repo)(); !strings.Contains(note, "could not allow") {
		t.Fatal(note)
	}
	// The created file cannot be removed afterwards.
	l, path, repo = agyLayout(t)
	undo := agyWrite(l, repo)
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
	if note := undo(); !strings.Contains(note, "by hand") {
		t.Fatal(note)
	}
}

func TestAgyWriteRuleUsesThePathAsGivenWhenItCannotResolve(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if got := agyWriteRule(missing); got != "write_file("+filepath.Join(missing, scratchFile)+")" {
		t.Fatal(got)
	}
}

// A settings directory that cannot be created is reported, and nothing else is written.
func TestAgyWriteCannotCreateTheSettingsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory permission bits do not refuse this account")
	}
	l, path, repo := agyLayout(t)
	gemini := filepath.Dir(filepath.Dir(path))
	if err := os.Mkdir(gemini, 0o500); err != nil { // ~/.gemini exists, antigravity-cli cannot be made
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gemini, 0o700) })
	if note := agyWrite(l, repo)(); !strings.Contains(note, "could not allow") {
		t.Fatal(note)
	}
}

// A file obstructing the parent directory is refused on every platform, even
// when the test account can bypass directory permission bits.
func TestWriteSettingsPreservesAFileObstructingTheDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	const before = "existing file\n"
	if err := os.WriteFile(parent, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSettings(filepath.Join(parent, "settings.json"), []byte("{}\n")); err == nil {
		t.Fatal("wrote settings through a file")
	}
	if got := read(t, parent); got != before {
		t.Fatalf("obstructing file changed: %q", got)
	}
}

// Without a home directory there is no settings file, and nothing to allow or undo.
func TestAgyWriteWithoutAHomeDoesNothing(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if note := agyWrite(integrate.Layout{Getenv: func(string) string { return "" }}, t.TempDir())(); note != "" {
		t.Fatal(note)
	}
}
