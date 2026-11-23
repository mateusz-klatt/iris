package hook

import (
	"context"
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

// placeOf is the checkout that holds dir, as gitinfo.Place finds it, read with the runner's finder.
func (r Runner) placeOf(dir string, repo gitinfo.Repo, places map[string]gitinfo.Repo) gitinfo.Repo {
	find := r.Find
	if find == nil {
		find = gitinfo.Find
	}
	return gitinfo.Place(dir, repo, find, places)
}

// reconcileRequest names the checkout a shell status was taken in, so a hub that names
// file_claims attributes nothing from one that is not the Execution's (ADR 033).
func reconcileRequest(execution string, generation int, phase string, status map[string]string, repo gitinfo.Repo) hubclient.ReconcileRequest {
	return hubclient.ReconcileRequest{ExecutionID: execution, Generation: generation, Kind: phase, Status: status, Worktree: repo.Worktree, WorktreeID: repo.WorktreeID, RemoteURL: repo.RemoteURL, RootCommit: repo.RootCommit}
}
