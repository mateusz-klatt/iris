package hook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// withSubmodule is a superproject with a checked-out submodule at frontend, whose .git is a file
// naming its git directory under the superproject's modules, as git writes it.
func withSubmodule(t *testing.T) (root, modules string) {
	t.Helper()
	root = gitRepo(t)
	modules = filepath.Join(root, ".git", "modules", "frontend")
	for _, dir := range []string{filepath.Join(modules, "refs", "heads"), filepath.Join(root, "frontend", "src")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(modules, "HEAD"):                  "ref: refs/heads/main\n",
		filepath.Join(modules, "refs", "heads", "main"): "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		filepath.Join(modules, "config"):                "[remote \"origin\"]\n\turl = https://example.com/frontend.git\n",
		filepath.Join(root, "frontend", ".git"):         "gitdir: ../.git/modules/frontend\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, modules
}

// Each path of an edit is grouped with the checkout it lies in: the superproject, its submodule
// or another project. A path in no checkout is left out, and one named twice is sent once.
func TestFileGroupsPlaceEachPathInItsOwnCheckout(t *testing.T) {
	root, modules := withSubmodule(t)
	other := gitRepo(t)
	repo, err := gitinfo.Find(root)
	if err != nil {
		t.Fatal(err)
	}
	ev := Event{CWD: root, Paths: []string{filepath.Join(root, "README.md"), "frontend/src/x.ts", filepath.Join(other, "src", "main.go"), filepath.Join(t.TempDir(), "loose.txt"), "README.md"}, RenameFrom: "frontend/src/old.ts", RenameTo: "frontend/src/new.ts"}
	groups := Runner{}.fileGroups(repo, ev)
	if len(groups) != 3 {
		t.Fatalf("groups %+v", groups)
	}
	if groups[0].Worktree != root || !reflect.DeepEqual(groups[0].Paths, []string{"README.md"}) {
		t.Fatalf("superproject group %+v", groups[0])
	}
	sub := groups[1]
	if sub.Worktree != filepath.Join(root, "frontend") || sub.CommonDir != modules || sub.RemoteURL != "https://example.com/frontend.git" || !reflect.DeepEqual(sub.Paths, []string{"src/x.ts", "src/old.ts", "src/new.ts"}) {
		t.Fatalf("submodule group %+v", sub)
	}
	if groups[2].Worktree != other || !reflect.DeepEqual(groups[2].Paths, []string{"src/main.go"}) {
		t.Fatalf("other project group %+v", groups[2])
	}
	// Relative paths go from the worktree when the event names no working directory.
	if got := (Runner{}).fileGroups(repo, Event{Paths: []string{"frontend/src/x.ts"}}); len(got) != 1 || got[0].Paths[0] != "src/x.ts" {
		t.Fatalf("without a working directory: %+v", got)
	}
}

// A nested .git that cannot be read leaves its paths with the event's checkout, as before, so a
// broken submodule never loses the claim the superproject would have taken.
func TestAnUnreadableNestedCheckoutLeavesItsPathsWithTheEvent(t *testing.T) {
	root := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "vendor", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "lib", ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitinfo.Find(root)
	if err != nil {
		t.Fatal(err)
	}
	groups := Runner{}.fileGroups(repo, Event{CWD: root, Paths: []string{"vendor/lib/a.go"}})
	if len(groups) != 1 || groups[0].Worktree != root || groups[0].Paths[0] != "vendor/lib/a.go" {
		t.Fatalf("groups %+v", groups)
	}
	// A directory that is in no checkout at all, and outside the event's, takes nothing.
	if got := (Runner{}).fileGroups(repo, Event{CWD: root, Paths: []string{"/"}}); len(got) != 0 {
		t.Fatalf("the filesystem root: %+v", got)
	}
}

// A reconcile names the checkout its status came from.
func TestAReconcileNamesTheCheckoutOfItsStatus(t *testing.T) {
	repo := gitinfo.Repo{Worktree: "/src/app/frontend", WorktreeID: "dev:9", RemoteURL: "https://example.com/frontend.git", RootCommit: "root"}
	got := reconcileRequest("ex_1", 2, "after", map[string]string{"a": " M"}, repo)
	want := hubclient.ReconcileRequest{ExecutionID: "ex_1", Generation: 2, Kind: "after", Status: map[string]string{"a": " M"}, Worktree: "/src/app/frontend", WorktreeID: "dev:9", RemoteURL: "https://example.com/frontend.git", RootCommit: "root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

// fileHub answers a registration with the capabilities it was given, and records grouped and
// plain touches.
type fileHub struct {
	capabilities []string
	files        *[]hubclient.TouchFilesRequest
	plain        *[]hubclient.TouchRequest
}

func (h fileHub) RegisterSession(context.Context, hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	return hubclient.SessionResponse{ExecutionID: "ex_1", Generation: 1, Handle: "ex_1", Capabilities: h.capabilities}, nil
}
func (h fileHub) Touch(_ context.Context, req hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	*h.plain = append(*h.plain, req)
	return hubclient.TouchResponse{Action: "allow"}, nil
}
func (h fileHub) TouchFiles(_ context.Context, req hubclient.TouchFilesRequest) (hubclient.TouchResponse, error) {
	*h.files = append(*h.files, req)
	return hubclient.TouchResponse{Action: "deny", Message: "Iris: src/x.ts is held by codex-host-frontend in repository frontend until 14:32 (auto claim)."}, nil
}
func (fileHub) EndExecution(context.Context, string, int) error { return nil }

// A hub that names file_claims gets the edit's paths grouped by checkout and decides the edit. An
// older hub gets the paths of the event's checkout as before, and an edit with none of those
// needs no call.
func TestAnEditIsTouchedByCheckoutOnlyWhereTheHubTakesIt(t *testing.T) {
	root, _ := withSubmodule(t)
	other := gitRepo(t)
	edit := func(path string) string {
		return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Edit","tool_use_id":"call-1","tool_input":{"file_path":%q,"old_string":"a","new_string":"b"}}`, root, path)
	}
	run := func(hub fileHub, path string) Output {
		return Runner{ClientName: "claude", EventName: "PreToolUse", Stdin: strings.NewReader(edit(path)), Hub: hub, after: never}.Run()
	}
	var files []hubclient.TouchFilesRequest
	var plain []hubclient.TouchRequest
	capable := fileHub{capabilities: []string{"shell_scope", "file_claims"}, files: &files, plain: &plain}
	out := run(capable, filepath.Join(root, "frontend", "src", "x.ts"))
	wantCall := namespaceToolCallID(Event{Client: ClientClaude, ConversationID: "s1", ToolCallID: "call-1", NamespaceVerified: true})
	if len(files) != 1 || len(plain) != 0 || files[0].ToolCallID != wantCall || files[0].Groups[0].Worktree != filepath.Join(root, "frontend") || files[0].Groups[0].Paths[0] != "src/x.ts" {
		t.Fatalf("grouped %+v plain %+v", files, plain)
	}
	if !strings.Contains(string(out.Stdout), `"permissionDecision":"deny"`) || !strings.Contains(string(out.Stdout), "in repository frontend") {
		t.Fatalf("decision %q", out.Stdout)
	}
	older := fileHub{capabilities: []string{"shell_scope"}, files: &files, plain: &plain}
	run(older, filepath.Join(root, "frontend", "src", "x.ts"))
	if len(files) != 1 || len(plain) != 1 || plain[0].Paths[0] != "frontend/src/x.ts" {
		t.Fatalf("older hub: grouped %+v plain %+v", files, plain)
	}
	out = run(older, filepath.Join(other, "src", "main.go"))
	if len(plain) != 1 || string(out.Stdout) != "{}\n" {
		t.Fatalf("another project's file on an older hub: plain %+v, %q", plain, out.Stdout)
	}
}
