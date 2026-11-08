package budget

import (
	"encoding/json"
	"strings"
	"testing"
)

// The measure is what encoding/json writes, character by character, so a budget in encoded bytes
// holds for what the client actually receives.
func TestEncodedMatchesEncodingJSON(t *testing.T) {
	for _, s := range []string{
		"plain", "żółw", "quote \" and \\", "line\nbreak\rtab\t", "bell\a nul\x00",
		"<script>&amp;</script>", "sep par ", "bad \xff byte", "emoji 🙂", "",
	} {
		raw, _ := json.Marshal(s)
		if got, want := Encoded(s), len(raw)-2; got != want {
			t.Errorf("%q: %d, encoding/json writes %d", s, got, want)
		}
	}
}

// A cut keeps whole characters and stays within the encoded budget, however the characters
// expand.
func TestCutStaysWithinTheEncodedBudget(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"abc", 10, "abc"},
		{"aż", 2, "a"},      // ż is two bytes: it does not fit in the one left
		{"a\x01b", 6, "a"},  // a control character is six bytes encoded
		{"<<<<", 13, "<<"},  // each < is six
		{"\"q\"", 4, "\"q"}, // a quote is two
		{strings.Repeat("x", 5), 0, ""},
	} {
		got := Cut(tc.s, tc.n)
		if got != tc.want || Encoded(got) > tc.n || !strings.HasPrefix(tc.s, got) {
			t.Errorf("Cut(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
