package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func preserveConsoleKeyIO(t *testing.T) func() {
	t.Helper()
	create, write, closeFile, link := createConsoleKeyFile, writeConsoleKeyFile, closeConsoleKeyFile, linkConsoleKeyFile
	entropy, syncFile, lstat := readEntropy, syncTokenFile, lstatToken
	restore := func() {
		createConsoleKeyFile, writeConsoleKeyFile, closeConsoleKeyFile, linkConsoleKeyFile = create, write, closeFile, link
		readEntropy, syncTokenFile, lstatToken = entropy, syncFile, lstat
	}
	t.Cleanup(restore)
	return restore
}

// consoleKeyFileClosed reports whether f has been closed. Sync says so on every system; Stat
// on Windows reports an invalid handle instead of os.ErrClosed.
func consoleKeyFileClosed(f *os.File) bool {
	return errors.Is(f.Sync(), os.ErrClosed)
}

func assertNoConsoleKeyTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".console-key-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("console key operation left a private temporary file", err)
	}
}

func TestConsoleKeyPublishesOnlyAfterCompleteProtectedWriteSyncAndClose(t *testing.T) {
	preserveConsoleKeyIO(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "console.key")
	originalCreate, originalWrite, originalLink := createConsoleKeyFile, writeConsoleKeyFile, linkConsoleKeyFile
	var file *os.File
	createConsoleKeyFile = func(dir, pattern string) (*os.File, error) {
		var err error
		file, err = originalCreate(dir, pattern)
		return file, err
	}
	writing, resume := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	writeConsoleKeyFile = func(f *os.File, encoded string) (int, error) {
		n, err := originalWrite(f, encoded[:9])
		if err != nil {
			return n, err
		}
		close(writing)
		<-resume
		m, err := originalWrite(f, encoded[9:])
		return n + m, err
	}
	synced := false
	originalSync := syncTokenFile
	syncTokenFile = func(f *os.File) error {
		if err := originalSync(f); err != nil {
			return err
		}
		synced = true
		return nil
	}
	linkConsoleKeyFile = func(source, target string) error {
		if !consoleKeyFileClosed(file) || !synced {
			return errors.New("publication preceded sync or close")
		}
		raw, err := os.ReadFile(source)
		if err != nil || len(raw) != 44 {
			return errors.New("publication preceded the complete write")
		}
		return originalLink(source, target)
	}
	type result struct {
		key []byte
		err error
	}
	done := make(chan result, 1)
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		key, err := ConsoleKey(path)
		done <- result{key, err}
	}()
	t.Cleanup(func() { unblock.Do(func() { close(resume) }); writer.Wait() })
	select {
	case <-writing:
	case got := <-done:
		t.Fatal("writer stopped before the partial-file observation", got.err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a partial key appeared at the final name", err)
	}
	if key, err := LoadConsoleKey(path); key != nil || err == nil {
		t.Fatal("startup admitted a key while its private write was incomplete")
	}
	info, err := os.Stat(file.Name())
	if err != nil || info.Size() != 9 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("partial bytes were not confined to the private file", err)
	}
	unblock.Do(func() { close(resume) })
	got := <-done
	if got.err != nil || len(got.key) != 32 {
		t.Fatal("complete key was not published", got.err)
	}
	loaded, err := LoadConsoleKey(path)
	if err != nil || !bytes.Equal(loaded, got.key) {
		t.Fatal("published key differs from the complete protected contents", err)
	}
	assertNoConsoleKeyTemps(t, dir)
}

func TestConcurrentConsoleKeyPublishersReturnTheSameCompleteWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.key")
	const publishers = 16
	keys := make([][]byte, publishers)
	errors := make([]error, publishers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := range publishers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			keys[i], errors[i] = ConsoleKey(path)
		}()
	}
	close(start)
	wait.Wait()
	for i := range publishers {
		if errors[i] != nil || len(keys[i]) != 32 || !bytes.Equal(keys[i], keys[0]) {
			t.Fatal("concurrent activation failed or selected a different key", errors[i])
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != base64.RawURLEncoding.EncodeToString(keys[0])+"\n" {
		t.Fatal("winning final key was incomplete or changed", err)
	}
	assertNoConsoleKeyTemps(t, dir)
}

func TestConsoleKeyNeverReplacesAFileThatAppearsAtPublication(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed", true: "valid"}[valid], func(t *testing.T) {
			preserveConsoleKeyIO(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "console.key")
			winner := bytes.Repeat([]byte{0xb7}, 32)
			raw := "existing malformed contents\n"
			if valid {
				raw = base64.RawURLEncoding.EncodeToString(winner) + "\n"
			}
			original := linkConsoleKeyFile
			linkConsoleKeyFile = func(source, target string) error {
				if err := os.WriteFile(target, []byte(raw), 0600); err != nil {
					return err
				}
				return original(source, target)
			}
			key, err := ConsoleKey(path)
			if valid {
				if err != nil || !bytes.Equal(key, winner) {
					t.Fatal("activation did not load the complete winning key", err)
				}
			} else if key != nil || err == nil || err.Error() != "invalid console key file" {
				t.Fatal("activation repaired or admitted a malformed competing file", err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || string(unchanged) != raw {
				t.Fatal("publication overwrote the competing file", err)
			}
			assertNoConsoleKeyTemps(t, dir)
		})
	}
}

func TestConsoleKeyCrashLeftoverDoesNotBlockRetryOrGetDeleted(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, ".console-key-interrupted")
	partial := []byte("incomplete private bytes")
	if err := os.WriteFile(leftover, partial, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "console.key")
	key, err := ConsoleKey(path)
	if err != nil || len(key) != 32 {
		t.Fatal("a private crash leftover blocked activation", err)
	}
	stillPrivate, err := os.ReadFile(leftover)
	if err != nil || !bytes.Equal(stillPrivate, partial) {
		t.Fatal("activation changed an unrelated crash leftover", err)
	}
	loaded, err := LoadConsoleKey(path)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatal("activation after a private crash did not persist a complete key", err)
	}
}

func TestConsoleKeyPublicationFailuresLeaveNoFinalOrTempAndPermitRetry(t *testing.T) {
	for _, failure := range []string{"stat", "create", "protect", "entropy", "write", "sync", "close", "link"} {
		t.Run(failure, func(t *testing.T) {
			restore := preserveConsoleKeyIO(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "console.key")
			private := errors.New("private diagnostic containing secret file contents")
			var file *os.File
			originalCreate, originalWrite, originalClose := createConsoleKeyFile, writeConsoleKeyFile, closeConsoleKeyFile
			createConsoleKeyFile = func(dir, pattern string) (*os.File, error) {
				var err error
				file, err = originalCreate(dir, pattern)
				return file, err
			}
			switch failure {
			case "stat":
				lstatToken = func(string) (os.FileInfo, error) { return nil, private }
			case "create":
				createConsoleKeyFile = func(string, string) (*os.File, error) { return nil, private }
			case "protect":
				createConsoleKeyFile = func(dir, pattern string) (*os.File, error) {
					var err error
					file, err = originalCreate(dir, pattern)
					if err != nil {
						return nil, err
					}
					if err = file.Close(); err != nil {
						return nil, err
					}
					if err = os.Remove(file.Name()); err != nil {
						return nil, err
					}
					return file, nil
				}
				readEntropy = func([]byte) (int, error) {
					t.Error("secret generated before file protection succeeded")
					return 0, private
				}
			case "entropy":
				readEntropy = func([]byte) (int, error) { return 0, private }
			case "write":
				writeConsoleKeyFile = func(f *os.File, value string) (int, error) {
					n, err := originalWrite(f, value[:9])
					if err != nil {
						return n, err
					}
					return n, private
				}
			case "sync":
				syncTokenFile = func(*os.File) error { return private }
			case "close":
				closeConsoleKeyFile = func(f *os.File) error {
					if err := originalClose(f); err != nil {
						return err
					}
					return private
				}
			case "link":
				linkConsoleKeyFile = func(string, string) error { return private }
			}
			key, err := ConsoleKey(path)
			if key != nil || err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), path) {
				t.Fatal("failed publication returned key material or unsafe diagnostics", err)
			}
			if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed publication left a final file", err)
			}
			if file != nil {
				if !consoleKeyFileClosed(file) {
					t.Fatal("failed publication left its file descriptor open")
				}
			}
			assertNoConsoleKeyTemps(t, dir)
			restore()
			key, err = ConsoleKey(path)
			if err != nil || len(key) != 32 {
				t.Fatal("retry after failed publication did not succeed", err)
			}
			loaded, err := LoadConsoleKey(path)
			if err != nil || !bytes.Equal(loaded, key) {
				t.Fatal("successful retry did not persist the returned key", err)
			}
			assertNoConsoleKeyTemps(t, dir)
		})
	}
}

func TestConsoleKeyParentChangingToAFileFailsClosedWithoutReplacingIt(t *testing.T) {
	restore := preserveConsoleKeyIO(t)
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	path := filepath.Join(parent, "console.key")
	original := lstatToken
	lstatToken = func(filename string) (os.FileInfo, error) {
		info, err := original(filename)
		if errors.Is(err, os.ErrNotExist) {
			if writeErr := os.WriteFile(parent, []byte("another file"), 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
		return info, err
	}
	if key, err := ConsoleKey(path); key != nil || err == nil || err.Error() != "console key directory unavailable" {
		t.Fatal("a parent replaced by a file did not close activation safely", err)
	}
	unchanged, err := os.ReadFile(parent)
	if err != nil || string(unchanged) != "another file" {
		t.Fatal("activation replaced the competing parent file", err)
	}
	restore()
	if err = os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if key, err := ConsoleKey(path); err != nil || len(key) != 32 {
		t.Fatal("activation could not retry after the parent was repaired", err)
	}
	assertNoConsoleKeyTemps(t, parent)
}
