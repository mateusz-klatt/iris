package gitinfo

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func gitDirWith(t *testing.T, head string) (root, git string) {
	t.Helper()
	root = t.TempDir()
	git = filepath.Join(root, ".git")
	writeFile(t, filepath.Join(git, "HEAD"), head)
	return root, git
}

func TestFindRejectsAnEmptyStart(t *testing.T) {
	if _, err := Find(""); !errors.Is(err, ErrNotRepo) {
		t.Fatal(err)
	}
}

func TestFindRejectsADeletedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to delete the working directory")
	}
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := Find("relative"); !errors.Is(err, ErrNotRepo) {
		t.Fatal(err)
	}
}

func TestGitFileThatIsNotALink(t *testing.T) {
	for name, body := range map[string]string{
		"no prefix": "something else\n",
		"no target": "gitdir:   \n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".git"), body)
			if _, err := Find(root); !errors.Is(err, ErrNotRepo) {
				t.Fatal(err)
			}
		})
	}
}

func TestGitFileThatCannotBeRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), ".git")
	if _, err := readGitdirFile(missing); !errors.Is(err, ErrNotRepo) {
		t.Fatal(err)
	}
}

func TestGitFileWithARelativeTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git"), "gitdir: elsewhere/repo\n")
	writeFile(t, filepath.Join(root, "elsewhere", "repo", "HEAD"), "ref: refs/heads/main\n")
	repo, err := Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "elsewhere", "repo"); repo.GitDir != want {
		t.Fatalf("%q want %q", repo.GitDir, want)
	}
}

func TestHEADStates(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Branch != "" || repo.RootCommit != "" {
		t.Fatalf("a missing HEAD named something: %+v", repo)
	}

	root, git := gitDirWith(t, shaA+"\n")
	writeLoose(t, git, shaA, "tree x\n\nroot\n")
	repo, err = Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Branch != "" || repo.RootCommit != shaA {
		t.Fatalf("detached HEAD: %+v", repo)
	}

	root, _ = gitDirWith(t, "not a commit\n")
	repo, err = Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Branch != "" || repo.RootCommit != "" {
		t.Fatalf("garbage HEAD: %+v", repo)
	}
}

func TestUnresolvableBranchHasNoRoot(t *testing.T) {
	root, _ := gitDirWith(t, "ref: refs/heads/none\n")
	repo, err := Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Branch != "none" || repo.RootCommit != "" {
		t.Fatalf("%+v", repo)
	}
}

func TestPackedRefSkipsWhatIsNotTheRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packed-refs")
	writeFile(t, path, "# pack-refs\n"+
		"nospace\n"+
		shaB+" refs/heads/other\n"+
		"short refs/heads/main\n"+
		"^"+shaB+"\n"+
		shaA+" refs/heads/main\n")
	if got := packedRef(path, "refs/heads/main"); got != shaA {
		t.Fatalf("%q", got)
	}
	if got := packedRef(path, "refs/heads/absent"); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestRemoteChoice(t *testing.T) {
	root, git := gitDirWith(t, "ref: refs/heads/main\n")
	if repo, err := Find(root); err != nil || repo.RemoteURL != "" {
		t.Fatalf("no config: %+v %v", repo, err)
	}
	writeFile(t, filepath.Join(git, "config"), `[core]
	url = not-a-remote
[remote "upstream"]
	url = "https://example.test/upstream"
[remote "fork"]
	url = https://example.test/fork
`)
	repo, err := Find(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.RemoteURL != "https://example.test/upstream" {
		t.Fatalf("no origin should mean the first remote: %q", repo.RemoteURL)
	}
}

func TestRootWalkStopsOnLongHistory(t *testing.T) {
	_, git := gitDirWith(t, "x")
	// A commit that is its own parent never reaches a root, so the walk must give up.
	writeLoose(t, git, shaA, "tree x\nparent "+shaA+"\n\nloop\n")
	if got := rootCommit(git, shaA); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestLooseObjectsThatAreNotCheapCommits(t *testing.T) {
	_, git := gitDirWith(t, "x")
	if _, ok := looseParents(git, "ab"); ok {
		t.Fatal("short id accepted")
	}
	if _, ok := looseParents(git, shaA); ok {
		t.Fatal("missing object accepted")
	}
	object := filepath.Join(git, "objects", shaB[:2], shaB[2:])
	writeFile(t, object, "not zlib")
	if _, ok := looseParents(git, shaB); ok {
		t.Fatal("garbage accepted")
	}
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte("commit 1\x00body that is long enough to cut")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, object, buf.String()[:buf.Len()-6])
	if _, ok := looseParents(git, shaB); ok {
		t.Fatal("truncated stream accepted")
	}
	writeLoose(t, git, shaB, "tree x\n\nblob\n")
	object = filepath.Join(git, "objects", shaA[:2], shaA[2:])
	buf.Reset()
	w = zlib.NewWriter(&buf)
	if _, err := w.Write([]byte("blob 1\x00x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, object, buf.String())
	if _, ok := looseParents(git, shaA); ok {
		t.Fatal("a blob is not a commit")
	}
}

func TestIsSHA(t *testing.T) {
	for s, want := range map[string]bool{
		shaA:                    true,
		strings.ToUpper(shaA):   true,
		strings.Repeat("0", 64): true,
		"abc":                   false,
		strings.Repeat("g", 40): false,
	} {
		if got := isSHA(s); got != want {
			t.Errorf("isSHA(%q) = %v", s, got)
		}
	}
}

func TestRelativeRefusals(t *testing.T) {
	cases := []struct{ root, path string }{
		{"relative/root", "/a"},
		{"/a/b/c", "/a"},
		{"/a/b", "/a/x/y"},
	}
	for _, tc := range cases {
		if got, err := Relative(tc.root, tc.path); !errors.Is(err, ErrEscape) {
			t.Errorf("Relative(%q, %q) = %q, %v", tc.root, tc.path, got, err)
		}
	}
	got, err := Relative("/", "/../a")
	if err != nil || got != "a" {
		t.Fatalf("a parent of the volume root stays there: %q %v", got, err)
	}
}

func TestStatusReadsARealRepository(t *testing.T) {
	dir := t.TempDir()
	run := exec.Command("git", "init")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	writeFile(t, filepath.Join(dir, "with space.txt"), "x")
	got, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got["with space.txt"] != "??" {
		t.Fatalf("%v", got)
	}
}

func TestStatusReportsGitFailure(t *testing.T) {
	if _, err := Status(context.Background(), filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("no error")
	}
}

func TestStatusStopsAtItsLimit(t *testing.T) {
	var out limitedStatus
	if n, err := out.Write(make([]byte, statusLimit)); err != nil || n != statusLimit {
		t.Fatal(n, err)
	}
	if _, err := out.Write([]byte{0}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatal(err)
	}
	if out.kept.Len() != statusLimit {
		t.Fatalf("kept %s bytes", strconv.Itoa(out.kept.Len()))
	}
}

// os/exec copies git's output with io.Copy, and io.Copy gives the whole copy to a writer that
// has ReadFrom. An embedded bytes.Buffer has one, and it read git's output to the end without
// a single Write, so the limit never applied. The source here ends after 1 MiB, so a limit
// that fails again costs this test a megabyte and no more.
func TestTheStatusLimitHoldsForTheCopyExecMakes(t *testing.T) {
	var out limitedStatus
	copied, err := io.Copy(&out, io.LimitReader(repeating('a'), 1<<20))
	if err == nil || copied > statusLimit || out.kept.Len() > statusLimit {
		t.Fatalf("copied %d bytes and kept %d: %v", copied, out.kept.Len(), err)
	}
}

type repeating byte

func (r repeating) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// A status above the limit is refused, not accepted whole. The first call is the control: the
// same worktree is read while its status is below the limit.
func TestStatusRefusesAWorktreeAboveItsLimit(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", root)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Each untracked file is one record of 108 bytes: "?? ", a 104-byte name and a NUL.
	untracked := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			name := strings.Repeat("n", 100) + strconv.Itoa(1000+i)
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	untracked(0, 100)
	got, err := Status(context.Background(), root)
	if err != nil || len(got) != 100 {
		t.Fatalf("below the limit: %d paths, %v", len(got), err)
	}
	untracked(100, 600)
	if got, err = Status(context.Background(), root); err == nil {
		t.Fatalf("accepted %d paths, %d bytes of status, above the limit of %d", len(got), 600*108, statusLimit)
	}
}

func TestParseStatusRefusesAMalformedRecord(t *testing.T) {
	for _, raw := range []string{"M\x00", "XYZa\x00"} {
		if _, err := ParseStatus([]byte(raw)); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}
