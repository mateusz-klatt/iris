package hook

import (
	"context"
	"os"
	"path/filepath"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// fileTouchHub takes a grouped touch (ADR 033). The hub client has it. A hub is sent one only when
// its registration answer names file_claims.
type fileTouchHub interface {
	TouchFiles(context.Context, hubclient.TouchFilesRequest) (hubclient.TouchResponse, error)
}

// fileGroups groups an edit's paths by the checkout each file lies in: the innermost directory at
// or above the file that has a .git entry, so a submodule's file is its own repository's, wherever
// the session was started (ADR 033). repo is the event's checkout, which is not read again. A path
// that cannot be placed belongs to repo, as it did before, when it lies inside it; otherwise it is
// left out, as a path outside git takes no claim (D3, rule 5).
func (r Runner) fileGroups(repo gitinfo.Repo, ev Event) []hubclient.ClaimGroup {
	raw := append([]string{}, ev.Paths...)
	for _, p := range []string{ev.RenameFrom, ev.RenameTo} {
		if p != "" {
			raw = append(raw, p)
		}
	}
	var groups []hubclient.ClaimGroup
	index := map[string]int{}
	seen := map[string]bool{}
	places := map[string]gitinfo.Repo{}
	for _, p := range raw {
		abs := p
		if !isAbsPath(abs) {
			base := ev.CWD
			if base == "" {
				base = repo.Worktree
			}
			abs = joinPath(base, abs)
		}
		place := r.placeOf(filepath.Dir(abs), repo, places)
		rel, err := gitinfo.Relative(place.Worktree, abs)
		if err != nil || rel == "" {
			continue
		}
		i, found := index[place.GitDir]
		if !found {
			i = len(groups)
			index[place.GitDir] = i
			groups = append(groups, hubclient.ClaimGroup{Worktree: place.Worktree, WorktreeID: place.WorktreeID, CommonDir: place.CommonDir, RemoteURL: place.RemoteURL, RootCommit: place.RootCommit})
		}
		if seen[place.GitDir+"\x00"+rel] {
			continue
		}
		seen[place.GitDir+"\x00"+rel] = true
		groups[i].Paths = append(groups[i].Paths, rel)
	}
	return groups
}

// placeOf is the checkout that holds dir. Walking up, it stops at repo's worktree, which it
// already knows, or at the first directory with a .git entry, which it reads as the hook reads
// any checkout (D11). A directory it cannot place, or whose checkout cannot be read, is repo's.
// places remembers the answer for every directory the walk passed, so one call reads each
// directory and each checkout once, however many of its paths share them.
func (r Runner) placeOf(dir string, repo gitinfo.Repo, places map[string]gitinfo.Repo) gitinfo.Repo {
	find := r.Find
	if find == nil {
		find = gitinfo.Find
	}
	var passed []string
	place := repo
	for d := dir; ; {
		if known, ok := places[d]; ok {
			place = known
			break
		}
		passed = append(passed, d)
		// Relative refuses the root itself, so the root is the directory whose .git is ".git".
		if rel, err := gitinfo.Relative(repo.Worktree, filepath.Join(d, ".git")); err == nil && rel == ".git" {
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
	for _, d := range passed {
		places[d] = place
	}
	return place
}

// reconcileRequest names the checkout a shell status was taken in, so a hub that names
// file_claims attributes nothing from one that is not the Execution's (ADR 033).
func reconcileRequest(execution string, generation int, phase string, status map[string]string, repo gitinfo.Repo) hubclient.ReconcileRequest {
	return hubclient.ReconcileRequest{ExecutionID: execution, Generation: generation, Kind: phase, Status: status, Worktree: repo.Worktree, WorktreeID: repo.WorktreeID, RemoteURL: repo.RemoteURL, RootCommit: repo.RootCommit}
}
