package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestLoadConsoleKeyNeverCreatesMissingState(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "console.key"), filepath.Join(dir, "absent-parent", "console.key")} {
		key, err := LoadConsoleKey(path)
		if key != nil || err == nil || err.Error() != "console key file unavailable" {
			t.Fatal("missing console key did not fail closed", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("startup created a key or parent directory", err)
	}
}

func TestLoadConsoleKeyProtectsAndPreservesExplicitActivationKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.key")
	want, err := ConsoleKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		key, err := LoadConsoleKey(path)
		if err != nil || !bytes.Equal(key, want) {
			t.Fatal("loading changed the explicit activation key", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("loading rewrote the activation key", err)
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("loading did not protect the existing key", err)
	}
}

func TestLoadConsoleKeyRejectsMalformedAndNonregularFilesWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.key")
	for _, raw := range []string{
		"private malformed contents",
		base64.RawURLEncoding.EncodeToString(make([]byte, 31)),
		base64.RawURLEncoding.EncodeToString(make([]byte, 33)),
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB",
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)) + "=",
	} {
		if err := os.WriteFile(path, []byte(raw+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		key, err := LoadConsoleKey(path)
		if key != nil || err == nil || err.Error() != "invalid console key file" {
			t.Fatal("malformed console key was accepted or exposed", err)
		}
		unchanged, err := os.ReadFile(path)
		if err != nil || string(unchanged) != raw+"\n" {
			t.Fatal("loading repaired a malformed key", err)
		}
	}
	if key, err := LoadConsoleKey(t.TempDir()); key != nil || err == nil || err.Error() != "console key must be a regular file" {
		t.Fatal("loading accepted a directory", err)
	}
	t.Run("symlink", func(t *testing.T) {
		link := path + ".link"
		if err := os.Symlink(path, link); err != nil {
			t.Skip("symlink creation unavailable", err)
		}
		if key, err := LoadConsoleKey(link); key != nil || err == nil || err.Error() != "console key must be a regular file" {
			t.Fatal("loading followed a symlink", err)
		}
	})
}

func TestLoadConsoleKeyFailuresNeverReadUnprotectedContentsOrExposeDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.key")
	if _, err := ConsoleKey(path); err != nil {
		t.Fatal(err)
	}
	originalLstat, originalRead := lstatToken, readTokenFile
	t.Cleanup(func() { lstatToken, readTokenFile = originalLstat, originalRead })
	privateError := errors.New("private path and secret file contents")
	lstatToken = func(string) (os.FileInfo, error) { return nil, privateError }
	readTokenFile = func(string) ([]byte, error) {
		t.Fatal("contents read before file validation and protection")
		return nil, nil
	}
	key, err := LoadConsoleKey(path)
	if key != nil || err == nil || err.Error() != "console key file unavailable" {
		t.Fatal("stat failure was not safely refused", err)
	}
	lstatToken = originalLstat
	readTokenFile = func(string) ([]byte, error) { return nil, privateError }
	key, err = LoadConsoleKey(path)
	if key != nil || err == nil || err.Error() != "console key file unavailable" {
		t.Fatal("read failure exposed diagnostics", err)
	}

	// A move after the regular-file check exercises the real owner-protection
	// failure on Unix and Windows without bypassing their security handling.
	lstatToken = func(filename string) (os.FileInfo, error) {
		info, err := originalLstat(filename)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(filename, filename+".moved"); err != nil {
			t.Fatal(err)
		}
		return info, nil
	}
	readTokenFile = func(string) ([]byte, error) {
		t.Fatal("contents read after protection failed")
		return nil, nil
	}
	key, err = LoadConsoleKey(path)
	if key != nil || err == nil || err.Error() != "console key file protection failed" || strings.Contains(err.Error(), path) {
		t.Fatal("unprotected key or path-bearing error was returned", err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("loading recreated a key moved during protection", err)
	}
}
