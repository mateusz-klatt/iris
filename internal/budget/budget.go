// Package budget measures message bodies as a client sees them: encoded as a JSON string, which is
// what a tool result's size limit applies to (ADR 008).
package budget

import "unicode/utf8"

// Bodies is how many bytes of encoded message bodies one MCP result carries: under the 25,000
// tokens Claude Code allows a tool result by default, with room for the rest.
const Bodies = 32 << 10

// Encoded is the size of s inside a JSON string, as encoding/json writes it, quotes excluded.
func Encoded(s string) int {
	n := 0
	for _, r := range s {
		n += runeSize(r)
	}
	return n
}

// Cut is the longest prefix of s whose encoded size is within n and that ends on a character
// boundary.
func Cut(s string, n int) string {
	used := 0
	for i, r := range s {
		size := runeSize(r)
		if used+size > n {
			return s[:i]
		}
		used += size
	}
	return s
}

// runeSize is one character's encoded size. encoding/json escapes quotes and backslashes in two
// bytes, newline, carriage return and tab too, other control characters in six, the HTML
// characters <, > and & and the line separators U+2028 and U+2029 in six, and writes an invalid
// byte as the six-byte �.
func runeSize(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029':
		return 6
	}
	return utf8.RuneLen(r)
}
