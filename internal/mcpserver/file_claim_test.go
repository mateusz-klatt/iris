package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/binding"
)

// superproject is a checkout with a checked-out submodule at frontend.
func superproject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	modules := filepath.Join(root, ".git", "modules", "frontend")
	for _, dir := range []string{filepath.Join(root, ".git", "refs", "heads"), filepath.Join(modules, "refs", "heads"), filepath.Join(root, "frontend", "src")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(root, ".git", "HEAD"):            "ref: refs/heads/main\n",
		filepath.Join(modules, "HEAD"):                 "ref: refs/heads/main\n",
		filepath.Join(modules, "config"):               "[remote \"origin\"]\n\turl = https://example.com/frontend.git\n",
		filepath.Join(root, "frontend", ".git"):        "gitdir: ../.git/modules/frontend\n",
		filepath.Join(root, "frontend", "src", "x.ts"): "x\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// An explicit claim's path below a checked-out submodule is placed in the submodule, which is sent
// with its place in the session's worktree. Aliases of such a path are placed alike. The
// submodule's directory itself, a path of the session's own checkout, a path that leaves the
// worktree, and any path a hub or binding cannot place stay with the session's repository.
func TestAnExplicitClaimIsPlacedInTheCheckoutThatHoldsIt(t *testing.T) {
	root := superproject(t)
	capable := binding.Bound{Worktree: root, Capabilities: binding.Capabilities("shell_scope\nfile_claims")}
	for _, path := range []string{"frontend/src/x.ts", "./frontend/src/x.ts", "frontend//src/x.ts", "frontend/src/"} {
		checkout, ok, err := fileClaim(capable, path)
		if err != nil || !ok || checkout["prefix"] != "frontend" || checkout["worktree"] != filepath.Join(root, "frontend") || checkout["remote_url"] != "https://example.com/frontend.git" || checkout["common_dir"] != filepath.Join(root, ".git", "modules", "frontend") {
			t.Fatalf("%q: %v %v %v", path, checkout, ok, err)
		}
	}
	for _, c := range []struct {
		b    binding.Bound
		path string
	}{
		{capable, "frontend"},
		{capable, "docs/a.md"},
		{capable, ""},
		{capable, "."},
		{capable, "frontend/.."},
		{capable, "../other/src/x.ts"},
		{capable, "/" + root + "/frontend/src/x.ts"},
		{capable, "frontend\\src\\x.ts"},
		{capable, "C:/frontend/src/x.ts"},
		{binding.Bound{Worktree: root, Capabilities: binding.Capabilities("shell_scope")}, "frontend/src/x.ts"},
		{binding.Bound{Capabilities: capable.Capabilities}, "frontend/src/x.ts"},
	} {
		if checkout, ok, err := fileClaim(c.b, c.path); ok || err != nil {
			t.Errorf("%q with %+v was placed in %v, %v", c.path, c.b, checkout, err)
		}
	}
	// A worktree that is not here is said to be so, rather than claimed in the wrong repository.
	gone := binding.Bound{Worktree: filepath.Join(root, "gone"), Capabilities: capable.Capabilities}
	if _, ok, err := fileClaim(gone, "frontend/src/x.ts"); ok || err == nil || !strings.Contains(err.Error(), "is not a directory on this host") {
		t.Fatalf("an absent worktree: %v %v", ok, err)
	}
}

// The claim tool sends a submodule's path to the grouped route with its checkout, and the rest to
// the plain route as before.
func TestTheClaimToolSendsASubmodulePathWithItsCheckout(t *testing.T) {
	root := superproject(t)
	worktree, _ := json.Marshal(root)
	f := &fakeHub{reply: `{"fence":1,"token":"t","expires_at":null}`, bind: fmt.Sprintf(`{"execution_id":"ex","generation":3,"repository_id":"rp","worktree":%s,"capabilities":["shell_scope","file_claims"]}`, worktree)}
	session := connect(t, f)
	if out, failed := use(t, session, "claim", map[string]any{"kind": "prefix", "path": "frontend/src", "key": "k"}); failed {
		t.Fatal(out)
	}
	got := f.last(t)
	checkout, _ := got.body["checkout"].(map[string]any)
	if got.path != "/v1/claims/files" || got.body["path"] != "frontend/src" || checkout["prefix"] != "frontend" || checkout["worktree"] != filepath.Join(root, "frontend") {
		t.Fatalf("%s %v", got.path, got.body)
	}
	if out, failed := use(t, session, "claim", map[string]any{"kind": "exact", "path": "README.md", "key": "k2"}); failed {
		t.Fatal(out)
	}
	if got := f.last(t); got.path != "/v1/claims" || got.body["checkout"] != nil || got.body["path"] != "README.md" {
		t.Fatalf("%s %v", got.path, got.body)
	}
}

// An absent worktree fails the claim tool with the reason, and nothing reaches the hub.
func TestTheClaimToolRefusesToPlaceFromAnAbsentWorktree(t *testing.T) {
	gone, _ := json.Marshal(filepath.Join(t.TempDir(), "gone"))
	f := &fakeHub{reply: `{}`, bind: fmt.Sprintf(`{"execution_id":"ex","generation":3,"repository_id":"rp","worktree":%s,"capabilities":["file_claims"]}`, gone)}
	out, failed := use(t, connect(t, f), "claim", map[string]any{"kind": "exact", "path": "frontend/src/x.ts", "key": "k"})
	if !failed || !strings.Contains(fmt.Sprint(out), "is not a directory on this host") || len(f.calls) != 0 {
		t.Fatalf("%v %v, calls %v", out, failed, f.calls)
	}
}

// A session outside git takes no claim: the tool says so before it reads the filesystem, and
// nothing reaches the hub (ADR 034).
func TestTheClaimToolRefusesASessionOutsideGit(t *testing.T) {
	f := &fakeHub{reply: `{}`, bind: `{"execution_id":"ex","generation":3,"repository_id":"rp","worktree":"/nowhere/at/all","capabilities":["file_claims"],"outside_git":true}`}
	out, failed := use(t, connect(t, f), "claim", map[string]any{"kind": "exact", "path": "frontend/src/x.ts", "key": "k"})
	if !failed || !strings.Contains(fmt.Sprint(out), "outside any git checkout") || strings.Contains(fmt.Sprint(out), "not a directory") || len(f.calls) != 0 {
		t.Fatalf("%v %v, calls %v", out, failed, f.calls)
	}
}
