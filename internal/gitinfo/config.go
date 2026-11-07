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
		value, found = unquote(strings.TrimSpace(v)), true
	}
	return value, found
}
