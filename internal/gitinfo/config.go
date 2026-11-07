package gitinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// ConfigValue reads one key from the repository's own config file, the last
// value winning as in git. Section and key are matched without case, and a
// subsection is not supported. Included files, the user's global config and
// the system config are not read: a hook must not walk beyond the git dir,
// so a value set only there is reported as absent.
func (r Repo) ConfigValue(section, key string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(r.CommonDir, "config"))
	if err != nil {
		return "", false
	}
	want := "[" + strings.ToLower(section) + "]"
	var current, value string
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.ToLower(line)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if current != want || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		if !ok {
			// A key with no value is true in git.
			v = "true"
		}
		value, found = configString(v), true
	}
	return value, found
}

// configString decodes a value as git writes it. Double quotes keep
// whitespace and comment characters, a backslash escapes \, ", n, t and b,
// and # or ; outside quotes starts a comment. git config stores a Windows
// path's backslashes escaped, as C:\\hooks.
func configString(raw string) string {
	var b strings.Builder
	quoted := false
	// Unquoted whitespace is kept only between other characters, so it is
	// held back until something follows it, and dropped before the first.
	pending := ""
	put := func(s string) {
		if b.Len() > 0 {
			b.WriteString(pending)
		}
		pending = ""
		b.WriteString(s)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\' && i+1 < len(raw):
			i++
			put(unescape(raw[i]))
		case c == '"':
			quoted = !quoted
			if b.Len() > 0 {
				b.WriteString(pending)
			}
			pending = ""
		case !quoted && (c == '#' || c == ';'):
			return b.String()
		case !quoted && (c == ' ' || c == '\t'):
			pending += string(c)
		default:
			put(string(c))
		}
	}
	return b.String()
}

func unescape(c byte) string {
	switch c {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'b':
		return "\b"
	}
	return string(c)
}
