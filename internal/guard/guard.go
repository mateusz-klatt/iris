// Package guard implements the cooperative, per-repository Git checkpoint.
package guard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// rename and hostname are replaced in tests. Neither can be made to fail through
// the file system once the temporary hook exists, and the rollback must be proven.
var (
	rename   = os.Rename
	hostname = os.Hostname
)

func hookPath(ctx context.Context, dir string) (string, error) {
	repo, err := gitinfo.Find(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	cmd.Dir = repo.Worktree
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("find Git hooks directory: %w", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "pre-commit"), nil
}

const chainLine = "exec \"$0.iris-original\" \"$@\"\n"

// legacy is the hook earlier versions wrote. It found iris only on PATH, so a commit from a shell
// without it failed with 127 instead of being allowed, as D4 requires.
func legacy(chain bool) []byte {
	s := "#!/bin/sh\n# Iris cooperative guard v1\niris guard check || exit $?\n"
	if chain {
		s += chainLine
	}
	return []byte(s)
}

// wrapper runs the iris recorded at install, then iris from PATH. With neither it says so and
// allows the commit: the guard is cooperative, and a missing binary must not block commits forever.
// A real conflict still refuses. The chained original hook runs either way.
func wrapper(iris string, chain bool) []byte {
	s := "#!/bin/sh\n# Iris cooperative guard v2\n" +
		"iris=" + shellQuote(iris) + "\n" +
		"[ -x \"$iris\" ] || iris=$(command -v iris) || iris=\n" +
		"if [ -n \"$iris\" ]; then\n" +
		"\t\"$iris\" guard check || exit $?\n" +
		"else\n" +
		"\techo \"Iris guard: iris not found; commit not checked\" >&2\n" +
		"fi\n"
	if chain {
		s += chainLine
	}
	return []byte(s)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ours reports whether b is a hook Iris wrote, of either version and for any recorded path, and
// whether it chains an original. Anything else belongs to the user.
func ours(b []byte) (chain, ok bool) {
	for _, c := range []bool{false, true} {
		if bytes.Equal(b, legacy(c)) {
			return c, true
		}
		lines := strings.SplitAfter(string(b), "\n")
		if len(lines) > 2 && strings.HasPrefix(lines[2], "iris=") {
			lines[2] = "iris=''\n"
			if strings.Join(lines, "") == string(wrapper("", c)) {
				return c, true
			}
		}
	}
	return false, false
}

// Install preserves any existing pre-commit hook and chains it after a successful check. iris is
// the path the hook runs first. A hook Iris wrote before, of either version, is rewritten in
// place and keeps its chained original.
func Install(ctx context.Context, dir, iris string) error {
	p, err := hookPath(ctx, dir)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup := p + ".iris-original"
	chain, mine := ours(old)
	if mine && bytes.Equal(old, wrapper(iris, chain)) {
		return nil
	}
	// An Iris hook is replaced in place and keeps its backup. Anything else becomes the backup.
	keep := old != nil && !mine
	if keep {
		chain = true
		if info, e := os.Lstat(p); e == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("pre-commit is a symlink; choose a regular hook before installing Iris")
		}
		if _, e := os.Lstat(backup); !errors.Is(e, os.ErrNotExist) {
			return errors.New("an Iris hook backup already exists; refusing to replace it")
		}
	}
	if err = os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".iris-hook-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.Write(wrapper(iris, chain))
	err = errors.Join(err, f.Close(), os.Chmod(tmp, 0o700))
	if err == nil && keep {
		err = rename(p, backup)
	}
	if err != nil {
		return err
	}
	if err = rename(tmp, p); err != nil && keep {
		_ = rename(backup, p)
	}
	return err
}

// Remove restores the original hook, refusing to discard later user edits.
func Remove(ctx context.Context, dir string) error {
	p, err := hookPath(ctx, dir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	chain, ok := ours(b)
	if !ok {
		return errors.New("pre-commit is not an unchanged Iris hook; refusing to remove it")
	}
	if chain {
		if _, err = os.Stat(p + ".iris-original"); err != nil {
			return err
		}
	}
	if err = os.Remove(p); err != nil {
		return err
	}
	if chain {
		return rename(p+".iris-original", p)
	}
	return nil
}

// Check inspects both ends of staged renames, including deletions and unusual names.
// Hub errors are returned separately so the CLI can warn and allow a commit.
func Check(ctx context.Context, dir string, hub *hubclient.Client) (string, error) {
	repo, err := gitinfo.Find(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "--no-renames", "-z", "--")
	cmd.Dir = repo.Worktree
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	paths := []string{}
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	if len(paths) == 0 {
		return "", nil
	}
	host, err := hostname()
	if err != nil {
		return "", err
	}
	result, err := hub.CheckPaths(ctx, hubclient.CheckRequest{CommonDir: repo.CommonDir, Host: host, Worktree: repo.Worktree, WorktreeID: repo.WorktreeID, RemoteURL: repo.RemoteURL, RootCommit: repo.RootCommit, Paths: paths})
	if err != nil {
		return "", err
	}
	return result.Message, nil
}
