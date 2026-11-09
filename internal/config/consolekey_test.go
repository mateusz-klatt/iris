package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConsoleKeyIsIndependentProtectedAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.key")
	key, err := ConsoleKey(path)
	if err != nil || len(key) != 32 {
		t.Fatal("console key generation failed", err)
	}
	again, err := ConsoleKey(path)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("console key did not persist", err)
	}
	token, err := Token(filepath.Join(dir, "hub.token"))
	if err != nil || token == base64.RawURLEncoding.EncodeToString(key) {
		t.Fatal("console key reused machine credential", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("console key permissions are not owner-only")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rotated, err := ConsoleKey(path)
	if err != nil || bytes.Equal(key, rotated) {
		t.Fatal("console key rotation did not create a fresh key", err)
	}
}

func TestConsoleKeyRejectsUnusableFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.key")
	for _, value := range []string{"invalid", base64.RawURLEncoding.EncodeToString(make([]byte, 33)), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB"} {
		if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		key, err := ConsoleKey(path)
		if err == nil || key != nil {
			t.Fatal("unusable console key accepted")
		}
	}
	if key, err := ConsoleKey(t.TempDir()); err == nil || key != nil {
		t.Fatal("console key accepted a directory")
	}
}
