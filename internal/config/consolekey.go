package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// These are file I/O boundaries, so tests can exercise interrupted writes,
// failed closes and competing publication while using real protected files.
var (
	createConsoleKeyFile = os.CreateTemp
	writeConsoleKeyFile  = (*os.File).WriteString
	closeConsoleKeyFile  = (*os.File).Close
	linkConsoleKeyFile   = os.Link
)

// ConsoleKey creates or loads the independent 256-bit console key in a protected
// hub-local file. The caller supplies a separate path from the machine token.
// Fresh owner provisioning and explicit owner activation call this creation
// path. Ordinary serve startup uses LoadConsoleKey, so a missing key cannot
// silently activate the console.
// The final name is published only after a protected temporary file is written,
// synced and closed. Publication requires filesystem hard-link support and never
// replaces an existing file. This does not promise directory-entry durability
// beyond the filesystem, or cleanup of private temporary files after a crash.
func ConsoleKey(filename string) ([]byte, error) {
	if _, err := lstatToken(filename); err == nil {
		return LoadConsoleKey(filename)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("console key file unavailable")
	}
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("console key directory unavailable")
	}
	f, err := createConsoleKeyFile(dir, ".console-key-*")
	if err != nil {
		return nil, errors.New("console key temporary file unavailable")
	}
	partial := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(partial)
	}()
	if err = secureToken(partial); err != nil {
		return nil, errors.New("console key file protection failed")
	}
	var key [32]byte
	if _, err = readEntropy(key[:]); err != nil {
		return nil, errors.New("console key entropy unavailable")
	}
	// File.WriteString reports a short write as an error.
	if _, err = writeConsoleKeyFile(f, base64.RawURLEncoding.EncodeToString(key[:])+"\n"); err != nil {
		return nil, errors.New("console key write failed")
	}
	if err = syncTokenFile(f); err != nil {
		return nil, errors.New("console key sync failed")
	}
	if err = closeConsoleKeyFile(f); err != nil {
		return nil, errors.New("console key close failed")
	}
	if err = linkConsoleKeyFile(partial, filename); err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadConsoleKey(filename)
		}
		return nil, errors.New("console key publication failed")
	}
	return key[:], nil
}

// LoadConsoleKey loads an existing independent 256-bit console key and protects
// it with the same owner-only handling as machine credentials. It never creates a
// missing file or parent directory. Every error leaves console authentication
// closed without stopping agent coordination, and exposes no file contents or
// underlying filesystem diagnostics.
func LoadConsoleKey(filename string) ([]byte, error) {
	info, err := lstatToken(filename)
	if err != nil {
		return nil, errors.New("console key file unavailable")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("console key must be a regular file")
	}
	if err = secureToken(filename); err != nil {
		return nil, errors.New("console key file protection failed")
	}
	raw, err := readTokenFile(filename)
	if err != nil {
		return nil, errors.New("console key file unavailable")
	}
	return decodeConsoleKey(strings.TrimSpace(string(raw)))
}

func decodeConsoleKey(encoded string) ([]byte, error) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != 32 || base64.RawURLEncoding.EncodeToString(key) != encoded {
		return nil, errors.New("invalid console key file")
	}
	return key, nil
}
