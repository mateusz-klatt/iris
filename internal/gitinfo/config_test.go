package gitinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigStringDecodesValuesAsGitWritesThem(t *testing.T) {
	for raw, want := range map[string]string{
		` C:\\hooks`:                  `C:\hooks`,
		` "C:\\Program Files\\hooks"`: `C:\Program Files\hooks`,
		` "a b" # comment`:            "a b",
		`  x  y  ; comment`:           "x  y",
		` "  lead"`:                   "  lead",
		` say \"hi\"`:                 `say "hi"`,
		` a\tb\nc\bd`:                 "a\tb\nc\bd",
		` ends\`:                      `ends\`,
		` "#not a comment"`:           "#not a comment",
		` x "" y`:                     "x  y",
	} {
		if got := configString(raw); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}

func TestConfigValueReadsTheRepositoryConfigAsGitDoes(t *testing.T) {
	common := t.TempDir()
	config := `# a comment
[core]
	bare = false
	HooksPath = first
; another comment
[remote "origin"]
	hooksPath = not-core
[Core]
	hookspath = "quoted last"
[iris]
	flag
	guardHint
`
	if err := os.WriteFile(filepath.Join(common, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Repo{CommonDir: common}
	if v, ok := r.ConfigValue("core", "hooksPath"); !ok || v != "quoted last" {
		t.Fatalf("%q %v", v, ok)
	}
	if v, ok := r.ConfigValue("iris", "guardhint"); !ok || v != "true" {
		t.Fatalf("a bare key is true: %q %v", v, ok)
	}
	if _, ok := r.ConfigValue("core", "missing"); ok {
		t.Fatal("found a missing key")
	}
	if _, ok := (Repo{CommonDir: filepath.Join(common, "none")}).ConfigValue("core", "bare"); ok {
		t.Fatal("found a key without a config file")
	}
}
