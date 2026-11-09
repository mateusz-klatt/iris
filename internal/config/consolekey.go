package config

import (
	"encoding/base64"
	"errors"
	"strings"
)

// ConsoleKey creates or loads the independent 256-bit console key in a protected
// hub-local file. The caller supplies a separate path from the machine token.
// Only explicit owner activation calls this creation path. Ordinary serve startup
// uses LoadConsoleKey, so a missing key cannot silently activate the console.
func ConsoleKey(filename string) ([]byte, error) {
	encoded, err := Token(filename)
	if err != nil {
		return nil, err
	}
	return decodeConsoleKey(encoded)
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
