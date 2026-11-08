package guard

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

// A merge stages every path the other side changed, though this side never touched them. Those
// paths carry the other side's content, so they are not checked against claims: during a merge
// or a cherry-pick, a staged path counts only when its content differs from HEAD's and from
// every incoming commit's. A path resolved to something new, or changed on both sides, counts.
func TestAMergeChecksOnlyWhatThisSideChanged(t *testing.T) {
	dir := t.TempDir()
	commit := func(msg string) {
		git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", msg)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "remote", "add", "origin", "https://example.com/shop.git")
	for _, name := range []string{"theirs.txt", "both.txt", "picked.txt"} {
		write(name, "base\n")
	}
	git(t, dir, "add", ".")
	commit("base")
	git(t, dir, "switch", "-q", "-c", "other")
	write("theirs.txt", "other side\n")
	write("both.txt", "other side\n")
	git(t, dir, "add", ".")
	commit("other")
	git(t, dir, "switch", "-q", "main")
	write("both.txt", "this side\n")
	git(t, dir, "add", ".")
	commit("this")

	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	holder, err := s.Call(ctx, "sessions", store.Request{Client: "codex", Host: "holder", SessionID: "holder", Cwd: "/other", Worktree: "/other", RemoteURL: "https://example.com/shop.git"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"theirs.txt", "both.txt", "picked.txt"} {
		if _, err = s.Call(ctx, "claim", store.Request{ExecutionID: holder["execution_id"].(string), Generation: holder["generation"].(int64), Key: path, Path: path, Kind: "exact"}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewUnstartedServer(nil)
	u, _ := url.Parse("http://" + server.Listener.Addr().String())
	server.Config.Handler = hub.New(s, "test-token", []string{u.Host})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: config{server.URL}}

	// both.txt conflicts; resolving it to new content is this side's change.
	merge := exec.Command("git", "-c", "user.name=T", "-c", "user.email=t@example.com", "merge", "--no-edit", "other")
	merge.Dir = dir
	_ = merge.Run()
	write("both.txt", "resolved\n")
	git(t, dir, "add", "both.txt")
	message, err := Check(ctx, dir, client)
	if err != nil || !strings.Contains(message, "both.txt") {
		t.Fatalf("a resolved path was not checked: %q %v", message, err)
	}
	// Taking the other side's version is the other side's content.
	write("both.txt", "other side\n")
	git(t, dir, "add", "both.txt")
	if message, err = Check(ctx, dir, client); err != nil || message != "" {
		t.Fatalf("a merge was refused for the other side's content: %q %v", message, err)
	}
	git(t, dir, "merge", "--abort")

	// A cherry-pick that stops on a conflict: picked.txt comes in as the picked commit has it.
	git(t, dir, "switch", "-q", "other")
	write("picked.txt", "picked\n")
	write("both.txt", "other again\n")
	git(t, dir, "add", ".")
	commit("pick me")
	git(t, dir, "switch", "-q", "main")
	pick := exec.Command("git", "-c", "user.name=T", "-c", "user.email=t@example.com", "cherry-pick", "other")
	pick.Dir = dir
	_ = pick.Run()
	write("both.txt", "other again\n")
	git(t, dir, "add", "both.txt")
	if message, err = Check(ctx, dir, client); err != nil || message != "" {
		t.Fatalf("a cherry-pick was refused for the picked commit's content: %q %v", message, err)
	}
	git(t, dir, "cherry-pick", "--abort")

	// Outside a merge, the same staged change is checked as before.
	write("theirs.txt", "edited here\n")
	git(t, dir, "add", "theirs.txt")
	if message, err = Check(ctx, dir, client); err != nil || !strings.Contains(message, "theirs.txt") {
		t.Fatalf("an ordinary commit was not checked: %q %v", message, err)
	}
}

// A merge state Check cannot read, or an incoming commit git does not know, is an error, which the
// CLI reports and allows the commit with, as for an unreachable hub.
func TestAMergeStateThatCannotBeReadIsReported(t *testing.T) {
	dir, _ := repoHook(t)
	seed(t, filepath.Join(dir, "a.txt"), "x")
	git(t, dir, "add", "a.txt")
	head := filepath.Join(dir, ".git", "MERGE_HEAD")
	if err := os.Mkdir(head, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(bg, dir, nil); err == nil {
		t.Fatal("an unreadable MERGE_HEAD was ignored")
	}
	if err := os.Remove(head); err != nil {
		t.Fatal(err)
	}
	seed(t, head, "0000000000000000000000000000000000000001\n")
	if _, err := Check(bg, dir, nil); err == nil {
		t.Fatal("an unknown incoming commit was ignored")
	}
}
