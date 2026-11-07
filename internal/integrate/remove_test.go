package integrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manualMark = "MANUAL_KEEP"

// editByHand changes a file the way a person would after Iris installed.
func editByHand(t *testing.T, path string) {
	t.Helper()
	data := mustRead(t, path)
	var next []byte
	switch {
	case strings.HasSuffix(path, "monitors.json"):
		var arr []any
		if err := json.Unmarshal(data, &arr); err != nil {
			t.Fatal(err)
		}
		arr = append(arr, map[string]any{"name": "mine", "command": manualMark})
		next, _ = marshalJSON(arr)
	case strings.HasSuffix(path, ".json"):
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		doc["manual"] = manualMark
		next, _ = marshalJSON(doc)
	case strings.HasSuffix(path, ".toml"):
		next = append(data, []byte("manual = \""+manualMark+"\"\n")...)
	default:
		next = append(data, []byte(manualMark+"\n")...)
	}
	if err := os.WriteFile(path, next, 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNoIris(t *testing.T, path string, text string) {
	t.Helper()
	for _, needle := range []string{"--event", "mcp_servers.iris", `"iris"`, markerStart, notificationPlain} {
		if strings.Contains(text, needle) {
			t.Fatalf("%s still holds %q\n%s", path, needle, text)
		}
	}
}

func TestRemoveKeepsAManualChangeToAFileIrisCreated(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			files, err := c.Files(l)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				editByHand(t, f)
			}
			if err := c.Remove(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				text := string(mustRead(t, f))
				if !strings.Contains(text, manualMark) {
					t.Fatalf("%s lost the manual change\n%s", f, text)
				}
				assertNoIris(t, f, text)
			}
		})
	}
}

func TestRemoveKeepsAManualChangeToAFileThatExisted(t *testing.T) {
	for _, c := range All() {
		t.Run(c.Name(), func(t *testing.T) {
			l := testLayout(t)
			files, err := c.Files(l)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				switch {
				case strings.HasSuffix(f, "monitors.json"):
					seedFile(t, f, "[]\n")
				case strings.HasSuffix(f, ".json"):
					seedFile(t, f, "{}\n")
				default:
					seedFile(t, f, "# mine\n")
				}
			}
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				editByHand(t, f)
			}
			if err := c.Remove(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				text := string(mustRead(t, f))
				if !strings.Contains(text, manualMark) {
					t.Fatalf("%s lost the manual change\n%s", f, text)
				}
				assertNoIris(t, f, text)
			}
		})
	}
}

func TestRemoveKeepsOtherHooksAndServersAddedAfterInstall(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Codex)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, files[0]), &doc); err != nil {
		t.Fatal(err)
	}
	hooks := doc["hooks"].(map[string]any)
	stop := hooks["Stop"].([]any)
	// One person added a hook beside Iris's in its own group, one added a new group.
	group := stop[0].(map[string]any)
	group["hooks"] = append(group["hooks"].([]any), map[string]any{"type": "command", "command": "beside"})
	hooks["Stop"] = append(stop, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "after"}}})
	body, _ := marshalJSON(doc)
	if err := os.WriteFile(files[0], body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	text := string(mustRead(t, files[0]))
	if !strings.Contains(text, "beside") || !strings.Contains(text, "after") {
		t.Fatalf("lost a hook\n%s", text)
	}
	if strings.Contains(text, "--client codex") || strings.Contains(text, "SessionStart") {
		t.Fatalf("kept Iris\n%s", text)
	}
}

// The reported failure: the backup held an older Iris, and removal restored it.
func TestRemoveDoesNotBringBackAnIrisFromTheBackup(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Copilot)
	files, _ := c.Files(l)
	old := `C:\old\iris.exe`
	stale := `{"mcpServers":{"iris":{"command":` + mustQuote(old) + `},"other":{"command":"x"}}}` + "\n"
	seedFile(t, files[1], stale)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	text := string(mustRead(t, files[1]))
	if strings.Contains(text, "iris") {
		t.Fatalf("old integration came back\n%s", text)
	}
	if !strings.Contains(text, "other") {
		t.Fatalf("lost the other server\n%s", text)
	}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestRemoveRestoresTheOriginalBytesWhenNothingElseChanged(t *testing.T) {
	cases := map[string]string{
		Codex:  "{\n    \"z\": 1,\n    \"a\": [ 1, 2 ]\n}",
		Cursor: "{\"version\":1,\"hooks\":{}}",
		Claude: "   \n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			l := testLayout(t)
			c, _ := ByName(name)
			files, _ := c.Files(l)
			target := files[0]
			if name == Claude {
				target = files[2]
			}
			seedFile(t, target, body)
			if err := c.Install(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			if err := c.Remove(l, &fakePlugins{}); err != nil {
				t.Fatal(err)
			}
			if got := string(mustRead(t, target)); got != body {
				t.Fatalf("got %q want %q", got, body)
			}
		})
	}
}

func TestRemoveRestoresATOMLFileWithCarriageReturns(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Kimi)
	files, _ := c.Files(l)
	body := "a = 1\r\nb = 2"
	seedFile(t, files[0], body)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[0])); got != body {
		t.Fatalf("got %q", got)
	}
}

func TestRemoveKeepsCarriageReturnsAndTextAroundTheBlock(t *testing.T) {
	head := "# mine\r\n\r\n"
	tail := "# after\r\n"
	got, removed, err := stripBlock([]byte(head + markerStart + "\r\nx = 1\r\n" + markerEnd + "\r\n" + tail))
	if err != nil || !removed {
		t.Fatal(removed, err)
	}
	if string(got) != head+tail {
		t.Fatalf("%q", got)
	}
	got, _, err = stripBlock([]byte(head + markerStart + "\r\n" + markerEnd + "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# mine\r\n" {
		t.Fatalf("%q", got)
	}
}

func TestRemoveDoesNotRecreateAFileDeletedByHand(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	files, _ := c.Files(l)
	seedFile(t, files[0], "{\"mine\": true}\n")
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recreated: %v", err)
	}
	m, err := loadManifest(l.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 0 {
		t.Fatalf("manifest still lists %v", m.Files)
	}
}

func TestRemoveLeavesAFileWithoutIrisEntriesUntouched(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Grok)
	files, _ := c.Files(l)
	seedFile(t, files[0], "{\n}\n")
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	body := "{ \"mine\":   true }\n"
	if err := os.WriteFile(files[0], []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, files[0])); got != body {
		t.Fatalf("rewrote %q", got)
	}
}

func TestRemoveRefusesABrokenFileAndFinishesTheRest(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Codex)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	if err := os.WriteFile(files[0], []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); !errors.Is(err, ErrBroken) || !strings.Contains(err.Error(), files[0]) {
		t.Fatalf("err %v", err)
	}
	if got := string(mustRead(t, files[0])); got != "{" {
		t.Fatalf("touched the broken file: %q", got)
	}
	if _, err := os.Stat(files[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the other file was not finished: %v", err)
	}
	if err := os.WriteFile(files[0], []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a retry did not finish: %v", err)
	}
}

func TestRemoveRefusesAMarkerWithoutItsPartner(t *testing.T) {
	for _, body := range []string{markerStart + "\n", markerEnd + "\n", markerEnd + "\n" + markerStart + "\n", "a\x00"} {
		if _, _, err := stripBlock([]byte(body)); !errors.Is(err, ErrBroken) {
			t.Fatalf("%q: %v", body, err)
		}
	}
}

func TestRemoveRefusesASymlink(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Grok)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	real := filepath.Join(l.Home, "real.json")
	seedFile(t, real, "{}")
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, files[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); !errors.Is(err, ErrBroken) {
		t.Fatalf("err %v", err)
	}
}

func TestRemoveKeepsLargeNumbersExact(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Cursor)
	if err := c.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, files[1]), &doc); err != nil {
		t.Fatal(err)
	}
	doc["big"] = json.Number("12345678901234567890")
	body, _ := marshalJSON(doc)
	if err := os.WriteFile(files[1], body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if text := string(mustRead(t, files[1])); !strings.Contains(text, "12345678901234567890") {
		t.Fatalf("number changed\n%s", text)
	}
}

func TestClaudeRemoveKeepsTheTreeOnlyAroundWhatIsStillThere(t *testing.T) {
	l := testLayout(t)
	c, _ := ByName(Claude)
	if err := c.Install(l, &fakePlugins{}); err != nil {
		t.Fatal(err)
	}
	files, _ := c.Files(l)
	root, _ := claudeClient{}.root(l)
	extra := filepath.Join(root, "plugins", "iris", "notes.txt")
	seedFile(t, extra, "mine\n")
	if err := c.Remove(l, &fakePlugins{}); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, extra)); got != "mine\n" {
		t.Fatalf("%q", got)
	}
	if _, err := os.Stat(filepath.Dir(files[5])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty directory left: %v", err)
	}
}

// Remove takes out the directories install created, and only those: ~/.grok/hooks did not exist
// and was left behind empty (measured), while ~/.copilot/hooks existed, empty, and must stay.
func TestRemoveTakesOutOnlyTheDirectoriesInstallCreated(t *testing.T) {
	l := testLayout(t)
	grok, _ := ByName(Grok)
	copilot, _ := ByName(Copilot)
	gfiles, _ := grok.Files(l)
	cfiles, _ := copilot.Files(l)
	ghome := filepath.Dir(filepath.Dir(gfiles[0])) // ~/.grok exists, ~/.grok/hooks does not
	chooks := filepath.Dir(cfiles[0])              // ~/.copilot/hooks exists, empty
	for _, d := range []string{ghome, chooks} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []Client{grok, copilot} {
		if err := c.Install(l, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Remove(l, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Dir(gfiles[0])); !os.IsNotExist(err) {
		t.Fatalf("the hooks directory install created is still there: %v", err)
	}
	for _, d := range []string{ghome, chooks} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s existed before install and must stay: %v", d, err)
		}
	}
}

func TestACreatedDirectoryWithSomethingInItStays(t *testing.T) {
	l := testLayout(t)
	grok, _ := ByName(Grok)
	files, _ := grok.Files(l)
	if err := grok.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(filepath.Dir(files[0]), "mine.json")
	if err := os.WriteFile(mine, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The user also deleted Iris's file; remove still tidies what it can.
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if err := grok.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal(err)
	}
}

// A created directory the user replaced with a link keeps the link: os.Remove would delete it.
func TestACreatedDirectoryReplacedByALinkStays(t *testing.T) {
	l := testLayout(t)
	grok, _ := ByName(Grok)
	files, _ := grok.Files(l)
	if err := grok.Install(l, nil); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Dir(files[0])
	elsewhere := t.TempDir()
	if err := os.Rename(files[0], filepath.Join(elsewhere, filepath.Base(files[0]))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hooks); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, hooks); err != nil {
		t.Fatal(err)
	}
	if err := grok.Remove(l, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(hooks); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the user's link is gone: %v", err)
	}
}
