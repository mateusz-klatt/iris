package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTLSRequiredOutsideLiteralLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7878", "[::1]:7878"} {
		if err := (Config{Listen: addr}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, addr := range []string{"0.0.0.0:7878", ":7878", "localhost:7878", "192.0.2.1:7878"} {
		c := Config{Listen: addr}
		if c.Validate() == nil {
			t.Fatal(addr)
		}
		c.TLSCert = "cert"
		c.TLSKey = "key"
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if (Config{Listen: "127.0.0.1:7878", TLSCert: "cert"}).Validate() == nil {
		t.Fatal("incomplete TLS accepted")
	}
}
func TestOwnerOnlyStableToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	a, err := Token(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Token(p)
	if err != nil || a != b {
		t.Fatalf("unstable token: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if err = os.WriteFile(p, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Token(p); err == nil {
		t.Fatal("short token accepted")
	}
}
func TestTokenSymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege varies")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do not overwrite"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestDefaultUsesTheConfigDirAndRejectsABadListen(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:7878" || cfg.Database != filepath.Join(dir, "iris", "hub.db") || cfg.TokenFile != filepath.Join(dir, "iris", "hub.token") {
		t.Fatalf("%+v", cfg)
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", "")
	if _, err = Default(); err == nil {
		t.Fatal("missing config dir")
	}
	if err = (Config{Listen: "not-an-address"}).Validate(); err == nil {
		t.Fatal("bad listen")
	}
}

func TestTokenRejectsADirectoryAndABlockedParent(t *testing.T) {
	dir := t.TempDir()
	if _, err := Token(dir); err == nil {
		t.Fatal("directory accepted")
	}
	parent := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(filepath.Join(parent, "hub.token")); err == nil {
		t.Fatal("created a token under a file")
	}
	if err := secureToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing token")
	}
	if err := secureToken(dir); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	if _, err := Token(filepath.Join(locked, "hub.token")); err == nil {
		t.Fatal("created a token in a read-only directory")
	}
}

func TestTokenRemovesAPartialFileWhenCreationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.token")

	origEntropy, origSync, origLstat, origRead := readEntropy, syncTokenFile, lstatToken, readTokenFile
	t.Cleanup(func() {
		readEntropy, syncTokenFile, lstatToken, readTokenFile = origEntropy, origSync, origLstat, origRead
	})

	readEntropy = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if _, err := Token(path); err == nil || !strings.Contains(err.Error(), "no entropy") {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial token left behind", err)
	}
	readEntropy = origEntropy

	syncTokenFile = func(*os.File) error { return errors.New("sync failed") }
	if _, err := Token(path); err == nil || !strings.Contains(err.Error(), "sync failed") {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial token left behind", err)
	}
	syncTokenFile = origSync

	if _, err := Token(path); err != nil {
		t.Fatal(err)
	}
	lstatToken = func(string) (os.FileInfo, error) { return nil, errors.New("gone") }
	if _, err := Token(path); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatal(err)
	}
	lstatToken = origLstat
	readTokenFile = func(string) ([]byte, error) { return nil, errors.New("unreadable") }
	if _, err := Token(path); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatal(err)
	}
}
