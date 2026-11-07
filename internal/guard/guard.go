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

func wrapper(chain bool) []byte {
	s := "#!/bin/sh\n# Iris cooperative guard v1\niris guard check || exit $?\n"
	if chain {
		s += "exec \"$0.iris-original\" \"$@\"\n"
	}
	return []byte(s)
}

// Install preserves any existing pre-commit hook and chains it after a successful check.
func Install(ctx context.Context, dir string) error {
	p, err := hookPath(ctx, dir)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bytes.Equal(old, wrapper(false)) || bytes.Equal(old, wrapper(true)) {
		return nil
	}
	if info, e := os.Lstat(p); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("pre-commit is a symlink; choose a regular hook before installing Iris")
	}
	backup := p + ".iris-original"
	if _, e := os.Lstat(backup); !errors.Is(e, os.ErrNotExist) {
		return errors.New("Iris hook backup already exists; refusing to replace it")
	}
	if err = os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".iris-hook-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(wrapper(old != nil))
	err = errors.Join(err, f.Close(), os.Chmod(tmp, 0o700))
	if err == nil && old != nil {
		err = rename(p, backup)
	}
	if err != nil {
		return err
	}
	if err = rename(tmp, p); err != nil && old != nil {
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
	chain := bytes.Equal(b, wrapper(true))
	if !chain && !bytes.Equal(b, wrapper(false)) {
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
	result, err := hub.CheckPaths(ctx, hubclient.CheckRequest{CommonDir: repo.CommonDir, Host: host, Worktree: repo.Worktree, RemoteURL: repo.RemoteURL, RootCommit: repo.RootCommit, Paths: paths})
	if err != nil {
		return "", err
	}
	return result.Message, nil
}
