package gitinfo

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const object40 = "0123456789abcdef0123456789abcdef01234567"

func record(mode, stage, path string) string {
	return mode + " " + object40 + " " + stage + "\t" + path + "\x00"
}

// listing feeds a ls-files listing to the parser in the chunks given.
func listing(chunks ...string) ([]string, error) {
	var l gitlinkListing
	for _, c := range chunks {
		if _, err := l.Write([]byte(c)); err != nil {
			return nil, err
		}
	}
	return l.paths()
}

// The set has every gitlink at any stage once, in byte order, whatever the paths hold and
// however git's output is cut into writes. Other entries are left out.
func TestTheSetHasEachGitlinkOnceInByteOrder(t *testing.T) {
	stream := record("100644", "0", "a file") + record("100755", "0", "a script") + record("120000", "0", "a link") +
		record("160000", "0", "modules/b") +
		record("160000", "1", "Z") + record("160000", "2", "Z") + record("160000", "3", "Z") +
		record("160000", "2", "mixed") + record("100644", "3", "mixed") +
		record("160000", "0", "tab\tin name") +
		"160000 " + strings.Repeat("ab", 32) + " 0\tsha256\x00"
	want := []string{"Z", "mixed", "modules/b", "sha256", "tab\tin name"}
	if got, err := listing(stream); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("whole: %q %v", got, err)
	}
	var bytewise []string
	for _, c := range stream {
		bytewise = append(bytewise, string(c))
	}
	if got, err := listing(bytewise...); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("byte by byte: %q %v", got, err)
	}
	if got, err := listing(record("100644", "0", "only a file")); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no gitlinks is a known empty set: %#v %v", got, err)
	}
}

// A listing the parser cannot read whole is an error, never a smaller set.
func TestTheSetIsUnknownWhenTheListingIsNotWhole(t *testing.T) {
	many := ""
	for i := range GitlinkLimit + 1 {
		many += record("160000", "0", "m/"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+strings.Repeat("y", i/26))
	}
	for name, stream := range map[string]string{
		"a short mode":                   "16000 " + object40 + " 0\tp\x00",
		"a mode not octal":               "160008 " + object40 + " 0\tp\x00",
		"an octal mode git never writes": "160001 " + object40 + " 0\tp\x00",
		"a sparse directory":             record("040000", "0", "dir/"),
		"a short object":                 "160000 " + object40[1:] + " 0\tp\x00",
		"an upper object":                "160000 " + strings.ToUpper(object40) + " 0\tp\x00",
		"a fourth stage":                 record("160000", "4", "p"),
		"a stage of two digits":          record("160000", "00", "p"),
		"no tab":                         "160000 " + object40 + " 0 p\x00",
		"no path":                        record("160000", "0", ""),
		"an empty record":                "\x00",
		"two fields":                     "160000 " + object40 + "\tp\x00",
		"no final nul":                   strings.TrimSuffix(record("160000", "0", "p"), "\x00"),
		"a record too long":              record("160000", "0", strings.Repeat("p", gitlinkRecordLimit)),
		"too many gitlinks":              many,
	} {
		if got, err := listing(stream); err == nil {
			t.Errorf("%s: %q", name, got)
		}
	}
	full := gitlinkListing{total: gitlinkOutputLimit}
	if _, err := full.Write([]byte{'1'}); !errors.Is(err, errGitlinkLimit) {
		t.Fatalf("past the output limit: %v", err)
	}
}

// A git that fails gives no set: outside a repository, ls-files exits with an error.
func TestAFailingGitGivesNoSet(t *testing.T) {
	if got, err := Gitlinks(context.Background(), t.TempDir()); err == nil {
		t.Fatalf("outside a repository: %v", got)
	}
}

func gitlinks(t *testing.T, root string) []string {
	t.Helper()
	got, err := Gitlinks(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The set comes from the index, so a submodule whose declaration in .gitmodules is gone, or
// names another path, is in it all the same (ADR 018).
func TestTheSetIsTheIndexNotGitmodules(t *testing.T) {
	super := superproject(t)
	writeFile(t, filepath.Join(super, ".gitmodules"), "[submodule \"lib\"]\n\tpath = wrong\n\turl = ../sub\n")
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"lib"}) {
		t.Fatalf("a wrong path: %v", got)
	}
	gitIn(t, super, "rm", "-qf", ".gitmodules")
	gitIn(t, super, "commit", "-qm", "no declaration")
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"lib"}) {
		t.Fatalf("no .gitmodules: %v", got)
	}
}

// The index's states that the research names: a staged gitlink that nothing declares, a
// removed one, a nested one, a linked worktree's own index, and a split index.
func TestTheSetFollowsTheIndexInEachState(t *testing.T) {
	super := superproject(t)
	head := strings.TrimSpace(gitIn(t, filepath.Join(super, "lib"), "rev-parse", "HEAD"))
	gitIn(t, super, "update-index", "--add", "--cacheinfo", "160000,"+head+",undeclared")
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"lib", "undeclared"}) {
		t.Fatalf("staged, undeclared: %v", got)
	}
	gitIn(t, super, "update-index", "--split-index")
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"lib", "undeclared"}) {
		t.Fatalf("split index: %v", got)
	}
	gitIn(t, super, "update-index", "--no-split-index")
	gitIn(t, super, "update-index", "--force-remove", "undeclared")
	linked := filepath.Join(filepath.Dir(super), "linked")
	gitIn(t, super, "worktree", "add", "-q", linked)
	if got := gitlinks(t, linked); !reflect.DeepEqual(got, []string{"lib"}) {
		t.Fatalf("linked worktree: %v", got)
	}
	inner := filepath.Join(filepath.Dir(super), "sub")
	gitIn(t, filepath.Join(super, "lib"), "submodule", "-q", "add", inner, "nested")
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"lib"}) {
		t.Fatalf("a nested gitlink belongs to its own submodule: %v", got)
	}
	gitIn(t, super, "update-index", "--force-remove", "lib")
	if got := gitlinks(t, super); len(got) != 0 {
		t.Fatalf("removed: %v", got)
	}
}

// A conflict on a submodule's commit leaves the gitlink at stages 1 to 3 and none at 0; it is in
// the set once.
func TestAConflictedGitlinkIsInTheSet(t *testing.T) {
	super := superproject(t)
	lib := filepath.Join(super, "lib")
	gitIn(t, super, "checkout", "-qb", "other")
	gitIn(t, lib, "commit", "-q", "--allow-empty", "-m", "theirs")
	gitIn(t, super, "commit", "-qam", "lib moves")
	gitIn(t, super, "checkout", "-q", "-")
	gitIn(t, lib, "checkout", "-q", "HEAD~1")
	gitIn(t, lib, "commit", "-q", "--allow-empty", "-m", "ours")
	gitIn(t, super, "commit", "-qam", "lib moves too")
	cmd := []string{"merge", "-q", "other"}
	if out, err := runGit(super, cmd...); err == nil {
		t.Fatalf("the merge did not conflict: %s", out)
	}
	if stages := gitIn(t, super, "ls-files", "--stage", "lib"); strings.Count(stages, "160000") != 3 {
		t.Fatalf("not conflicted at three stages: %s", stages)
	}
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"lib"}) {
		t.Fatalf("conflict: %v", got)
	}
}

// A sparse index keeps a plain directory outside the cone as one entry, and git keeps a
// directory that holds a gitlink explicit. The listing is expanded either way, so the gitlinks
// inside and outside the cone are all in the set.
func TestASparseIndexIsListedExpanded(t *testing.T) {
	super := superproject(t)
	head := strings.TrimSpace(gitIn(t, filepath.Join(super, "lib"), "rev-parse", "HEAD"))
	for _, p := range []string{"keep/mod", "drop/mod"} {
		gitIn(t, super, "update-index", "--add", "--cacheinfo", "160000,"+head+","+p)
	}
	writeFile(t, filepath.Join(super, "cold", "deep", "file"), "cold\n")
	gitIn(t, super, "add", "cold")
	gitIn(t, super, "commit", "-qm", "two more and a plain tree")
	gitIn(t, super, "sparse-checkout", "set", "--cone", "--sparse-index", "keep")
	if sparse := gitIn(t, super, "ls-files", "--stage", "--sparse"); !strings.Contains(sparse, "040000") {
		t.Fatalf("this git kept no sparse directory entry, so the case is not exercised: %s", sparse)
	}
	if got := gitlinks(t, super); !reflect.DeepEqual(got, []string{"drop/mod", "keep/mod", "lib"}) {
		t.Fatalf("sparse index: %v", got)
	}
}
