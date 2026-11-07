package integrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// stripFunc returns data without Iris's own entries and reports whether it
// found any. It must refuse what it cannot parse by returning ErrBroken.
type stripFunc func(path string, data []byte) ([]byte, bool, error)

// unwire takes Iris out of every file it recorded for client. The file as it is
// now is the input, never the backup, so what changed since install survives.
// A file that cannot be edited safely stays as it is and stays in the manifest,
// so a retry can finish it. The other files are still done.
func (l Layout) unwire(client string, strip stripFunc) error {
	dir, err := l.state()
	if err != nil {
		return err
	}
	m, err := loadManifest(dir)
	if err != nil {
		return err
	}
	var paths []string
	for path, rec := range m.Files {
		if rec.Client == client {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	var failed []error
	for _, path := range paths {
		if err := unwireFile(dir, path, m.Files[path], strip); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", path, err))
			continue
		}
		delete(m.Files, path)
	}
	return errors.Join(append(failed, saveManifest(dir, m))...)
}

func unwireFile(stateDir, path string, rec manifestFile, strip stripFunc) error {
	var backup string
	if rec.Backup != "" {
		backup = filepath.Join(stateDir, rec.Backup)
	}
	if err := refuseSpecial(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(backup)
		removeDirs(rec)
		return nil
	}
	if err != nil {
		return err
	}
	next, removed, err := strip(path, data)
	if err != nil {
		return err
	}
	switch {
	case rec.Created && hasNoContent(next):
		if err = os.Remove(path); err == nil {
			removeDirs(rec)
		}
	case removed:
		err = atomicWrite(path, asFound(path, next, backup, strip))
	}
	if err != nil {
		return err
	}
	_ = os.Remove(backup)
	return nil
}

// removeDirs takes out the directories Iris created for a file, deepest first. One that holds
// anything stays. Measured on Grok: ~/.grok/hooks was left behind, empty, after remove.
// A path that is now a link stays: removing it would delete the user's link, whatever it points to.
func removeDirs(rec manifestFile) {
	for _, d := range rec.Dirs {
		if info, err := os.Lstat(d); err == nil && info.IsDir() {
			_ = os.Remove(d)
		}
	}
}

// asFound returns the backup bytes when next differs from them only by
// formatting, so an untouched file comes back exactly as Iris found it. A backup
// holding Iris entries of its own is never used: restoring it would reinstall
// an older integration.
func asFound(path string, next []byte, backup string, strip stripFunc) []byte {
	original, err := os.ReadFile(backup)
	if err != nil {
		return next
	}
	clean, removed, err := strip(path, original)
	if err != nil || removed || !sameText(clean, next) {
		return next
	}
	return original
}

func sameText(a, b []byte) bool {
	return bytes.Equal(normalizeText(a), normalizeText(b))
}

func normalizeText(b []byte) []byte {
	return bytes.TrimSpace(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")))
}

func hasNoContent(b []byte) bool {
	switch string(bytes.TrimSpace(b)) {
	case "", "{}", "[]":
		return true
	default:
		return false
	}
}

// stripClient handles the files every client but Claude writes. Hooks and MCP
// servers never share a file, and each MCP file has mcp in its name.
func stripClient(client string) stripFunc {
	return func(path string, data []byte) ([]byte, bool, error) {
		switch {
		case strings.HasSuffix(path, ".toml"):
			return stripBlock(data)
		case strings.Contains(filepath.Base(path), "mcp"):
			return stripObject(data, removeMCPServer)
		default:
			return stripObject(data, func(doc map[string]any) bool {
				return removeHooks(doc, client)
			})
		}
	}
}

func stripObject(data []byte, edit func(map[string]any) bool) ([]byte, bool, error) {
	doc, err := readObject(data)
	if err != nil {
		return nil, false, err
	}
	removed := edit(doc)
	next, err := marshalJSON(doc)
	return next, removed, err
}

func removeMCPServer(doc map[string]any) bool {
	servers, _ := doc["mcpServers"].(map[string]any)
	_, removed := servers["iris"]
	delete(servers, "iris")
	if servers != nil && len(servers) == 0 {
		delete(doc, "mcpServers")
	}
	return removed
}

// removeHooks drops Iris's hooks and any container left empty. Copilot and
// Cursor require a version beside their hooks, and Install adds it, so it goes
// once no hooks remain. That loses a version the user wrote in a hooks file
// which has no hooks, which is a key those clients do not read without them.
func removeHooks(doc map[string]any, client string) bool {
	removed := false
	if client == Antigravity {
		_, removed = doc["iris"]
		delete(doc, "iris")
	}
	hooks, _ := doc["hooks"].(map[string]any)
	for event, cur := range hooks {
		arr, ok := cur.([]any)
		if !ok {
			continue
		}
		kept, changed := dropHooks(arr, func(v any) bool {
			return treeContains(v, "--client "+client+" --event "+event) || matchesExec(v, client, event)
		})
		removed = removed || changed
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if hooks != nil && len(hooks) == 0 {
		delete(doc, "hooks")
	}
	if _, ok := doc["hooks"]; !ok && (client == Copilot || client == Cursor) {
		delete(doc, "version")
	}
	return removed
}

// dropHooks removes matching hooks from a flat list or from the groups of a
// nested one. A group that loses its last hook goes with it, and a group that
// also holds someone else's hook stays.
func dropHooks(arr []any, own func(any) bool) ([]any, bool) {
	kept := []any{}
	changed := false
	for _, item := range arr {
		group, _ := item.(map[string]any)
		inner, nested := group["hooks"].([]any)
		if !nested {
			if own(item) {
				changed = true
			} else {
				kept = append(kept, item)
			}
			continue
		}
		left, innerChanged := dropHooks(inner, own)
		if innerChanged {
			changed = true
			if len(left) == 0 {
				continue
			}
			group["hooks"] = left
		}
		kept = append(kept, item)
	}
	return kept, changed
}

// stripBlock removes the marked TOML block and the blank line Install put before
// it. Line endings elsewhere are kept.
func stripBlock(data []byte) ([]byte, bool, error) {
	text := string(data)
	if strings.ContainsRune(text, 0) {
		return nil, false, ErrBroken
	}
	start := strings.Index(text, markerStart)
	end := strings.Index(text, markerEnd)
	if start < 0 && end < 0 {
		return data, false, nil
	}
	if start < 0 || end < start {
		return nil, false, ErrBroken
	}
	head := text[:start]
	tail := text[end+len(markerEnd):]
	tail = strings.TrimPrefix(strings.TrimPrefix(tail, "\r"), "\n")
	if tail == "" && (strings.HasSuffix(head, "\n\n") || strings.HasSuffix(head, "\n\r\n")) {
		head = head[:strings.LastIndex(head[:len(head)-1], "\n")+1]
	}
	return []byte(head + tail), true, nil
}

// stripClaude handles the plugin files, which Iris renders from templates.
func stripClaude(root string) stripFunc {
	return func(path string, data []byte) ([]byte, bool, error) {
		rel, _ := filepath.Rel(root, path)
		template, _ := claudeFS.ReadFile(claudeTemplateDir + "/" + filepath.ToSlash(rel))
		switch {
		case strings.HasSuffix(path, ".md"):
			return stripText(data, template)
		case strings.HasSuffix(path, "monitors.json"):
			return stripMonitors(data)
		case strings.HasSuffix(path, "hooks.json"):
			return stripObject(data, func(doc map[string]any) bool {
				return removeHooks(doc, Claude)
			})
		case strings.HasSuffix(path, ".mcp.json"):
			return stripObject(data, removeMCPServer)
		default:
			return stripObject(data, func(doc map[string]any) bool {
				return removeTemplateKeys(doc, template)
			})
		}
	}
}

// stripText removes the exact template. A text somebody edited no longer
// matches, so it stays: Iris cannot tell which part of it is theirs.
func stripText(data, template []byte) ([]byte, bool, error) {
	if !bytes.Contains(data, template) {
		return data, false, nil
	}
	return bytes.Replace(data, template, nil, 1), true, nil
}

func stripMonitors(data []byte) ([]byte, bool, error) {
	var arr []any
	if len(bytes.TrimSpace(data)) != 0 {
		if err := decodeJSON(data, &arr); err != nil {
			return nil, false, err
		}
	}
	kept := []any{}
	removed := false
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m["name"] == "iris" {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	next, err := marshalJSON(kept)
	return next, removed, err
}

func removeTemplateKeys(doc map[string]any, template []byte) bool {
	var add map[string]any
	_ = json.Unmarshal(template, &add)
	removed := false
	for key := range add {
		if _, ok := doc[key]; ok {
			delete(doc, key)
			removed = true
		}
	}
	return removed
}

// pruneEmptyDirs removes directories under and including root that hold
// nothing. A directory with anything left in it stays.
func pruneEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	slices.Reverse(dirs)
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
}
