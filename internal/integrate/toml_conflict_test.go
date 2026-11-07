package integrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A person's own, unmarked [mcp_servers.iris] would become a duplicate table, which makes the
// whole file invalid and stops the client starting (gap A7). Install refuses and writes nothing.
func TestInstallRefusesAnUnmarkedIrisTable(t *testing.T) {
	forms := map[string]string{
		"table":            "[mcp_servers.iris]\ncommand = \"mine\"\n",
		"quoted, comment":  "[ mcp_servers . \"iris\" ] # mine\ncommand = \"mine\"\n",
		"inline in parent": "[mcp_servers]\niris = { command = \"mine\" }\n",
		"dotted in parent": "[mcp_servers]\niris.command = \"mine\"\n",
		"dotted top level": "mcp_servers.iris.command = \"mine\"\n",
		"inline at root":   "mcp_servers = { iris = { command = \"mine\" } }\n",
		"inline, second":   "mcp_servers = { other = {}, \"iris\" = { command = \"mine\" } }\n",
	}
	for name, mine := range forms {
		for _, client := range []string{Codex, Grok} {
			t.Run(client+"/"+name, func(t *testing.T) {
				l := testLayout(t)
				c, _ := ByName(client)
				files, _ := c.Files(l)
				if err := os.MkdirAll(filepath.Dir(files[1]), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(files[1], []byte(mine), 0o600); err != nil {
					t.Fatal(err)
				}
				err := c.Install(l, nil)
				if !errors.Is(err, ErrBroken) || !strings.Contains(err.Error(), "[mcp_servers.iris] is already defined") {
					t.Fatalf("install: %v", err)
				}
				if got := string(mustRead(t, files[1])); got != mine {
					t.Fatalf("file changed\n%s", got)
				}
			})
		}
	}
}

// Other tables, comments, array tables and Iris's own marked table are not conflicts.
func TestUnrelatedTablesAreNotConflicts(t *testing.T) {
	block := wrapBlock("[mcp_servers.iris]\ncommand = \"iris\"\n")
	existing := "# [mcp_servers.iris] in a comment\n[mcp_servers.other]\ncommand = \"x\"\nirisx = 1\n[[hooks]]\nevent = \"Stop\"\n" + block
	if _, err := upsertBlock(existing, block); err != nil {
		t.Fatal(err)
	}
	// A line of a multi-line array starts with [ but is no table header.
	nested := "pairs = [\n  [\"mcp_servers.iris\", 2],\n]\n"
	if _, err := upsertBlock(nested, block); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertBlock("mcp_servers = { other = { command = \"iris\" } }\n", block); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertBlock("[mcp_servers]\nother = { command = \"x\" }\n", block); err != nil {
		t.Fatal(err)
	}
	kimi := wrapBlock("[[hooks]]\nevent = \"Stop\"\n")
	if _, err := upsertBlock("[[hooks]]\nevent = \"Stop\"\ncommand = \"mine\"\n", kimi); err != nil {
		t.Fatal(err)
	}
}
