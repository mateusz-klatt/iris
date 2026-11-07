package integrate

import (
	"bytes"
	"strings"
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
