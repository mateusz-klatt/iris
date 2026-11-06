package guard

import (
	"bytes"
	"context"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

func TestInstallRemove(t *testing.T) {
	for _, custom := range []string{"", "relative hooks", "absolute"} {
		t.Run(custom, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init")
			git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
			linked := filepath.Join(t.TempDir(), "linked")
			git(t, dir, "worktree", "add", "-b", "linked", linked)
			if custom != "" {
				p := custom
				if custom == "absolute" {
					p = filepath.Join(dir, "absolute hooks")
				}
				git(t, dir, "config", "core.hooksPath", p)
			}
			ctx := context.Background()
			p, err := hookPath(ctx, linked)
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
				if err = Install(ctx, linked); err != nil {
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
			if Remove(ctx, linked) == nil {
				t.Fatal("removed changed hook")
			}
			if err = os.WriteFile(p, wrapper(true), 0755); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err = Remove(ctx, linked); err != nil && bytes.Equal(mustRead(t, p), original) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
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
	if err := Install(ctx, dir); err != nil {
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
	defer s.Close()
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
