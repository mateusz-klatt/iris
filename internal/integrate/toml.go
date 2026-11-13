package integrate

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	markerStart = "# >>> iris >>>"
	markerEnd   = "# <<< iris <<<"
)

func wrapBlock(body string) string {
	body = strings.Trim(body, "\n")
	return markerStart + "\n" + body + "\n" + markerEnd + "\n"
}

func upsertBlock(existing, block string) (string, error) {
	existing = strings.ReplaceAll(existing, "\r\n", "\n")
	if bytes.Contains([]byte(existing), []byte{0}) {
		return "", ErrBroken
	}
	start := strings.Index(existing, markerStart)
	end := strings.Index(existing, markerEnd)
	outside := existing
	if start >= 0 && end > start {
		outside = existing[:start] + existing[end:]
	}
	if table, ok := definedOutside(outside, block); ok {
		return "", fmt.Errorf("%w: %s is already defined outside Iris's markers; remove or rename it, then run iris integrate again", ErrBroken, table)
	}
	switch {
	case start >= 0 && end > start:
		end += len(markerEnd)
		if end < len(existing) && existing[end] == '\n' {
			end++
		}
		return existing[:start] + block + existing[end:], nil
	case start >= 0 || end >= 0:
		return "", ErrBroken
	case strings.TrimSpace(existing) == "":
		return block, nil
	}
	base := existing
	if !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	return base + "\n" + block, nil
}

// adoptTable gives Iris's markers back to its own table after a client rewrote the file without
// them. Grok saves its config.toml whenever a setting changes, and its writer keeps tables and
// keys but drops comments and reflows arrays (measured on 1.0.77), so the markers go and
// upsertBlock would refuse the file as holding a person's own table. Only a file with no
// markers at all, and a [name] header form, are considered: the table must decode to keys that
// want has and to want's command and args. It is replaced in place by block; every other line
// stays as it is. Anything else is returned unchanged, for upsertBlock to refuse as before.
func adoptTable(existing, name string, want map[string]any, block string) string {
	if strings.Contains(existing, markerStart) || strings.Contains(existing, markerEnd) {
		return existing
	}
	lines := strings.SplitAfter(existing, "\n")
	start := -1
	for i, line := range lines {
		if n, ok := tableName(line); ok && n == name {
			if start >= 0 {
				return existing
			}
			start = i
		}
	}
	if start < 0 {
		return existing
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if _, ok := tableName(lines[i]); ok || strings.HasPrefix(strings.TrimSpace(lines[i]), "[[") {
			end = i
			break
		}
	}
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	var got map[string]any
	if toml.Unmarshal([]byte(strings.Join(lines[start+1:end], "")), &got) != nil {
		return existing
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			return existing
		}
	}
	if !reflect.DeepEqual(got["command"], want["command"]) || !reflect.DeepEqual(got["args"], want["args"]) {
		return existing
	}
	return strings.Join(lines[:start], "") + block + strings.Join(lines[end:], "")
}

// definedOutside reports a table of block that existing already defines, which Iris would
// otherwise append a second time: a duplicate table makes the whole file invalid TOML, and the
// client then refuses to start (gap review A7). Array tables ([[...]]) may repeat. A table is
// also defined by a dotted key or an inline key under its parent.
func definedOutside(existing, block string) (string, bool) {
	for _, line := range strings.Split(block, "\n") {
		name, ok := tableName(line)
		if !ok {
			continue
		}
		i := strings.LastIndex(name, ".")
		parent, leaf := name[:max(i, 0)], name[i+1:]
		current := ""
		for _, l := range strings.Split(existing, "\n") {
			if n, isTable := tableName(l); isTable {
				if n == name {
					return "[" + name + "]", true
				}
				current = n
				continue
			}
			key, value, isKey := strings.Cut(l, "=")
			if !isKey || strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			key = normalizeKey(key)
			full := key
			if current != "" {
				full = current + "." + key
			}
			// mcp_servers = { iris = {…} } defines the table inline in its parent's value.
			compact := strings.ReplaceAll(strings.ReplaceAll(value, " ", ""), "\"", "")
			inline := full == parent && (strings.Contains(compact, "{"+leaf+"=") || strings.Contains(compact, ","+leaf+"="))
			if full == name || strings.HasPrefix(full, name+".") || (parent != "" && current == parent && key == leaf) || inline {
				return "[" + name + "]", true
			}
		}
	}
	return "", false
}

// tableName is the normalized name of a [table] header line; [[array]] headers are not tables.
func tableName(line string) (string, bool) {
	line = strings.TrimSpace(line)
	j := strings.LastIndex(line, "]")
	if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") || j < 0 {
		return "", false
	}
	if rest := strings.TrimSpace(line[j+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", false
	}
	return normalizeKey(line[1:j]), true
}

// normalizeKey drops spaces around dots and the quotes of simple quoted segments.
func normalizeKey(key string) string {
	parts := strings.Split(strings.TrimSpace(key), ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), "\"'")
	}
	return strings.Join(parts, ".")
}

// TOMLString quotes s as a TOML basic string.
func TOMLString(s string) string { return tomlString(s) }

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// validateHookKeys accepts only the documented Kimi [[hooks]] fields.
func validateHookKeys(block string) error {
	allowed := map[string]bool{"event": true, "matcher": true, "command": true, "timeout": true}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || line == "[[hooks]]" {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok || !allowed[strings.TrimSpace(key)] {
			return ErrBroken
		}
	}
	return nil
}
