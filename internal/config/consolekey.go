package config

import (
	"encoding/base64"
	"errors"
)

// ConsoleKey creates or loads the independent 256-bit console key in a protected
// hub-local file. The caller supplies a separate path from the machine token.
// Startup must keep console authentication closed if this returns an error;
// agent coordination does not depend on this key. Staged account code does not
// call it during ordinary serve startup before the account cutover.
func ConsoleKey(filename string) ([]byte, error) {
	encoded, err := Token(filename)
	if err != nil {
		return nil, err
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != 32 || base64.RawURLEncoding.EncodeToString(key) != encoded {
		return nil, errors.New("invalid console key file")
	}
	return key, nil
}
