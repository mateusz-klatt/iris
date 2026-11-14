package integrate

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

func init() { register(kimiClient{}) }

type kimiClient struct{}

func (kimiClient) Name() string  { return Kimi }
func (kimiClient) Human() string { return "" }

func (c kimiClient) home(l Layout) (string, error) {
	home, err := l.envDir("KIMI_CODE_HOME", ".kimi-code")
	if err != nil {
		return "", err
	}
	return filepath.Abs(home)
}

func (c kimiClient) Files(l Layout) ([]string, error) {
	dir, err := c.home(l)
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "config.toml"), filepath.Join(dir, "mcp.json")}, nil
}

func (c kimiClient) Install(l Layout, _ Plugins) error { return c.install(l, nil) }

func (c kimiClient) install(l Layout, wake *bool) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	existingMCP, _, err := readExisting(files[1])
	if err != nil {
		return err
	}
	current, err := readObject(existingMCP)
	if err != nil {
		return err
	}
	iris := l.Iris()
	// The watcher reads only the OS account's pinned configuration, so wake can be turned on only
	// where that is the configuration in use.
	root, err := KimiWakeRoot(l)
	if err != nil {
		return err
	}
	if wake != nil && *wake {
		configDir, err := l.IrisConfigDir()
		if err != nil {
			return err
		}
		if pinned := filepath.Dir(root); !sameConfigDir(l.goos(), configDir, pinned) {
			return fmt.Errorf("kimi wake requires the default Iris configuration directory %s; current environment resolves %s", pinned, configDir)
		}
	}
	choice, err := kimiChoice(root, current, iris, wake)
	if err != nil {
		return err
	}
	events := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"}
	var body strings.Builder
	for _, event := range events {
		command := hookShell(iris, Kimi, event)
		if l.goos() == "windows" {
			// Kimi's Windows spawning is undocumented: cmd is an unverified assumption.
			command = hookCmd(iris, Kimi, event)
		}
		fmt.Fprintf(&body, "[[hooks]]\nevent = %s\ncommand = %s\ntimeout = %d\n\n",
			tomlString(event), tomlString(command), eventTimeout(Kimi, event))
	}
	block := wrapBlock(strings.TrimRight(body.String(), "\n"))
	// The skill comes last, so it never tells the model about a watcher whose files are not there.
	for _, f := range []struct {
		path   string
		render func([]byte) ([]byte, error)
	}{
		{files[0], func(existing []byte) ([]byte, error) { return upsertHooks(existing, block) }},
		{kimiwake.PreferencePath(root), func([]byte) ([]byte, error) { return []byte(choice + "\n"), nil }},
		{files[1], func(existing []byte) ([]byte, error) {
			return writeObject(existing, func(doc map[string]any) error {
				kimiMCPConfig(doc, iris, filepath.Dir(files[0]))
				return nil
			})
		}},
	} {
		if err := l.update(Kimi, f.path, f.render); err != nil {
			return err
		}
	}
	return c.skill(l, filepath.Dir(files[0]), kimiwake.On(choice))
}

// skill installs the native skill while wake is on, and otherwise takes back the text Iris wrote.
// A copy a person changed stays.
func (c kimiClient) skill(l Layout, home string, on bool) error {
	path := filepath.Join(home, "skills", "iris", "SKILL.md")
	if on {
		return l.update(Kimi, path, func(existing []byte) ([]byte, error) { return kimiSkillText(existing), nil })
	}
	return l.unwireWhere(Kimi, stripClient(Kimi), func(p string) bool { return p == path })
}

// upsertHooks refuses a block with an unknown key before it reaches the file,
// because Kimi drops its whole hooks section over one.
func upsertHooks(existing []byte, block string) ([]byte, error) {
	if err := validateHookKeys(block); err != nil {
		return nil, err
	}
	next, err := upsertBlock(string(existing), block)
	return []byte(next), err
}

// Remove takes Iris out of Kimi's files and the wake preference, then stops the watchers that
// removal leaves without a purpose. A failed stop is reported and does not keep the files.
func (c kimiClient) Remove(l Layout, _ Plugins) error {
	return errors.Join(l.unwire(Kimi, stripClient(Kimi)), stopKimiWatchers(l))
}

func (c kimiClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	data, err := readFile(files[0])
	if err != nil {
		return Invocation{}, err
	}
	shell := tomlCommand(string(data), "SessionStart")
	if shell == "" {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell, Kind: clientShell(l.goos(), Kimi)}, nil
}

func tomlCommand(file, event string) string {
	start := strings.Index(file, markerStart)
	end := strings.Index(file, markerEnd)
	if start < 0 || end < start {
		return ""
	}
	body := file[start:end]
	if !strings.Contains(body, `event = "`+event+`"`) {
		return ""
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "command = ") {
			continue
		}
		if strings.Contains(line, "--event "+event) {
			return unquoteTOML(strings.TrimPrefix(line, "command = "))
		}
	}
	return ""
}

func unquoteTOML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return ""
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	esc := false
	for _, r := range s {
		if esc {
			switch r {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteRune(r)
			}
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
