package mcpserver

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// fileClaim places an explicit claim's path, which is relative to the session's repository, in the
// checkout that holds it (ADR 033). When the path lies below another checkout, such as a checked-out
// submodule, and the hub takes grouped claims, it answers that checkout, with where it lies in the
// session's worktree as its prefix. The path itself is sent as given: the hub validates it as it
// does any explicit claim's and only then takes the prefix off. A path at the session's own level,
// the submodule's directory itself included, stays with the session's repository, as does one that
// does not stay inside the worktree, and any path an older hub or an older binding cannot place.
//
// A session outside git takes no claim at all, so its claim is refused before the filesystem is
// read, as the hub would refuse it (ADR 034).
func fileClaim(b binding.Bound, raw string) (map[string]any, bool, error) {
	if b.Outside {
		return nil, false, &hubclient.Error{Code: "outside_git", Message: "this session runs outside any git checkout, so it takes no claims; start a session in the checkout to claim its files"}
	}
	if !hubclient.FileClaimsCapable(b.Capabilities.List()) || b.Worktree == "" || strings.ContainsAny(raw, "\\:") {
		return nil, false, nil
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return nil, false, nil
	}
	if info, err := os.Stat(b.Worktree); err != nil || !info.IsDir() {
		// The hub names the first spelling the host gave this worktree. Placing a path from a
		// directory that is not here would claim a submodule's file in the superproject.
		return nil, false, &hubclient.Error{Code: "unavailable", Message: "the session's worktree " + b.Worktree + " is not a directory on this host, so the claim cannot be placed in its checkout; end and restart the session from its checkout"}
	}
	abs := filepath.Join(b.Worktree, filepath.FromSlash(clean))
	own := gitinfo.Repo{Worktree: b.Worktree}
	place := gitinfo.Place(filepath.Dir(abs), own, gitinfo.Find, nil)
	if place.Worktree == own.Worktree {
		return nil, false, nil
	}
	// Place stops at the session's worktree, so a checkout it finds lies inside it.
	prefix, _ := gitinfo.Relative(own.Worktree, filepath.Join(place.Worktree, ".git"))
	checkout := map[string]any{"worktree": place.Worktree, "worktree_id": place.WorktreeID, "common_dir": place.CommonDir, "remote_url": place.RemoteURL, "root_commit": place.RootCommit, "branch": place.Branch, "paths": []string{}, "prefix": strings.TrimSuffix(prefix, "/.git")}
	return checkout, true, nil
}
