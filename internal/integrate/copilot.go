package integrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func init() { register(copilotClient{}) }

type copilotClient struct{}

func (copilotClient) Name() string { return Copilot }
func (copilotClient) Human() string {
	return "Enable Copilot CLI extensions with `copilot --experimental` or `/settings experimental on`."
}

func (copilotClient) home(l Layout) (string, error) {
	return l.envDir("COPILOT_HOME", ".copilot")
}

func (c copilotClient) Files(l Layout) ([]string, error) {
	dir, err := c.home(l)
	if err != nil {
		return nil, err
	}
	extensionDir := filepath.Join(dir, "extensions", "iris")
	return []string{
		filepath.Join(dir, "hooks", "iris.json"),
		filepath.Join(dir, "mcp-config.json"),
		filepath.Join(extensionDir, "watcher.mjs"),
		filepath.Join(extensionDir, "extension.mjs"),
	}, nil
}

func (c copilotClient) Install(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	iris := l.Iris()
	watcher := ownedCopilotFile([]byte(copilotWatcher))
	extension := ownedCopilotFile(renderCopilotExtension(iris))
	var adopt []string
	for _, path := range files[2:] {
		existing, absent, err := readExisting(path)
		if err != nil {
			return err
		}
		if absent || l.drift != nil {
			continue
		}
		if !irisCopilotFile(existing) {
			return fmt.Errorf("%w: %s is not a file Iris wrote, or it was edited since; move it aside, then run iris integrate again", ErrBroken, path)
		}
		adopt = append(adopt, path)
	}
	// A file Iris wrote and nobody changed is Iris's alone, so Remove may delete it even when the
	// manifest that recorded its creation is gone.
	for _, path := range adopt {
		if err := l.remember(Copilot, path, nil, true); err != nil {
			return err
		}
	}
	events := []string{"sessionStart", "userPromptSubmitted", "preToolUse", "postToolUse", "postToolUseFailure", "agentStop", "sessionEnd"}
	if err := l.update(Copilot, files[0], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			if err := requireVersion1(doc); err != nil {
				return err
			}
			for _, event := range events {
				hook := map[string]any{
					"type":       "command",
					"bash":       Guard("linux", Copilot, iris, event),
					"powershell": Guard("windows", Copilot, iris, event),
					"timeoutSec": eventTimeout(Copilot, event),
				}
				if err := putFlatHook(doc, event, hook, "--client copilot --event "+event); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if err := l.update(Copilot, files[1], func(existing []byte) ([]byte, error) {
		return writeObject(existing, func(doc map[string]any) error {
			mcpServers(doc, iris, Copilot)
			return nil
		})
	}); err != nil {
		return err
	}
	if err := l.update(Copilot, files[2], func([]byte) ([]byte, error) {
		return watcher, nil
	}); err != nil {
		return err
	}
	return l.update(Copilot, files[3], func([]byte) ([]byte, error) {
		return extension, nil
	})
}

func (c copilotClient) Remove(l Layout, _ Plugins) error {
	files, err := c.Files(l)
	if err != nil {
		return err
	}
	// An extension file carries its own proof that Iris wrote it, so one nobody changed is removed
	// even when no manifest records it, as after the manifest was lost. A file that cannot be read
	// is left to unwire, which reports it if it was recorded.
	for _, path := range files[2:] {
		if data, absent, err := readExisting(path); err == nil && !absent && irisCopilotFile(data) {
			if err := l.remember(Copilot, path, nil, true); err != nil {
				return err
			}
		}
	}
	return l.unwire(Copilot, stripCopilot())
}

func (c copilotClient) Canary(l Layout) (Invocation, error) {
	files, err := c.Files(l)
	if err != nil {
		return Invocation{}, err
	}
	doc, err := readJSON(files[0])
	if err != nil {
		return Invocation{}, err
	}
	root, _ := doc.(map[string]any)
	hooks, _ := root["hooks"].(map[string]any)
	arr, _ := hooks["preToolUse"].([]any)
	if len(arr) == 0 {
		return Invocation{}, ErrNotConfigured
	}
	item, _ := arr[0].(map[string]any)
	field := "bash"
	if l.goos() == "windows" {
		field = "powershell"
	}
	shell, _ := item[field].(string)
	if shell == "" {
		return Invocation{}, ErrNotConfigured
	}
	return Invocation{Shell: shell, Kind: clientShell(l.goos(), Copilot)}, nil
}

func requireVersion1(doc map[string]any) error {
	v, ok := doc["version"]
	if !ok {
		doc["version"] = 1
		return nil
	}
	if n, ok := v.(json.Number); !ok || n.String() != "1" {
		return ErrBroken
	}
	return nil
}

func renderCopilotExtension(iris string) []byte {
	executable, _ := json.Marshal(iris)
	return []byte(strings.Replace(copilotExtension, `"__IRIS_EXECUTABLE__"`, string(executable), 1))
}

// copilotOwner opens each extension file Iris writes. It carries the SHA-256 of everything below
// it, so Iris can tell a file it wrote and nobody changed, which it replaces or removes whichever
// version or iris path wrote it, from a file of the user's, which it never touches.
const copilotOwner = "// Written by iris integrate copilot. Iris replaces or removes this file only while the lines below still match sha256:"

func ownedCopilotFile(body []byte) []byte {
	sum := sha256.Sum256(body)
	return append([]byte(copilotOwner+hex.EncodeToString(sum[:])+"\n"), body...)
}

// irisCopilotFile reports whether data is an extension file Iris wrote and nobody has changed.
func irisCopilotFile(data []byte) bool {
	head, body, ok := bytes.Cut(data, []byte("\n"))
	digest, owned := strings.CutPrefix(string(head), copilotOwner)
	sum := sha256.Sum256(body)
	return ok && owned && digest == hex.EncodeToString(sum[:])
}

// errChangedExtension keeps a recorded extension file that no longer matches its digest, emptied
// included: what is in it now is somebody's, and deleting it would lose their work. unwire keeps
// the record too, so the command fails until the file is moved aside and then finishes on a retry.
var errChangedExtension = errors.New("not a file Iris wrote, or it was edited since, so it stays; move it aside, then run iris integrate --remove copilot again")

func stripCopilot() stripFunc {
	strip := stripClient(Copilot)
	return func(path string, data []byte) ([]byte, bool, error) {
		if filepath.Ext(path) != ".mjs" {
			return strip(path, data)
		}
		if !irisCopilotFile(data) {
			return nil, false, errChangedExtension
		}
		return nil, true, nil
	}
}
