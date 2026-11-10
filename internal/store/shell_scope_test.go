package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func scoped(paths ...string) *ShellScope { return &ShellScope{Mode: "dirty", Submodules: paths} }

// A reduced snapshot's scope has one form: mode dirty, and gitlink paths as git prints them,
// sorted and unique. Anything else is refused before it can be compared with another.
func TestAReducedScopeHasOneForm(t *testing.T) {
	many := make([]string, maxScopeSubmodules+1)
	for i := range many {
		many[i] = fmt.Sprintf("modules/%04d", i)
	}
	for name, scope := range map[string]ShellScope{
		"mode all hides staged gitlinks": {Mode: "all"},
		"mode untracked is full scope":   {Mode: "untracked"},
		"no mode":                        {},
		"too many paths":                 {Mode: "dirty", Submodules: many},
		"an absolute path":               *scoped("/modules/one"),
		"a parent traversal":             *scoped("../one"),
		"a glob":                         *scoped("modules/*"),
		"an empty path":                  *scoped(""),
		"a trailing slash":               *scoped("modules/one/"),
		"a dot segment":                  *scoped("./modules/one"),
		"unsorted":                       *scoped("modules/two", "modules/one"),
		"a duplicate":                    *scoped("modules/one", "modules/one"),
	} {
		if err := validScope(scope); !isCode(err, "bad_request") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, scope := range map[string]ShellScope{
		"no submodules":    *scoped(),
		"sorted and clean": *scoped("lib", "modules/one", "modules/two"),
		"the limit":        {Mode: "dirty", Submodules: many[:maxScopeSubmodules]},
	} {
		if err := validScope(scope); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Each kind of snapshot has its route. The plain route never takes a reduced one, so no hub
// reads one as full; the scoped route takes nothing else.
func TestEachSnapshotKindHasItsRoute(t *testing.T) {
	s, _, _ := testStore(t)
	shell, _ := session(t, s, 1)
	shell.Kind, shell.Status = "before", map[string]string{}
	shell.Scope = scoped("modules/one")
	expectError(t, s, "reconcile", shell, "bad_request")
	shell.Scope = nil
	expectError(t, s, "reconcile_scoped", shell, "bad_request")
	shell.Scope = &ShellScope{Mode: "all"}
	expectError(t, s, "reconcile_scoped", shell, "bad_request")
	// The session form of the scoped route is refused the same way.
	register := Request{Client: "codex", Host: "host", Cwd: "/repo/w9", Worktree: "/repo/w9", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "9", Source: "hook", Kind: "before", Status: map[string]string{}}
	expectError(t, s, "reconcile_scoped", register, "bad_request")
	if !shellSession("reconcile_scoped", register) || shellSession("reconcile_scoped", shell) || shellSession("sessions", register) {
		t.Fatal("the scoped route's session form is not recognised as the plain route's is")
	}
}

// Scopes compare equal only with the same mode and the same paths; a full snapshot has none.
func TestAScopeIsItsModeAndItsPaths(t *testing.T) {
	if scopeKey(nil) != "" {
		t.Fatal("a full snapshot has a scope key")
	}
	// Two scopes built apart, with their paths in different slices, are one scope.
	paths := []string{"a"}
	if first, second := scoped("a", "b"), scoped(append(paths, "b")...); scopeKey(first) != scopeKey(second) {
		t.Fatal("equal scopes differ")
	}
	for _, other := range []*ShellScope{scoped("a"), scoped("a", "c"), scoped()} {
		if scopeKey(other) == scopeKey(scoped("a", "b")) {
			t.Fatalf("%v compares equal to a, b", other.Submodules)
		}
	}
	// No submodules, sent as null or as [], is one set.
	if scopeKey(&ShellScope{Mode: "dirty"}) != scopeKey(&ShellScope{Mode: "dirty", Submodules: []string{}}) {
		t.Fatal("an empty set has two keys")
	}
}

// In reduced scope a submodule path the status does not report is unknown: it does not leave,
// so nothing would be released for it. A reported one compares as in full scope, and so does
// every other path.
func TestAnUnreportedSubmoduleNeverLeaves(t *testing.T) {
	previous := map[string]string{"modules/one": " M", "modules/two": " M", "top.txt": " M", "gone.txt": "??"}
	current := map[string]string{"modules/two": "M ", "top.txt": " M", "new.txt": "??"}
	changed, left := shellChanges(previous, current, scoped("modules/one", "modules/two"))
	if !reflect.DeepEqual(changed, []string{"modules/two", "new.txt"}) || !reflect.DeepEqual(left, []string{"gone.txt"}) {
		t.Fatalf("reduced: changed %v, left %v", changed, left)
	}
	changed, left = shellChanges(previous, current, nil)
	if !reflect.DeepEqual(changed, []string{"modules/two", "new.txt"}) || !reflect.DeepEqual(left, []string{"gone.txt", "modules/one"}) {
		t.Fatalf("full: changed %v, left %v", changed, left)
	}
	if changed, left = shellChanges(map[string]string{}, map[string]string{}, nil); changed == nil || len(changed) != 0 || left != nil {
		t.Fatalf("no change: changed %#v, left %#v", changed, left)
	}
}

// A hook learns from every registration answer whether it may send a reduced snapshot here.
func TestRegistrationAnswersNameTheShellScope(t *testing.T) {
	s, _, _ := testStore(t)
	_, out := session(t, s, 1)
	if !reflect.DeepEqual(out["capabilities"], []string{"shell_scope"}) {
		t.Fatalf("session answer: %v", out["capabilities"])
	}
	register := Request{Client: "codex", Host: "host", Cwd: "/repo/w2", Worktree: "/repo/w2", RemoteURL: "https://example.com/team/project.git", RootCommit: "root", SessionID: "2", Source: "hook", Kind: "before", Status: map[string]string{}}
	answer := call(t, s, "reconcile", register)["session"].(map[string]any)
	if !reflect.DeepEqual(answer["capabilities"], []string{"shell_scope"}) {
		t.Fatalf("shell answer: %v", answer["capabilities"])
	}
	// Each answer has its own slice, so changing one cannot change the next.
	answer["capabilities"].([]string)[0] = "changed"
	if _, again := session(t, s, 3); again["capabilities"].([]string)[0] != "shell_scope" {
		t.Fatal("answers share their capability list")
	}
}
