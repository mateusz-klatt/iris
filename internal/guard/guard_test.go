package guard

import (
	"bytes"
	"context"
	"fmt"
	irisconfig "github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// realPath resolves what git resolves: /var is a link to /private/var on macOS, and the
// temporary directory on Windows runners uses a short name.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestMain keeps the developer's git configuration out of these repositories. A global
// commit.gpgsign otherwise makes every test commit depend on a signing tool.
func TestMain(m *testing.M) {
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	dir, err := os.MkdirTemp("", "iris-guard-test")
	if err != nil {
		panic(err)
	}
	restore := irisconfig.SetPinnedForTest(filepath.Join(dir, "pinned-iris"))
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestInstallRemove(t *testing.T) {
	for _, custom := range []string{"", "relative hooks", "absolute"} {
		t.Run(custom, func(t *testing.T) {
			dir := realPath(t, t.TempDir())
			git(t, dir, "init")
			git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
			linked := filepath.Join(realPath(t, t.TempDir()), "linked")
			git(t, dir, "worktree", "add", "-b", "linked", linked)
			if custom != "" {
				p := custom
				if custom == "absolute" {
					p = filepath.Join(dir, "absolute hooks")
				}
				git(t, dir, "config", "core.hooksPath", p)
			}
			ctx := context.Background()
			nested := filepath.Join(linked, "nested")
			if err := os.MkdirAll(nested, 0755); err != nil {
				t.Fatal(err)
			}
			p, err := hookPath(ctx, nested)
			if err != nil {
				t.Fatal(err)
			}
			if custom == "" && p != filepath.Join(dir, ".git", "hooks", "pre-commit") {
				t.Fatalf("wrong shared hooks path %s", p)
			}
			if custom == "relative hooks" && p != filepath.Join(linked, custom, "pre-commit") {
				t.Fatalf("wrong relative hooks path %s", p)
			}
			if err = os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				t.Fatal(err)
			}
			original := []byte("#!/bin/sh\nexit 7\n")
			if err = os.WriteFile(p, original, 0755); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err = Install(ctx, nested, testIris); err != nil {
					t.Fatal(err)
				}
			}
			backup, err := os.ReadFile(p + ".iris-original")
			if err != nil || !bytes.Equal(original, backup) {
				t.Fatalf("backup %s: %v", backup, err)
			}
			if err = os.WriteFile(p, []byte("user changed hook"), 0755); err != nil {
				t.Fatal(err)
			}
			if Remove(ctx, nested) == nil {
				t.Fatal("removed changed hook")
			}
			if err = os.WriteFile(p, wrapper(testIris, true), 0755); err != nil {
				t.Fatal(err)
			}
			if err = Remove(ctx, nested); err != nil {
				t.Fatal(err)
			}
			if Remove(ctx, nested) == nil {
				t.Fatal("removed the restored original hook")
			}
			if !bytes.Equal(mustRead(t, p), original) {
				t.Fatal("original not restored")
			}
		})
	}
}
func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInstallWithoutExistingHook(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	ctx := context.Background()
	if err := Install(ctx, dir, testIris); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, dir); err != nil {
		t.Fatal(err)
	}
}

type config struct{ address string }

func (c config) HubURL() string       { return c.address }
func (config) Token() (string, error) { return "test-token", nil }

func TestCheckStagedRenameAndHubFailure(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "remote", "add", "origin", "https://example.com/shop.git")
	old := filepath.Join(dir, "held name")
	if err := os.WriteFile(old, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	git(t, dir, "mv", "held name", "renamed")
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	registered, err := s.Call(ctx, "sessions", store.Request{Client: "codex", Host: "holder", SessionID: "holder", Cwd: "/other", Worktree: "/other", RemoteURL: "https://example.com/shop.git"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Call(ctx, "claim", store.Request{ExecutionID: registered["execution_id"].(string), Generation: registered["generation"].(int64), Key: "claim", Path: "held name", Kind: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	u, _ := url.Parse("http://" + server.Listener.Addr().String())
	server.Config.Handler = hub.New(s, "test-token", []string{u.Host})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: config{server.URL}}
	message, err := Check(ctx, dir, client)
	if err != nil || message == "" {
		t.Fatalf("staged old rename path not blocked: %q %v", message, err)
	}
	git(t, dir, "reset", "--hard", "HEAD")
	if err = os.WriteFile(old, []byte("unstaged"), 0600); err != nil {
		t.Fatal(err)
	}
	message, err = Check(ctx, dir, client)
	if err != nil || message != "" {
		t.Fatalf("unstaged path checked: %q %v", message, err)
	}
	git(t, dir, "add", ".")
	server.Close()
	if _, err = Check(ctx, dir, client); err == nil {
		t.Fatal("unreachable hub not reported")
	}
}

func TestCheckLinkedWorktreeCannotClaimTheHoldersSession(t *testing.T) {
	dir := realPath(t, t.TempDir())
	git(t, dir, "init")
	git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	linked := filepath.Join(realPath(t, t.TempDir()), "linked")
	git(t, dir, "worktree", "add", "-b", "linked", linked)
	for _, worktree := range []string{dir, linked} {
		if err := os.WriteFile(filepath.Join(worktree, "held"), []byte("staged"), 0600); err != nil {
			t.Fatal(err)
		}
		git(t, worktree, "add", "held")
	}
	repo, err := gitinfo.Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	holder, err := s.Call(ctx, "sessions", store.Request{Client: "claude", Host: host, SessionID: "holder-session", Cwd: dir, Worktree: repo.Worktree, WorktreeID: repo.WorktreeID, CommonDir: repo.CommonDir, RootCommit: repo.RootCommit, Source: "hook"})
	if err != nil {
		t.Fatal(err)
	}
	proof := store.Request{ExecutionID: holder["execution_id"].(string), Generation: holder["generation"].(int64), Key: "held", Path: "held", Kind: "exact"}
	claim, err := s.Call(ctx, "claim", proof)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	u, _ := url.Parse("http://" + server.Listener.Addr().String())
	server.Config.Handler = hub.New(s, "test-token", []string{u.Host})
	server.Start()
	defer server.Close()
	client := &hubclient.Client{Config: config{server.URL}}
	if message, err := Check(ctx, dir, client); err != nil || message != "" {
		t.Fatalf("same checkout was refused: %q %v", message, err)
	}
	// Inherited or copied session markers cannot turn another checkout into the holder.
	t.Setenv("IRIS_EXECUTION", proof.ExecutionID)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "holder-session")
	message, err := Check(ctx, linked, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"held is held by ",
		"The guard cannot identify the committing session.",
		"If this is your session in another worktree",
		fmt.Sprintf(`call the release tool with {"fence":%v} from that session, then retry the commit.`, claim["fence"]),
		"Otherwise message the holder with the send tool",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal %q does not contain %q", message, want)
		}
	}
	proof.Fence = claim["fence"].(int64)
	if _, err := s.Call(ctx, "release", proof); err != nil {
		t.Fatal(err)
	}
	if message, err := Check(ctx, linked, client); err != nil || message != "" {
		t.Fatalf("retry after the holder released the claim was refused: %q %v", message, err)
	}
}

func TestInstalledHookChainsAndStopsOnDenial(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	bin := t.TempDir()
	source := filepath.Join(bin, "fake.go")
	if err := os.WriteFile(source, []byte("package main\nimport (\"os\";\"strconv\")\nfunc main(){ code,_:=strconv.Atoi(os.Getenv(\"IRIS_GUARD_TEST_EXIT\")); os.Exit(code) }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	name := "iris"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, name), source)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fake iris: %v: %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := hookPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("#!/bin/sh\nprintf called > chain-ran\nexit 7\n")
	if err = os.WriteFile(p, original, 0755); err != nil {
		t.Fatal(err)
	}
	if err = Install(context.Background(), dir, filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
	commit := func() {
		t.Helper()
		cmd := exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "test")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("hook should block commit: %s", out)
		}
	}
	t.Setenv("IRIS_GUARD_TEST_EXIT", "0")
	commit()
	marker := filepath.Join(dir, "chain-ran")
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("original hook did not run: %v", err)
	}
	if err = os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_GUARD_TEST_EXIT", "1")
	commit()
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("original hook ran after Iris denied: %v", err)
	}
}
