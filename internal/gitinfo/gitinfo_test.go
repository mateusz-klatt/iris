package gitinfo

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"testing"
)

func TestRelative(t *testing.T) {
	root := "/workspace/project"
	cases := []struct {
		path string
		want string
		err  bool
	}{
		{path: "/workspace/project/src/a.go", want: "src/a.go"},
		{path: "/workspace/project/src/../a.go", want: "a.go"},
		{path: "src/a.go", want: "src/a.go"},
		{path: "src/../b.go", want: "b.go"},
		{path: "/workspace/project/../secret", err: true},
		{path: "../secret", err: true},
		{path: "foo/../../etc", err: true},
		{path: "/other/a", err: true},
		{path: "/workspace/project", err: true},
		{path: "/workspace/project2/a", err: true},
	}
	for _, tc := range cases {
		got, err := Relative(root, tc.path)
		if tc.err {
			if err == nil {
				t.Errorf("Relative(%q) = %q, want error", tc.path, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Relative(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}

	winRoot := `C:/repo`
	winCases := []struct {
		path string
		want string
		err  bool
	}{
		{path: `C:\repo\src\a.go`, want: "src/a.go"},
		{path: `c:/repo/src/a.go`, want: "src/a.go"},
		{path: `C:/repo/../win`, err: true},
		{path: `D:/repo/a.go`, err: true},
		{path: `src\a.go`, want: "src/a.go"},
	}
	for _, tc := range winCases {
		got, err := Relative(winRoot, tc.path)
		if tc.err {
			if err == nil {
				t.Errorf("Relative win %q = %q, want error", tc.path, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Relative win %q = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
}

func TestFindNormalRepo(t *testing.T) {
	root := t.TempDir()
	git := filepath.Join(root, ".git")
	writeFile(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
	child := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rootSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeFile(t, filepath.Join(git, "refs", "heads", "main"), child+"\n")
	writeLoose(t, git, rootSHA, "tree 0123456789abcdef0123456789abcdef01234567\n\nroot\n")
	writeLoose(t, git, child, "tree 0123456789abcdef0123456789abcdef01234567\nparent "+rootSHA+"\n\nchild\n")
	writeFile(t, filepath.Join(git, "config"), `
[core]
	repositoryformatversion = 0
[remote "upstream"]
	url = https://example.com/other.git
[remote "origin"]
	url = "https://example.com/project.git"
`)
	sub := filepath.Join(root, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := Find(sub)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Worktree != root || repo.GitDir != git || repo.CommonDir != git {
		t.Fatalf("dirs = %+v", repo)
	}
	if repo.Branch != "main" || repo.RemoteURL != "https://example.com/project.git" || repo.RootCommit != rootSHA {
		t.Fatalf("identity = %+v", repo)
	}
}

func TestFindLinkedWorktree(t *testing.T) {
	main := t.TempDir()
	common := filepath.Join(main, ".git")
	wtGit := filepath.Join(common, "worktrees", "feature")
	writeFile(t, filepath.Join(common, "config"), "[remote \"origin\"]\n\turl = git@github.com:example/project.git\n")
	writeFile(t, filepath.Join(common, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/feature\n")
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n")
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	writeFile(t, filepath.Join(common, "packed-refs"), "# pack-refs with: peeled\n"+sha+" refs/heads/feature\n")

	wt := filepath.Join(t.TempDir(), "feature")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")

	repo, err := Find(filepath.Join(wt, ".agents"))
	if err != nil {
		// .agents does not exist; Find still walks from the missing leaf's parent
		// only if the leaf's directory exists. Create it.
		t.Fatal(err)
	}
	if repo.Worktree != wt {
		t.Fatalf("worktree %s", repo.Worktree)
	}
	if repo.GitDir != wtGit || repo.CommonDir != common {
		t.Fatalf("git %s common %s", repo.GitDir, repo.CommonDir)
	}
	if repo.Branch != "feature" || repo.RemoteURL != "git@github.com:example/project.git" {
		t.Fatalf("identity %+v", repo)
	}
	if repo.RootCommit != "" {
		t.Fatalf("packed object is not a cheap root, got %s", repo.RootCommit)
	}
}

func TestFindNotRepo(t *testing.T) {
	if _, err := Find(t.TempDir()); err != ErrNotRepo {
		t.Fatalf("err = %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLoose(t *testing.T, gitDir, sha, payload string) {
	t.Helper()
	header := "commit " + itoa(len(payload)) + "\x00" + payload
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte(header)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gitDir, "objects", sha[:2], sha[2:]), buf.String())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
