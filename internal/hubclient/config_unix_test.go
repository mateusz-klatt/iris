//go:build !windows

package hubclient

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSessionSuccessionRejectsFIFOPipe(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	cfgPath := filepath.Join(dir, "client.json")
	if err := syscall.Mkfifo(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(cfgPath) })
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(string) string { return "" }}
	if e.SuccessionEnabled() {
		t.Fatal("FIFO client.json enabled succession")
	}
}
