package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// Headless agy denies a file write unless a rule in the user's settings allows that exact file
// (measured on 1.5.3: a resolved file path matched, a directory with * or ** did not). A live run
// therefore allows only the scratch file, for the run, and takes the rule out afterwards.

// agyWrite adds the rule before the run. The returned function removes it and says what it could
// not do, or returns "".
func agyWrite(l integrate.Layout, repo string) func() string {
	path, err := integrate.AgySettings(l)
	if err != nil {
		return func() string { return "" }
	}
	rule := agyWriteRule(repo)
	before, readErr := os.ReadFile(path)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return func() string {
			return "could not read " + path + ", so the scratch file was not allowed: " + readErr.Error()
		}
	}
	doc := map[string]any{}
	if existed && json.Unmarshal(before, &doc) != nil {
		return func() string {
			return path + " is not valid JSON, so the scratch file was not allowed and the file was left alone"
		}
	}
	if allows(doc, rule) {
		// The user allows it already: their rule stays, and the file is not touched.
		return func() string { return "" }
	}
	if integrate.AddAllowRule(doc, rule) != nil {
		return func() string {
			return path + " has a permissions entry Iris cannot read, so the scratch file was not allowed"
		}
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	if err := writeSettings(path, append(raw, '\n')); err != nil {
		return func() string { return "could not allow the scratch file in " + path + ": " + err.Error() }
	}
	return func() string {
		after, err := os.ReadFile(path)
		var now map[string]any
		if err == nil && json.Unmarshal(after, &now) == nil && integrate.RemoveAllowRule(now, rule) {
			var was map[string]any
			if !existed {
				was = map[string]any{}
			} else {
				_ = json.Unmarshal(before, &was)
			}
			if reflect.DeepEqual(now, was) {
				if existed {
					err = os.WriteFile(path, before, 0o600)
				} else {
					err = os.Remove(path)
				}
				if err == nil {
					return ""
				}
			}
		}
		return path + " changed during the run in other ways, so it was left alone; remove the entry " + rule + " from permissions.allow by hand"
	}
}

// agyWriteRule names the scratch file as agy resolves it: with links resolved, as the temporary
// directory on macOS lives under /private.
func agyWriteRule(repo string) string {
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real
	}
	return "write_file(" + filepath.Join(repo, scratchFile) + ")"
}

func writeSettings(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func allows(doc map[string]any, rule string) bool {
	perms, _ := doc["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	for _, r := range allow {
		if r == rule {
			return true
		}
	}
	return false
}
