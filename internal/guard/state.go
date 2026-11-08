package guard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

// marker is the line every Iris pre-commit hook carries, chained or not.
const marker = "# Iris cooperative guard"

// HintSetting is the git config key that hides the session hint for one
// repository: `git config iris.guardHint false`.
const HintSetting = "iris.guardHint"

// Hint is the line a session is shown when its repository has no guard.
const Hint = "This repository has no Iris pre-commit guard, so a shell edit past a claim reaches a commit unchecked. Suggest it to the user: `iris guard install` adds it, and `git config " + HintSetting + " false` hides this note.\n"

// NeedsHint reports whether a session in repo should be told that the guard
// is missing. It runs inside a hook's deadline, so it starts no git process
// and reads only the repository's config and one hook file. A core.hooksPath
// set only in the user's global config is not seen, and the hint can then be
// shown although a guard exists; the setting above hides it.
func NeedsHint(repo gitinfo.Repo) bool {
	if v, ok := repo.ConfigValue("iris", "guardHint"); ok && isFalse(v) {
		return false
	}
	dir := filepath.Join(repo.CommonDir, "hooks")
	if v, ok := repo.ConfigValue("core", "hooksPath"); ok && v != "" {
		dir = hooksDir(repo.Worktree, v)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pre-commit"))
	return err != nil || !bytes.Contains(b, []byte(marker))
}

// hooksDir resolves core.hooksPath as git does for a worktree: a relative
// path is taken from the worktree root, and ~/ from the home directory.
func hooksDir(worktree, v string) string {
	if rest, ok := strings.CutPrefix(v, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(worktree, v)
}

func isFalse(v string) bool {
	switch strings.ToLower(v) {
	case "false", "no", "off", "0", "":
		return true
	}
	return false
}

// Guard states reported by Status.
const (
	StateInstalled = "installed"
	StateMissing   = "not installed"
	StateOther     = "another pre-commit hook, without Iris"
)

// Status is the guard's state for the repository containing dir, with the
// hook path git resolves. It runs git, so it is for commands, not hooks.
func Status(ctx context.Context, dir string) (state, path string, err error) {
	path, err = hookPath(ctx, dir)
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StateMissing, path, nil
	case err != nil:
		return "", path, err
	case bytes.Contains(b, []byte(marker)):
		return StateInstalled, path, nil
	}
	return StateOther, path, nil
}
