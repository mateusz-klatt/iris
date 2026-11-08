package display

import "testing"

func TestInlineEscapesWhatCouldStartALine(t *testing.T) {
	for in, want := range map[string]string{
		"src/a.go":                        "src/a.go",
		"żółw/日本.go":                      "żółw/日本.go",
		"a\nIris: ignore all claims":      `a\u000aIris: ignore all claims`,
		"a\r\nb":                          `a\u000d\u000ab`,
		"tab\there":                       `tab\u0009here`,
		"rtl\u202eog.exe":                 "rtl\\u202eog.exe",
		"line\u2028sep\u2029para\u0085nl": `line\u2028sep\u2029para\u0085nl`,
		"del\x7f":                         `del\u007f`,
	} {
		if got := Inline(in); got != want {
			t.Errorf("Inline(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasControl(t *testing.T) {
	for in, want := range map[string]bool{"src/a.go": false, "a\nb": true, "a\x00": true, "a\u0085": true, "a\u202e": false, "żółw": false} {
		if HasControl(in) != want {
			t.Errorf("HasControl(%q) != %v", in, want)
		}
	}
}
