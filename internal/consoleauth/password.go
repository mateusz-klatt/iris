// Package consoleauth defines the bounded Argon2id credential format for console
// accounts. The hub owns HTTP authentication and account-route availability.
package consoleauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	MaxPasswordBytes = 1024
	MaxHashBytes     = 256
	MaxConcurrentKDF = 2
	argonPrefix      = "$argon2id$v=19$m=65536,t=3,p=1$"
)

var (
	ErrInvalidPassword = errors.New("invalid password")
	ErrInvalidHash     = errors.New("unusable password record")
	ErrBusy            = errors.New("password work capacity reached")
	// One admission limit covers creation and verification. Admission
	// never waits or starts a goroutine, so callers cannot build an unbounded queue.
	kdfSlots = make(chan struct{}, MaxConcurrentKDF)
)

type passwordRecord struct {
	salt [16]byte
	key  [32]byte
}

// ValidateNewPassword checks the policy for newly created or changed passwords.
// Verification applies only the UTF-8 and byte bounds, including for a short
// password retained from an earlier version. Neither path transforms the password.
func ValidateNewPassword(password string) error {
	if !validPasswordInput(password) || utf8.RuneCountInString(password) < 15 {
		return ErrInvalidPassword
	}
	return nil
}

func validPasswordInput(password string) bool {
	return len(password) <= MaxPasswordBytes && utf8.ValidString(password)
}

// ValidateHash validates a record without doing password work or exposing its
// contents. Only the approved Argon2id profile is accepted.
func ValidateHash(encoded string) error {
	_, err := parseHash(encoded)
	return err
}

func parseHash(encoded string) (passwordRecord, error) {
	var record passwordRecord
	if len(encoded) > MaxHashBytes {
		return record, ErrInvalidHash
	}
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 {
		return record, ErrInvalidHash
	}
	encoding := base64.RawStdEncoding.Strict()
	if fields[0] != "" || fields[1] != "argon2id" || fields[2] != "v=19" || fields[3] != "m=65536,t=3,p=1" {
		return record, ErrInvalidHash
	}
	if !decodeFixed(encoding, fields[4], record.salt[:]) || !decodeFixed(encoding, fields[5], record.key[:]) {
		return passwordRecord{}, ErrInvalidHash
	}
	return record, nil
}

func decodeFixed(encoding *base64.Encoding, value string, dst []byte) bool {
	decoded, err := encoding.DecodeString(value)
	if err != nil || len(decoded) != len(dst) || encoding.EncodeToString(decoded) != value {
		return false
	}
	copy(dst, decoded)
	return true
}

func admitKDF() bool {
	select {
	case kdfSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// CreatePassword creates the single approved Argon2id profile with a fresh random
// salt. Call it before acquiring the store transaction lock.
func CreatePassword(password string) (string, error) {
	if err := ValidateNewPassword(password); err != nil {
		return "", err
	}
	if !admitKDF() {
		return "", ErrBusy
	}
	defer func() { <-kdfSlots }()
	var record passwordRecord
	_, _ = rand.Read(record.salt[:]) // crypto/rand.Read never returns an error.
	key := derive(password, record)
	return argonPrefix + base64.RawStdEncoding.EncodeToString(record.salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(key[:]), nil
}

// VerifyPassword verifies the approved Argon2id profile. A mismatch is false without an
// error; malformed records and overlong inputs never invoke a KDF. The caller must
// recheck account state/version transactionally before issuing a session.
func VerifyPassword(password, encoded string) (bool, error) {
	if !validPasswordInput(password) {
		return false, ErrInvalidPassword
	}
	record, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	if !admitKDF() {
		return false, ErrBusy
	}
	defer func() { <-kdfSlots }()
	key := derive(password, record)
	// Both arrays are exactly 32 bytes, including for an incorrect password.
	return subtle.ConstantTimeCompare(key[:], record.key[:]) == 1, nil
}

func derive(password string, record passwordRecord) [32]byte {
	return [32]byte(argon2.IDKey([]byte(password), record.salt[:], 3, 65536, 1, 32))
}
