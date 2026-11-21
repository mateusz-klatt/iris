package gitinfo

import (
	"os"
	"path/filepath"
)

// Place is the checkout that holds dir: the innermost directory at or above it with a .git entry,
// read by find (ADR 033). Walking up, it stops at known's worktree, which the caller has already
// read. A directory it cannot place, or whose checkout cannot be read, is known's. places, when
// not nil, remembers the answer for every directory the walk passed, so one caller reads each
// directory and each checkout once.
func Place(dir string, known Repo, find func(string) (Repo, error), places map[string]Repo) Repo {
	var passed []string
	place := known
	for d := dir; ; {
		if p, ok := places[d]; ok {
			place = p
			break
		}
		passed = append(passed, d)
		// Relative refuses the root itself, so the root is the directory whose .git is ".git".
		if rel, err := Relative(known.Worktree, filepath.Join(d, ".git")); err == nil && rel == ".git" {
			break
		}
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			if found, err := find(d); err == nil {
				place = found
			}
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if places != nil {
		for _, d := range passed {
			places[d] = place
		}
	}
	return place
}
