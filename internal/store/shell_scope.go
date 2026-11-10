package store

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ShellScope says what a reduced shell snapshot did not look into (ADR 018). Its status ran
// with --ignore-submodules=dirty, and Submodules are the gitlink paths in the index that status
// read, whose content it did not check. A full snapshot has no scope. A Request carries one only
// on the scoped route.
type ShellScope struct {
	Mode       string   `json:"mode"`
	Submodules []string `json:"submodules"`
}

// maxScopeSubmodules bounds a scope's paths, as maxClaims bounds an Execution's claims.
const maxScopeSubmodules = 500

// shellCapabilities is what a registration answer names: a hook sends a reduced snapshot only
// to a hub that names shell_scope, and only on the scoped route, which an older hub refuses
// (ADR 018 decision 3). Each answer gets its own slice.
func shellCapabilities() []string { return []string{"shell_scope"} }

// validScope checks a reduced snapshot's scope. The mode is the only one a reduced status runs
// with: never all, which hides even a submodule commit staged in the superproject. The paths are
// relative, already clean as git prints them, sorted and unique, so two equal sets have one
// form.
func validScope(s ShellScope) error {
	if s.Mode != "dirty" {
		return fail("bad_request", "a reduced snapshot's mode must be dirty")
	}
	if len(s.Submodules) > maxScopeSubmodules {
		return fail("bad_request", fmt.Sprintf("a reduced snapshot names at most %d submodules", maxScopeSubmodules))
	}
	for i, p := range s.Submodules {
		clean, err := cleanPath(p, "exact", false)
		if err != nil {
			return err
		}
		if clean != p {
			return fail("bad_request", "submodule paths must be clean")
		}
		if i > 0 && s.Submodules[i-1] >= p {
			return fail("bad_request", "submodule paths must be sorted and unique")
		}
	}
	return nil
}

// scopeFits keeps each kind of snapshot on its route. The plain route takes full snapshots
// only, so a reduced one is never read as full by a hub that does not understand scope; the
// scoped route, which such a hub refuses, takes only reduced ones with a valid scope.
func scopeFits(operation string, r Request) error {
	if operation == "reconcile" {
		if r.Scope != nil {
			return fail("bad_request", "a reduced snapshot belongs on the scoped route")
		}
		return nil
	}
	if r.Scope == nil {
		return fail("bad_request", "the scoped route needs a scope")
	}
	return validScope(*r.Scope)
}

// scopeKey is a scope's identity: two snapshots are compared only when their keys are equal,
// the submodule paths included (ADR 018 decision 2). A full snapshot's key is empty.
func scopeKey(s *ShellScope) string {
	if s == nil {
		return ""
	}
	// No submodules can arrive as null or as []; both are the same empty set and one key.
	key := ShellScope{Mode: s.Mode, Submodules: s.Submodules}
	if key.Submodules == nil {
		key.Submodules = []string{}
	}
	raw, _ := json.Marshal(key) // a string and a slice of strings always encode
	return string(raw)
}

// shellChanges compares two snapshots of one scope. changed holds the paths that are new in the
// status or have another code; left those that are no longer in it. In reduced scope a
// submodule path the status does not report is unknown, not clean: it never leaves, so nothing
// is released for it (ADR 018 decision 2).
func shellChanges(previous, current map[string]string, scope *ShellScope) (changed, left []string) {
	changed = []string{}
	for p, status := range current {
		if previous[p] != status {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	unknown := map[string]bool{}
	if scope != nil {
		for _, p := range scope.Submodules {
			unknown[p] = true
		}
	}
	for p := range previous {
		if _, still := current[p]; !still && !unknown[p] {
			left = append(left, p)
		}
	}
	sort.Strings(left)
	return changed, left
}
