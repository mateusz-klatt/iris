// Package display makes agent-supplied text safe to put into a line a model reads.
package display

import (
	"fmt"
	"strings"
	"unicode"
)

// Inline escapes control and format characters, such as newline, carriage return
// and bidirectional overrides, as \u escapes. A value from another agent, such as
// a claim path or a branch, then cannot start a line of its own in Iris's context
// or pass itself off as Iris's own guidance. Everything else is kept as written.
func Inline(s string) string {
	if strings.IndexFunc(s, unsafe) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafe(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// HasControl reports a C0, DEL or C1 control character, which no claim path needs.
func HasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

func unsafe(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' '
}
