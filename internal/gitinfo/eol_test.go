package gitinfo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// crlfRepo commits files LF and then rewrites the first crlf of them with CRLF in the worktree,
// as Windows git with core.autocrlf=true checks them out. No global or system config applies.
func crlfRepo(t *testing.T, files, crlf int) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	for i := range files {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.txt", i)), []byte("one\ntwo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "lf")
	for i := range crlf {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.txt", i)), []byte("one\r\ntwo\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return root
}

// Files git stores LF and the worktree holds CRLF are counted when nothing converts them.
func TestLineEndingsCountsCRLFThisGitReadsAsChanged(t *testing.T) {
	root := crlfRepo(t, 5, 3)
	crlf, text, err := LineEndings(context.Background(), root)
	if err != nil || crlf != 3 || text != 5 {
		t.Fatalf("%d of %d: %v", crlf, text, err)
	}
}

// core.autocrlf true or input converts CRLF when git reads the worktree, so nothing is counted.
func TestLineEndingsCountsNothingWhenThisGitConverts(t *testing.T) {
	for _, value := range []string{"true", "input", "INPUT"} {
		root := crlfRepo(t, 5, 3)
		if out, err := exec.Command("git", "-C", root, "config", "core.autocrlf", value).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		if crlf, text, err := LineEndings(context.Background(), root); err != nil || crlf != 0 || text != 0 {
			t.Fatalf("%s: %d of %d: %v", value, crlf, text, err)
		}
	}
}

// A file with a text or eol attribute is converted by git, so it is not counted.
func TestLineEndingsSkipsFilesWithAnAttribute(t *testing.T) {
	root := crlfRepo(t, 5, 3)
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("f0.txt text eol=lf\nf1.txt text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if crlf, _, err := LineEndings(context.Background(), root); err != nil || crlf != 1 {
		t.Fatalf("%d: %v", crlf, err)
	}
}

// An index git cannot read is an error, not a count.
func TestLineEndingsReportsAnUnreadableIndex(t *testing.T) {
	root := crlfRepo(t, 2, 1)
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LineEndings(context.Background(), root); err == nil {
		t.Fatal("a broken index was counted")
	}
}

// The parser keeps paths with tabs and spaces whole, and counts only LF text in git.
func TestCountLineEndings(t *testing.T) {
	raw := strings.Join([]string{
		"i/lf    w/crlf  attr/                 \ta b\tc.go",
		"i/lf    w/lf    attr/                 \tclean.go",
		"i/lf    w/crlf  attr/text eol=lf      \tconverted.go",
		"i/-text w/-text attr/                 \tfont.woff2",
		"i/crlf  w/crlf  attr/                 \twindows.bat",
		"no tab here",
		"",
	}, "\x00")
	if crlf, text := CountLineEndings([]byte(raw)); crlf != 1 || text != 3 {
		t.Fatalf("%d of %d", crlf, text)
	}
}
