package consoleauth

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidDisplayName = errors.New("invalid display name")
)

// LoginKey validates a display username and computes its unique lookup key using
// the Unicode behavior of the pinned x/text dependency: NFC, case fold, NFC.
// It never trims or rewrites the username saved for display.
func LoginKey(username string) (string, error) {
	if username == "" || !utf8.ValidString(username) || utf8.RuneCountInString(username) > 64 || strings.TrimSpace(username) != username {
		return "", ErrInvalidUsername
	}
	for _, c := range username {
		if unicode.IsControl(c) {
			return "", ErrInvalidUsername
		}
	}
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(username))), nil
}

// ValidateDisplayName bounds text saved for display. An empty display name uses
// the username in views; profile text never supplies authority.
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 128 {
		return ErrInvalidDisplayName
	}
	return nil
}
