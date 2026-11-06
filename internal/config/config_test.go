package config

import (
	"os"
	"path/filepath"
	"runtime"
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
