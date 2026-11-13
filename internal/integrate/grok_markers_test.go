package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// grokConfig writes content as the layout's Grok config.toml, with IRIS_COMMAND replaced by the
// layout's Iris path as a TOML string, as Grok writes it (escaped backslashes on Windows), and returns the Grok client, its config path and what was written.
func grokConfig(t *testing.T, l Layout, content string) (Client, string, string) {
	t.Helper()
	c, _ := ByName(Grok)
	files, err := c.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	content = strings.ReplaceAll(content, "IRIS_COMMAND", tomlString(l.Iris()))
	if err := os.MkdirAll(filepath.Dir(files[1]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[1], []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return c, files[1], content
}

func decodeTOML(t *testing.T, raw string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := toml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return doc
}

// Grok saves its config.toml when a setting changes and drops the comments, Iris's markers among
// them, and reflows arrays (the fixture is such a save from Grok 1.0.77, paths anonymized). Its
// own [mcp_servers.iris] is still Iris's: doctor sees no drift, and integrate puts the markers
// back around that table in place, keeping every other line, permission_mode included.
func TestGrokAdoptsItsOwnTableAfterGrokRewroteTheConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "grok", "config-rewritten-1.0.77.toml"))
	if err != nil {
		t.Fatal(err)
	}
	l := testLayout(t)
	c, path, rewritten := grokConfig(t, l, string(raw))
	// The hooks and skill are installed as usual, so that only config.toml is in question.
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	if drifted, err := l.Drift(c); err != nil || len(drifted) != 0 {
		t.Fatalf("Grok's own save read as drift: %v %v", drifted, err)
	}
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	head := rewritten[:strings.Index(rewritten, "[mcp_servers.iris]")]
	if !strings.HasPrefix(got, head) || !strings.HasSuffix(got, wrapBlock("[mcp_servers.iris]\ncommand = "+tomlString(l.Iris())+"\nargs = [\"mcp\", \"--client\", \"grok\"]\nenabled = true\n")) {
		t.Fatalf("not adopted in place\n%s", got)
	}
	if want, have := decodeTOML(t, rewritten), decodeTOML(t, got); !sameContent([]byte(rewritten), []byte(got)) || have["ui"].(map[string]any)["permission_mode"] != "always-approve" || len(want) != len(have) {
		t.Fatalf("adoption changed a value\n%s", got)
	}
	if drifted, err := l.Drift(c); err != nil || len(drifted) != 0 {
		t.Fatalf("adopted file drifted: %v %v", drifted, err)
	}
}

// The table is adopted where it stands, whatever follows it, and blank lines after it stay.
func TestGrokAdoptsATableFollowedByOthers(t *testing.T) {
	for _, next := range []string{"[ui]\nyolo = false\n", "[[marketplace.sources]]\nname = \"x\"\n"} {
		l := testLayout(t)
		mine := "[mcp_servers.iris]\ncommand = IRIS_COMMAND\nargs = [ \"mcp\", \"--client\", \"grok\" ]\n\n\n" + next
		c, path, _ := grokConfig(t, l, mine)
		if err := c.Install(l, nil); err != nil {
			t.Fatal(err)
		}
		want := wrapBlock("[mcp_servers.iris]\ncommand = "+tomlString(l.Iris())+"\nargs = [\"mcp\", \"--client\", \"grok\"]\nenabled = true\n") + "\n\n" + next
		if got := string(mustRead(t, path)); got != want {
			t.Fatalf("got\n%s\nwant\n%s", got, want)
		}
	}
}

// An unmarked table that is not exactly Iris's own is a person's, and is refused as before,
// with the file left as it was: other args, a key Iris does not write, a table defined twice,
// or one that does not decode. Codex never adopts, even its own table.
func TestGrokRefusesAnUnmarkedTableThatIsNotIrisOwn(t *testing.T) {
	forms := map[string]string{
		"other args":   "[mcp_servers.iris]\ncommand = IRIS_COMMAND\nargs = [\"mcp\", \"--client\", \"codex\"]\n",
		"no args":      "[mcp_servers.iris]\ncommand = IRIS_COMMAND\n",
		"another key":  "[mcp_servers.iris]\ncommand = IRIS_COMMAND\nargs = [\"mcp\", \"--client\", \"grok\"]\nenv = { A = \"1\" }\n",
		"twice":        "[mcp_servers.iris]\ncommand = IRIS_COMMAND\nargs = [\"mcp\", \"--client\", \"grok\"]\n[mcp_servers.iris]\ncommand = \"x\"\n",
		"not decoding": "[mcp_servers.iris]\ncommand = IRIS_COMMAND\nargs = [\"mcp\", \"--client\", \"grok\"\n",
	}
	for name, mine := range forms {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			c, path, written := grokConfig(t, l, mine)
			err := c.Install(l, nil)
			if !errors.Is(err, ErrBroken) || !strings.Contains(err.Error(), "[mcp_servers.iris] is already defined") {
				t.Fatalf("install: %v", err)
			}
			if got := string(mustRead(t, path)); got != written {
				t.Fatalf("file changed\n%s", got)
			}
		})
	}
	l := testLayout(t)
	c, _ := ByName(Codex)
	files, err := c.Files(l)
	if err != nil {
		t.Fatal(err)
	}
	own := "[mcp_servers.iris]\ncommand = " + tomlString(l.Iris()) + "\nargs = [\"mcp\", \"--client\", \"codex\", \"--wake\"]\n"
	if err := os.MkdirAll(filepath.Dir(files[1]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[1], []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Install(l, nil); !errors.Is(err, ErrBroken) {
		t.Fatalf("codex adopted: %v", err)
	}
}

// TOML, like JSON, is compared by value: comments, spacing and array layout are not drift, and
// a changed value is.
func TestSameContentComparesTOMLByValue(t *testing.T) {
	marked := "# >>> iris >>>\n[t]\nargs = [\"a\", \"b\"]\n# <<< iris <<<\n"
	if !sameContent([]byte(marked), []byte("[t]\nargs = [\n    \"a\",\n    \"b\",\n]\n")) {
		t.Fatal("formatting read as drift")
	}
	if sameContent([]byte(marked), []byte("[t]\nargs = [\"a\", \"c\"]\n")) {
		t.Fatal("a changed value read as the same")
	}
}

// A Windows path is written by Grok as a TOML string with escaped backslashes, and decodes to the
// same command, so it is adopted on every platform alike.
func TestGrokAdoptsAWindowsPathWrittenAsATOMLString(t *testing.T) {
	iris := `C:\Users\person\AppData\Local\iris\iris.exe`
	ours := map[string]any{"command": iris, "args": []any{"mcp", "--client", Grok}, "enabled": true}
	block := wrapBlock("[mcp_servers.iris]\ncommand = " + tomlString(iris) + "\nargs = [\"mcp\", \"--client\", \"grok\"]\nenabled = true\n")
	saved := "[ui]\nyolo = false\n\n[mcp_servers.iris]\ncommand = " + tomlString(iris) + "\nargs = [\n    \"mcp\",\n    \"--client\",\n    \"grok\",\n]\nenabled = true\n"
	if got, want := adoptTable(saved, "mcp_servers.iris", ours, block), "[ui]\nyolo = false\n\n"+block; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Written raw, the backslashes are escapes: it decodes to another command or not at all, and is
	// a person's.
	raw := strings.ReplaceAll(saved, tomlString(iris), `"`+iris+`"`)
	if got := adoptTable(raw, "mcp_servers.iris", ours, block); got != raw {
		t.Fatalf("adopted a command that decodes otherwise\n%s", got)
	}
}
